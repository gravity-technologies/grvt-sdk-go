package grvt

// WS manager behaviour against the in-process fake gateway.

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gravity-technologies/grvt-sdk-go/models"
)

const testKey = "0xbfe25ff07bd38cb3fbc28a2a296d41aabd68a78c929d257935906489fc9c6d89"

func testClient(t *testing.T, g *fakeGateway, edit func(*Config)) (*Client, *events) {
	t.Helper()
	ev := &events{}
	cfg := Config{Env: g.env(), OnEvent: ev.add, Encoding: g.encoding()}
	if edit != nil {
		edit(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, ev
}

func tickerDelta(t *testing.T, c *Client, inst string) *Subscription[models.PartialWSTickerFeedDataV1] {
	t.Helper()
	sub, err := c.TickerDeltaStream(context.Background(), &models.WSTickerFeedSelectorV1{Instrument: inst, Rate: Ptr[uint32](500)})
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

func TestIteratorEndsOnClose(t *testing.T) {
	g := newFakeGateway(t)
	c, _ := testClient(t, g, nil)
	sub := tickerDelta(t, c, "X")
	g.push("v1.ticker.d", "X@500", 0, 0)
	var got []uint64
	for f, err := range sub.All() {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, f.SequenceNumber)
		if len(got) == 2 {
			_ = sub.Close()
		}
	}
	if !reflect.DeepEqual(got, []uint64{0, 101}) {
		t.Fatal(got)
	}
}

func TestHandshakeRejectedIsFatal(t *testing.T) {
	g := newFakeGateway(t)
	g.rejectHandshake = 403
	c, ev := testClient(t, g, nil)
	_, err := c.TickerDeltaStream(context.Background(), &models.WSTickerFeedSelectorV1{Instrument: "X"})
	var api *APIError
	if !errors.As(err, &api) || api.Status != 403 {
		t.Fatalf("want a 403 APIError, got %v", err)
	}
	if e := ev.of(EventError); len(e) != 1 || !e[0].Fatal {
		t.Fatalf("events %+v", ev.list)
	}
}

// -- order entry over WS ---------------------------------------------------------------------------

func wsTradingClient(t *testing.T, g *fakeGateway, edit func(*Config)) (*Client, *events) {
	signer, err := NewPrivateKeySigner(testKey)
	if err != nil {
		t.Fatal(err)
	}
	c, ev := testClient(t, g, func(c *Config) {
		c.APIKey = "key"
		c.Signer = signer
		c.OrderTransport = OrderTransportWS
		if edit != nil {
			edit(c)
		}
	})
	c.Runtime().Metadata().PutInstrument("BTC_USDT_Perp", InstrumentMeta{InstrumentHash: "0x030501", BaseDecimals: 9})
	return c, ev
}

func oneLegOrder() *models.ApiCreateOrderRequestInput {
	return &models.ApiCreateOrderRequestInput{Order: models.OrderInput{
		SubAccountID: Ptr[uint64](123),
		TimeInForce:  models.TimeInForceGoodTillTime,
		Legs: []models.OrderLeg{{Instrument: "BTC_USDT_Perp", Size: models.MustParseDecimal("0.01"), LimitPrice: Ptr(models.MustParseDecimal("65000.1")),
			IsBuyingAsset: true}},
	}}
}

func TestCreateOrderOverWS(t *testing.T) {
	g := newFakeGateway(t)
	g.rpc = func(_ string, params json.RawMessage) (any, wsAction) {
		var p map[string]any
		_ = json.Unmarshal(params, &p)
		return map[string]any{"result": p["order"]}, wsReply
	}
	c, _ := wsTradingClient(t, g, nil)
	req := oneLegOrder()
	resp, err := c.CreateOrder(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if req.Order.Signature != nil || req.Order.Metadata != nil {
		t.Fatal("the caller's request must not change")
	}
	o := resp.Result
	if o.Metadata.ClientOrderID == 0 {
		t.Fatal("client_order_id not filled in")
	}
	if s := o.Signature; s.R == "" || s.Signer == "" || s.ChainID != 326 {
		t.Fatalf("signature %+v", s)
	}
	h := g.handshakes[len(g.handshakes)-1]
	if h.Get("Cookie") != "gravity=sess-1" || h.Get(AccountHeader) != "acct-1" {
		t.Fatalf("handshake headers %v", h)
	}
	if g.calls("v1/create_order") != 1 {
		t.Fatal("create_order not sent over WS")
	}
}

func TestWSWriteTimeoutIsOrderStateUnknown(t *testing.T) {
	g := newFakeGateway(t)
	g.rpc = func(string, json.RawMessage) (any, wsAction) { return nil, wsHang }
	c, _ := wsTradingClient(t, g, func(c *Config) { c.WSRequestTimeout = 200 * time.Millisecond })
	req := oneLegOrder()
	req.Order.Metadata = &models.OrderMetadataInput{ClientOrderID: Ptr[uint64](42)}
	_, err := c.CreateOrder(context.Background(), req)
	var osu *OrderStateUnknownError
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("missing timeout cause: %v", err)
	}
	if !errors.As(err, &osu) || osu.ClientOrderID != 42 {
		t.Fatalf("want OrderStateUnknownError for client order 42, got %v", err)
	}
}
