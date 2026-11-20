package grvt

// REST behaviour against the fake gateway: login, retries, writes, hooks, signing metadata.

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gravity-technologies/grvt-sdk-go/models"
)

func TestLoginSendsCookieAndAccountHeader(t *testing.T) {
	eachEncoding(t, func(t *testing.T, enc Encoding) {
		g := newFakeGateway(t, enc)
		c, _ := testClient(t, g, func(c *Config) { c.APIKey = "key" })
		if _, err := c.AggregatedAccountSummary(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		s := c.Runtime().Session()
		if s.Cookie != "sess-1" || s.AccountID != "acct-1" || s.FundingAccountAddress != "0xabc" {
			t.Fatalf("session %+v", s)
		}
		if !s.ExpiresAt.After(s.RefreshDue()) || s.RefreshDue().Before(s.IssuedAt) {
			t.Fatalf("refresh at 80%% of the lifetime: %+v", s)
		}
		h := c.Runtime().session.headers()
		if h.Get("Cookie") != "gravity=sess-1" || h.Get(AccountHeader) != "acct-1" {
			t.Fatalf("headers %v", h)
		}
		if _, err := c.AggregatedAccountSummary(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		rh := g.restHeads[len(g.restHeads)-1]
		if rh.Get("Cookie") != "gravity=sess-1" || rh.Get(AccountHeader) != "acct-1" {
			t.Fatalf("REST headers %v", rh)
		}
		if len(g.loginTimes) != 1 {
			t.Fatalf("%d logins, want 1 (the session is reused)", len(g.loginTimes))
		}
	})
}

func restTradingClient(t *testing.T, g *fakeGateway, edit func(*Config)) (*Client, *events) {
	signer, err := NewPrivateKeySigner(testKey)
	if err != nil {
		t.Fatal(err)
	}
	return testClient(t, g, func(c *Config) {
		c.APIKey = "key"
		c.Signer = signer
		if edit != nil {
			edit(c)
		}
	})
}

// instrumentServer answers get_instrument (signing metadata) and hands create_order to orders.
func instrumentServer(orders func(body []byte) (int, string)) func(string, []byte) (int, string) {
	return func(path string, body []byte) (int, string) {
		switch path {
		case RPCInstrumentGetInstrument.Path:
			return 200, instrumentFixture()
		case RPCOrderCreateOrder.Path:
			return orders(body)
		}
		return 200, "{}"
	}
}

// A failed session renewal is tried again after the retry delay, not the retry delay plus the minimum
// renewal wait, and is reported as a non-fatal "session: ..." error event.
func TestFailedRenewalIsRetriedAfterTheRetryDelay(t *testing.T) {
	eachEncoding(t, func(t *testing.T, enc Encoding) {
		g := newFakeGateway(t, enc)
		g.firstCookieMaxAge = 1 // renewal due after 0.8 s, so the first re-login waits the 1 s minimum
		g.failLogin = 2
		c, ev := testClient(t, g, func(c *Config) { c.APIKey = "key" })
		c.Runtime().sessionRetryDelay = 300 * time.Millisecond // before the rotation starts at the first login
		if _, err := c.AggregatedAccountSummary(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		waitUntil(t, "a re-login after the failed one", func() bool {
			g.mu.Lock()
			defer g.mu.Unlock()
			return len(g.loginTimes) >= 3
		})
		g.mu.Lock()
		gap := g.loginTimes[2].Sub(g.loginTimes[1])
		g.mu.Unlock()
		if gap < 300*time.Millisecond || gap > 900*time.Millisecond {
			t.Errorf("retried %v after the failed re-login, want the 300ms retry delay", gap)
		}
		errs := ev.of(EventError)
		if len(errs) != 1 || errs[0].Fatal || errs[0].Socket != "" || !strings.HasPrefix(errs[0].Err, "session: ") {
			t.Errorf("error events %+v, want one non-fatal session error", errs)
		}
	})
}

func TestCreateOrderOverRESTLoadsMetadataAndSigns(t *testing.T) {
	eachEncoding(t, func(t *testing.T, enc Encoding) {
		g := newFakeGateway(t, enc)
		var sent atomic.Value
		g.rest = instrumentServer(func(body []byte) (int, string) {
			sent.Store(body)
			return 200, fixtureJSON(responseFixture(RPCOrderCreateOrder.Path))
		})
		c, _ := restTradingClient(t, g, nil)
		for i := 0; i < 2; i++ {
			if _, err := c.CreateOrder(context.Background(), oneLegOrder()); err != nil {
				t.Fatal(err)
			}
		}
		var req models.ApiCreateOrderRequestInput
		if err := json.Unmarshal(sent.Load().([]byte), &req); err != nil {
			t.Fatal(err)
		}
		if req.Order.Signature.R == "" || *req.Order.Metadata.ClientOrderID == 0 {
			t.Fatalf("not signed: %s", sent.Load())
		}
		n := 0
		for _, p := range g.restCalls {
			if p == RPCInstrumentGetInstrument.Path {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("instrument metadata loaded %d times, want once (cached)", n)
		}
	})
}

// capturingSigner records the messages it signs.
type capturingSigner struct {
	Signer
	msgs []Value
}

func (c *capturingSigner) SignTyped(ctx context.Context, d *Domain, st *StructType, msg Value) (Signature, error) {
	c.msgs = append(c.msgs, msg)
	return c.Signer.SignTyped(ctx, d, st, msg)
}

// TestMainAccountIsLoadedOnceAndSigned: a call that signs the session's main account fetches it once with
// funding_account_summary and signs it as accountID.
func TestMainAccountIsLoadedOnceAndSigned(t *testing.T) {
	eachEncoding(t, func(t *testing.T, enc Encoding) {
		const mainAccount = "0x0c3e7b0a6c8a1b2d3e4f5a6b7c8d9e0f1a2b3c4d"
		g := newFakeGateway(t, enc)
		g.rest = func(path string, _ []byte) (int, string) {
			if path == RPCAccountFundingAccountSummary.Path {
				resp := responseFixture(path)
				resp["result"].(map[string]any)["main_account_id"] = mainAccount
				return 200, fixtureJSON(resp)
			}
			return 200, `{"result":{"ack":true}}`
		}
		key, err := NewPrivateKeySigner(testKey)
		if err != nil {
			t.Fatal(err)
		}
		signer := &capturingSigner{Signer: key}
		c, _ := restTradingClient(t, g, func(c *Config) { c.Signer = signer })
		vault := uint64(42)
		for i := 0; i < 2; i++ {
			req := &models.ApiVaultBurnTokensRequestInput{VaultID: vault, Currency: "USDT", NumTokens: models.MustParseDecimal("1.5")}
			if _, err = c.VaultBurnTokens(context.Background(), req); err != nil {
				t.Fatal(err)
			}
		}
		n := 0
		for _, p := range g.restCalls {
			if p == RPCAccountFundingAccountSummary.Path {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("main account loaded %d times, want once (cached)", n)
		}
		want, err := toAddress(mainAccount)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range signer.msgs {
			// VaultBurnLpToken(uint64 vaultID,uint64 numLpTokens,address accountID,...)
			if got := m.(Struct)[2]; got != want {
				t.Fatalf("accountID %v, want %v", got, want)
			}
		}
		if len(signer.msgs) != 2 {
			t.Fatalf("signed %d times, want 2", len(signer.msgs))
		}
	})
}

func TestOverPrecisionIsRejectedBeforeSending(t *testing.T) {
	eachEncoding(t, func(t *testing.T, enc Encoding) {
		g := newFakeGateway(t, enc)
		g.rest = instrumentServer(func([]byte) (int, string) { return 200, "{}" })
		c, _ := restTradingClient(t, g, nil)
		req := oneLegOrder()
		req.Order.Legs[0].Size = models.MustParseDecimal("0.0000000001")
		_, err := c.CreateOrder(context.Background(), req)
		var se *SigningError
		if !errors.As(err, &se) || !strings.Contains(se.Message, "decimal places") {
			t.Fatalf("want SigningError, got %v", err)
		}
		if g.lastPath() == RPCOrderCreateOrder.Path {
			t.Fatal("sent anyway")
		}
	})
}

func TestLimitRejectionRefetchesTheInstrument(t *testing.T) {
	eachEncoding(t, func(t *testing.T, enc Encoding) {
		g := newFakeGateway(t, enc)
		var n atomic.Int32
		g.rest = instrumentServer(func([]byte) (int, string) {
			n.Add(1)
			return 400, `{"code":2064,"message":"Invalid limit price tick","status":400}`
		})
		c, _ := restTradingClient(t, g, nil)
		loads := func() int {
			k := 0
			for _, p := range g.restCalls {
				if p == RPCInstrumentGetInstrument.Path {
					k++
				}
			}
			return k
		}
		for i := 1; i <= 2; i++ {
			_, err := c.CreateOrder(context.Background(), oneLegOrder())
			var api *APIError
			if !errors.As(err, &api) || api.Code != 2064 {
				t.Fatalf("want 2064, got %v", err)
			}
			if loads() != i || n.Load() != int32(i) {
				t.Fatalf("after %d orders: %d metadata loads, %d sends; want one each per order", i, loads(), n.Load())
			}
		}
	})
}

// That an off-tick order is refused before sending is a shared scenario; this checks the error's details.
func TestOffTickErrorNamesTheNearestPrices(t *testing.T) {
	eachEncoding(t, func(t *testing.T, enc Encoding) {
		g := newFakeGateway(t, enc)
		g.rest = instrumentServer(func([]byte) (int, string) { return 200, "{}" })
		c, _ := restTradingClient(t, g, nil)
		c.Runtime().Metadata().PutInstrument("BTC_USDT_Perp", InstrumentMeta{InstrumentHash: "0x030501", BaseDecimals: 9,
			Limits: map[string]string{"tick_size": "0.1", "min_size": "0.001", "min_notional": "100"}})
		req := oneLegOrder()
		req.Order.Legs[0].LimitPrice = Ptr(models.MustParseDecimal("65000.13"))
		_, err := c.CreateOrder(context.Background(), req)
		var oob *OrderOutOfBoundsError
		if !errors.As(err, &oob) || !reflect.DeepEqual(oob.Nearest, []string{"65000.1", "65000.2"}) || oob.ServerCode != 2064 {
			t.Fatalf("want OrderOutOfBoundsError, got %v", err)
		}
	})
}

func TestOffTickOrderIsSignedAndSentWhenOrderChecksAreOff(t *testing.T) {
	eachEncoding(t, func(t *testing.T, enc Encoding) {
		g := newFakeGateway(t, enc)
		var sent atomic.Value
		g.rest = instrumentServer(func(body []byte) (int, string) {
			sent.Store(body)
			return 200, fixtureJSON(responseFixture(RPCOrderCreateOrder.Path))
		})
		c, _ := restTradingClient(t, g, func(c *Config) { c.CheckOrders = Ptr(false) })
		c.Runtime().Metadata().PutInstrument("BTC_USDT_Perp", InstrumentMeta{InstrumentHash: "0x030501", BaseDecimals: 9,
			Limits: map[string]string{"tick_size": "0.1", "min_size": "0.001", "min_notional": "100"}})
		req := oneLegOrder()
		req.Order.Legs[0].LimitPrice = Ptr(models.MustParseDecimal("65000.13"))
		if _, err := c.CreateOrder(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		body, _ := sent.Load().([]byte)
		if !strings.Contains(string(body), `"65000.13"`) || !strings.Contains(string(body), `"r":"0x`) {
			t.Fatalf("want the off-tick order signed and sent, got %s", body)
		}
	})
}

func TestMissingRequiredFieldIsConfigError(t *testing.T) {
	eachEncoding(t, func(t *testing.T, enc Encoding) {
		g := newFakeGateway(t, enc)
		c, _ := restTradingClient(t, g, nil)
		_, err := c.GetInstrument(context.Background(), &models.ApiGetInstrumentRequest{})
		var cfg *ConfigError
		if !errors.As(err, &cfg) || !strings.Contains(cfg.Message, "instrument is required") || len(g.restCalls) != 0 {
			t.Fatalf("err %v, calls %v", err, g.restCalls)
		}
	})
}

func TestBeforeSendRunsBeforeSigning(t *testing.T) {
	eachEncoding(t, func(t *testing.T, enc Encoding) {
		g := newFakeGateway(t, enc)
		var sent atomic.Value
		g.rest = instrumentServer(func(body []byte) (int, string) {
			sent.Store(body)
			return 200, fixtureJSON(responseFixture(RPCOrderCreateOrder.Path))
		})
		var seen []string
		c, _ := restTradingClient(t, g, func(c *Config) {
			c.BeforeSend = func(call *OutgoingCall) {
				seen = append(seen, call.RPC+" "+string(call.Transport))
				if r, ok := call.Params.(*models.ApiCreateOrderRequestInput); ok {
					if r.Order.Signature != nil {
						t.Error("BeforeSend must run before signing")
					}
					r.Order.PostOnly = Ptr(true)
				}
			}
		})
		if _, err := c.CreateOrder(context.Background(), oneLegOrder()); err != nil {
			t.Fatal(err)
		}
		var req models.ApiCreateOrderRequestInput
		_ = json.Unmarshal(sent.Load().([]byte), &req)
		if req.Order.PostOnly == nil || !*req.Order.PostOnly {
			t.Fatalf("hook edit lost: %s", sent.Load())
		}
		// the hook runs for create_order, then for the metadata lookup its signing needs
		if strings.Join(seen, ",") != "v1/create_order rest,v1/instrument rest" {
			t.Fatalf("hook saw %v", seen)
		}
	})
}

// namingSigner records the EIP-712 type of every message it signs.
type namingSigner struct {
	*PrivateKeySigner
	mu    sync.Mutex
	types []string
}

func (s *namingSigner) SignTyped(ctx context.Context, d *Domain, st *StructType, msg Value) (Signature, error) {
	s.mu.Lock()
	s.types = append(s.types, st.Name())
	s.mu.Unlock()
	return s.PrivateKeySigner.SignTyped(ctx, d, st, msg)
}

func TestBulkOrdersSignsEachOrder(t *testing.T) {
	eachEncoding(t, func(t *testing.T, enc Encoding) {
		g := newFakeGateway(t, enc)
		var sent atomic.Value
		g.rest = func(path string, body []byte) (int, string) {
			switch path {
			case RPCInstrumentGetInstrument.Path:
				return 200, instrumentFixture()
			case RPCOrderBulkOrders.Path:
				sent.Store(body)
			}
			return 200, fixtureJSON(responseFixture(path))
		}
		key, err := NewPrivateKeySigner(testKey)
		if err != nil {
			t.Fatal(err)
		}
		signer := &namingSigner{PrivateKeySigner: key}
		c, _ := testClient(t, g, func(c *Config) { c.APIKey, c.Signer = "key", signer })
		plain, builder := oneLegOrder().Order, oneLegOrder().Order
		builder.Builder, builder.BuilderFee = "0x2222222222222222222222222222222222222222", Ptr(models.MustParseDecimal("0.001"))
		builder.Metadata = &models.OrderMetadataInput{ClientOrderID: Ptr[uint64](42)}
		req := &models.ApiBulkOrdersRequestInput{SubAccountID: 123, Orders: []models.OrderInput{plain, builder}}
		if _, err := c.BulkOrders(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if req.Orders[0].Signature != nil {
			t.Fatal("the caller's request was changed")
		}
		var got models.ApiBulkOrdersRequestInput
		if err := json.Unmarshal(sent.Load().([]byte), &got); err != nil {
			t.Fatal(err)
		}
		o0, o1 := got.Orders[0], got.Orders[1]
		if o0.Signature.R == "" || o1.Signature.R == "" || *o0.Signature.Nonce == *o1.Signature.Nonce {
			t.Fatalf("each order needs its own signature and nonce: %s", sent.Load())
		}
		if *o0.Metadata.ClientOrderID == 0 || *o1.Metadata.ClientOrderID != 42 {
			t.Fatalf("client order ids: %s", sent.Load())
		}
		if !reflect.DeepEqual(signer.types, []string{"Order", "OrderWithBuilderFee"}) {
			t.Fatalf("signed types %v", signer.types)
		}

		// A pure bulk cancel has nothing to sign and needs no signer.
		nc, _ := testClient(t, g, func(c *Config) { c.APIKey = "key" })
		cancel := &models.ApiBulkOrdersRequestInput{SubAccountID: 123, ClientOrderIDs: models.Uint64s{42}}
		if _, err := nc.BulkOrders(context.Background(), cancel); err != nil {
			t.Fatal(err)
		}
	})
}

// That a lost bulk answer is unknown for every order is a shared scenario; this checks the error's text names
// every ID and leaves the single-order field empty.
func TestBulkUnknownErrorNamesEveryClientOrderID(t *testing.T) {
	eachEncoding(t, func(t *testing.T, enc Encoding) {
		g := newFakeGateway(t, enc)
		g.rest = func(path string, _ []byte) (int, string) {
			if path == RPCOrderBulkOrders.Path {
				return -1, ""
			}
			return 200, instrumentFixture()
		}
		c, _ := restTradingClient(t, g, nil)
		a, b := oneLegOrder().Order, oneLegOrder().Order
		a.Metadata = &models.OrderMetadataInput{ClientOrderID: Ptr[uint64](7)}
		b.Metadata = &models.OrderMetadataInput{ClientOrderID: Ptr[uint64](8)}
		_, err := c.BulkOrders(context.Background(), &models.ApiBulkOrdersRequestInput{SubAccountID: 123, Orders: []models.OrderInput{a, b}})
		var osu *OrderStateUnknownError
		if !errors.As(err, &osu) || osu.ClientOrderID != 0 || !strings.Contains(osu.Error(), "client_order_ids=[7 8]") {
			t.Fatalf("err %v", err)
		}
	})
}

func TestWriteReplyFailuresKeepIDsAndCause(t *testing.T) {
	eachEncoding(t, func(t *testing.T, enc Encoding) {
		for _, reply := range []struct {
			status int
			body   string
		}{
			{200, "null"}, {200, "not json"}, {200, "{}"}, {400, "proxy failed"},
			{400, `{"code":1003.5,"message":"bad"}`}, {400, `{"code":1003}`},
			{503, `{"code":1006,"message":"busy"}`},
			{500, `{"code":1005,"message":"unknown"}`},
		} {
			t.Run(reply.body, func(t *testing.T) {
				g := newFakeGateway(t, enc)
				g.rest = instrumentServer(func([]byte) (int, string) { return reply.status, reply.body })
				c, _ := restTradingClient(t, g, nil)
				req := oneLegOrder()
				req.Order.Metadata = &models.OrderMetadataInput{ClientOrderID: Ptr[uint64](99)}
				_, err := c.CreateOrder(context.Background(), req)
				var unknown *OrderStateUnknownError
				if !errors.As(err, &unknown) || unknown.ClientOrderID != 99 || unknown.Err == nil {
					t.Fatalf("%+v", err)
				}
			})
		}
	})
}

// The client order IDs are how an unknown outcome is looked up, so BeforeSend may not change them or
// the orders that carry them. Doing so is a ConfigError and nothing is sent.
func TestBeforeSendMustNotChangeOrderIdentity(t *testing.T) {
	eachEncoding(t, func(t *testing.T, enc Encoding) {
		for name, hook := range map[string]func(call *OutgoingCall){
			"change an id": func(call *OutgoingCall) {
				call.Params.(*models.ApiCreateOrderRequestInput).Order.Metadata.ClientOrderID = Ptr[uint64](99)
			},
			"remove an id": func(call *OutgoingCall) {
				call.Params.(*models.ApiCreateOrderRequestInput).Order.Metadata.ClientOrderID = nil
			},
			"change a bulk id": func(call *OutgoingCall) {
				call.Params.(*models.ApiBulkOrdersRequestInput).Orders[1].Metadata.ClientOrderID = Ptr[uint64](99)
			},
			"drop a bulk order": func(call *OutgoingCall) {
				req := call.Params.(*models.ApiBulkOrdersRequestInput)
				req.Orders = req.Orders[:1]
			},
		} {
			t.Run(name, func(t *testing.T) {
				g := newFakeGateway(t, enc)
				sent := 0
				g.rest = func(path string, _ []byte) (int, string) {
					if path == RPCOrderBulkOrders.Path || path == RPCOrderCreateOrder.Path {
						sent++
					}
					return 200, instrumentFixture()
				}
				c, _ := restTradingClient(t, g, func(c *Config) { c.BeforeSend = hook })
				var err error
				if strings.Contains(name, "bulk") {
					_, err = c.BulkOrders(context.Background(), &models.ApiBulkOrdersRequestInput{SubAccountID: 123, Orders: []models.OrderInput{oneLegOrder().Order, oneLegOrder().Order}})
				} else {
					_, err = c.CreateOrder(context.Background(), oneLegOrder())
				}
				var config *ConfigError
				if !errors.As(err, &config) || !strings.Contains(config.Message, "client order IDs") || sent != 0 {
					t.Fatalf("sent %d, err %v", sent, err)
				}
			})
		}
	})
}

func TestWebSocketErrorEnvelopeUsesTheRESTParser(t *testing.T) {
	for _, body := range []string{`false`, `{"code":1003}`, `{"code":[1003],"message":"bad"}`, `{"code":1002,"message":"internal"}`} {
		var f envelope
		if err := decodeEnvelope([]byte(`{"id":1,"error":`+body+`}`), &f); err != nil {
			t.Fatal(err)
		}
		var api *APIError
		err := errorTable.responseError(f.Error.Body, 0, "v1/create_order")
		if !errors.As(err, &api) || !errorTable.uncertain(api) {
			t.Fatalf("%s: %v", body, err)
		}
	}
}
