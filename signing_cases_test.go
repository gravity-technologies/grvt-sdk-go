package grvt_test

// Replay every server-produced signing case in sdk/scenarios/signing.json through the GENERATED sign
// functions: byte-equal domain separator, struct hash, digest, typed data, r/s/v, signature object and
// signed request; reject cases must fail before anything is signed.

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	grvt "github.com/gravity-technologies/grvt-sdk-go"
	"github.com/gravity-technologies/grvt-sdk-go/models"
)

type seen struct {
	domainSeparator, structName, structHash, digest string
	typedData                                       map[string]any
}

// recordingSigner captures what the generated code asked to sign, then signs for real.
type recordingSigner struct {
	inner *grvt.PrivateKeySigner
	mu    sync.Mutex
	seen  []seen
}

func (r *recordingSigner) Address() string { return r.inner.Address() }

func (r *recordingSigner) SignTyped(ctx context.Context, d *grvt.Domain, st *grvt.StructType, msg grvt.Value) (grvt.Signature, error) {
	sep := d.Separator()
	h, err := st.HashStruct(msg)
	if err != nil {
		return grvt.Signature{}, err
	}
	dg, _ := st.Digest(d, msg)
	td, err := st.TypedData(d, msg)
	if err != nil {
		return grvt.Signature{}, err
	}
	r.mu.Lock()
	r.seen = append(r.seen, seen{grvt.Hex0x(sep[:]), st.Name(), grvt.Hex0x(h[:]), grvt.Hex0x(dg[:]), td})
	r.mu.Unlock()
	return r.inner.SignTyped(ctx, d, st, msg)
}

type steps struct {
	TypedData       any    `json:"typed_data"`
	DomainSeparator string `json:"domain_separator"`
	StructHash      string `json:"struct_hash"`
	Digest          string `json:"digest"`
	R               string `json:"r"`
	S               string `json:"s"`
	V               uint8  `json:"v"`
	Signature       any    `json:"signature"`
	SignedRequest   any    `json:"signed_request"`
}

type signingCase struct {
	Name   string `json:"name"`
	RPC    string `json:"rpc"`
	Expect string `json:"expect"`
	Signed string `json:"signed_type"`
	Inputs struct {
		WireRequest json.RawMessage `json:"wire_request"`
		EnvChainID  uint64          `json:"env_chain_id"`
		Metadata    struct {
			Instruments []struct {
				Instrument     string `json:"instrument"`
				InstrumentHash string `json:"instrument_hash"`
				BaseDecimals   uint32 `json:"base_decimals"`
				QuoteDecimals  uint32 `json:"quote_decimals"`
			} `json:"instruments"`
			Currencies []struct {
				ID              uint32 `json:"id"`
				Symbol          string `json:"symbol"`
				BalanceDecimals uint32 `json:"balance_decimals"`
			} `json:"currencies"`
			MainAccount string `json:"main_account_id"`
		} `json:"metadata"`
	} `json:"inputs"`
	Steps steps `json:"steps"`
	// Elements and SignedRequest: a per-element binding (bulk orders), one signature per list element.
	Elements []struct {
		Path       string `json:"path"`
		SignedType string `json:"signed_type"`
		Steps      steps  `json:"steps"`
	} `json:"elements"`
	SignedRequest any `json:"signed_request"`
}

func asJSON(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// signWire decodes the wire request into its model, checks it re-encodes to the same JSON, and signs
// it with the generated function.
func signWire[Req any](t *testing.T, rt *grvt.Runtime, wire json.RawMessage, sign func(context.Context, *grvt.Runtime, *Req) error) (any, error) {
	t.Helper()
	req := new(Req)
	if err := json.Unmarshal(wire, req); err != nil {
		t.Fatalf("wire request decodes: %v", err)
	}
	var want any
	_ = json.Unmarshal(wire, &want)
	if got := asJSON(t, req); !reflect.DeepEqual(got, want) {
		t.Fatalf("the typed request does not re-encode to the wire JSON:\n got %v\nwant %v", got, want)
	}
	if err := sign(context.Background(), rt, req); err != nil {
		return nil, err
	}
	return asJSON(t, req), nil
}

func TestEverySigningCase(t *testing.T) {
	data, err := grvt.ReadShared("scenarios/signing.json")
	if err != nil {
		t.Fatal(err)
	}
	var all struct {
		TestKey struct {
			PrivateKey string `json:"private_key"`
			Address    string `json:"address"`
		} `json:"test_key"`
		Cases []signingCase `json:"cases"`
	}
	if err = json.Unmarshal(data, &all); err != nil {
		t.Fatal(err)
	}
	key, err := grvt.NewPrivateKeySigner(all.TestKey.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if key.Address() != all.TestKey.Address {
		t.Fatalf("address %s, want %s", key.Address(), all.TestKey.Address)
	}
	rpcs := map[string]bool{}
	for _, v := range all.Cases {
		t.Run(v.Name, func(t *testing.T) {
			rpcs[v.RPC] = true
			signer := &recordingSigner{inner: key}
			client, err := grvt.New(grvt.Config{Env: grvt.Testnet(), Signer: signer, Clock: grvt.FixedClock(0)})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if client.Runtime().Env().ChainID != v.Inputs.EnvChainID {
				t.Fatalf("chain id %d, want %d", client.Runtime().Env().ChainID, v.Inputs.EnvChainID)
			}
			for _, i := range v.Inputs.Metadata.Instruments {
				client.Runtime().Metadata().PutInstrument(i.Instrument, grvt.InstrumentMeta{
					InstrumentHash: i.InstrumentHash, BaseDecimals: i.BaseDecimals, QuoteDecimals: i.QuoteDecimals})
			}
			if v.Inputs.Metadata.MainAccount != "" {
				client.Runtime().Metadata().PutMainAccount(v.Inputs.Metadata.MainAccount)
			}
			for _, c := range v.Inputs.Metadata.Currencies {
				client.Runtime().Metadata().PutCurrency(c.Symbol, grvt.CurrencyMeta{ID: c.ID, Symbol: c.Symbol, BalanceDecimals: c.BalanceDecimals})
			}
			rt, wire := client.Runtime(), v.Inputs.WireRequest
			var signed any
			switch v.RPC {
			case "order.create_order":
				signed, err = signWire[models.ApiCreateOrderRequestInput](t, rt, wire, grvt.SignOrderCreateOrder)
			case "order.bulk_orders":
				signed, err = signWire[models.ApiBulkOrdersRequestInput](t, rt, wire, grvt.SignOrderBulkOrders)
			case "transfer.transfer":
				signed, err = signWire[models.ApiTransferRequestInput](t, rt, wire, grvt.SignTransferTransfer)
			case "transfer.withdrawal":
				signed, err = signWire[models.ApiWithdrawalRequestInput](t, rt, wire, grvt.SignTransferWithdrawal)
			case "edge.wallet_login":
				signed, err = signWire[models.EdgeWalletLoginRequestInput](t, rt, wire, grvt.SignEdgeWalletLogin)
			case "vault.vault_invest":
				signed, err = signWire[models.ApiVaultInvestV2RequestInput](t, rt, wire, grvt.SignVaultVaultInvest)
			case "vault.vault_redeem":
				signed, err = signWire[models.ApiVaultRedeemV2RequestInput](t, rt, wire, grvt.SignVaultVaultRedeem)
			case "vault.vault_burn_tokens":
				signed, err = signWire[models.ApiVaultBurnTokensRequestInput](t, rt, wire, grvt.SignVaultVaultBurnTokens)
			case "position.add_position_margin":
				signed, err = signWire[models.ApiAddIsolatedPositionMarginRequestInput](t, rt, wire, grvt.SignPositionAddPositionMargin)
			case "position.set_position_config":
				signed, err = signWire[models.ApiSetSubAccountPositionMarginConfigRequestInput](t, rt, wire, grvt.SignPositionSetPositionConfig)
			case "derisk_mm_ratio.set_derisk_mm_ratio":
				signed, err = signWire[models.ApiSetDeriskToMaintenanceMarginRatioRequestInput](t, rt, wire, grvt.SignDeriskMmRatioSetDeriskMmRatio)
			default:
				t.Fatalf("no binding for %s", v.RPC)
			}
			if v.Expect == "reject" {
				var se *grvt.SigningError
				if !errors.As(err, &se) || !strings.Contains(se.Message, "decimal places") {
					t.Fatalf("want a SigningError about decimal places, got %v", err)
				}
				if len(signer.seen) != 0 {
					t.Fatal("refused only after signing")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			check := func(what string, got, want any) {
				t.Helper()
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s:\n got %v\nwant %v", what, got, want)
				}
			}
			if len(v.Elements) > 0 {
				if len(signer.seen) != len(v.Elements) {
					t.Fatalf("signed %d times, want one per element (%d)", len(signer.seen), len(v.Elements))
				}
				for i, el := range v.Elements {
					s := signer.seen[i]
					check(el.Path+" signed type", s.structName, el.SignedType)
					check(el.Path+" domain separator", s.domainSeparator, el.Steps.DomainSeparator)
					check(el.Path+" struct hash", s.structHash, el.Steps.StructHash)
					check(el.Path+" digest", s.digest, el.Steps.Digest)
					check(el.Path+" typed data", asJSON(t, s.typedData), el.Steps.TypedData)
					list, idx, _ := strings.Cut(strings.TrimSuffix(el.Path, "]"), "[")
					n, err := strconv.Atoi(idx)
					if err != nil {
						t.Fatal(err)
					}
					sig := signed.(map[string]any)[list].([]any)[n].(map[string]any)["signature"]
					sm := sig.(map[string]any)
					check(el.Path+" r/s/v", []any{sm["r"], sm["s"], sm["v"]}, []any{el.Steps.R, el.Steps.S, float64(el.Steps.V)})
					check(el.Path+" signature object", sig, el.Steps.Signature)
				}
				check("signed request", signed, v.SignedRequest)
				return
			}
			if len(signer.seen) != 1 {
				t.Fatalf("signed %d times", len(signer.seen))
			}
			s := signer.seen[0]
			check("signed type", s.structName, v.Signed)
			check("domain separator", s.domainSeparator, v.Steps.DomainSeparator)
			check("struct hash", s.structHash, v.Steps.StructHash)
			check("digest", s.digest, v.Steps.Digest)
			check("typed data", asJSON(t, s.typedData), v.Steps.TypedData)
			sig := signed.(map[string]any)["signature"]
			if v.RPC == "order.create_order" {
				sig = signed.(map[string]any)["order"].(map[string]any)["signature"]
			}
			sm := sig.(map[string]any)
			check("r/s/v", []any{sm["r"], sm["s"], sm["v"]}, []any{v.Steps.R, v.Steps.S, float64(v.Steps.V)})
			check("signature object", sig, v.Steps.Signature)
			check("signed request", signed, v.Steps.SignedRequest)
		})
	}
	if len(all.Cases) != 20 {
		t.Errorf("%d signing cases, want 20", len(all.Cases))
	}
	var got []string
	for r := range rpcs {
		got = append(got, r)
	}
	sort.Strings(got)
	want := []string{"derisk_mm_ratio.set_derisk_mm_ratio", "edge.wallet_login", "order.bulk_orders", "order.create_order",
		"position.add_position_margin", "position.set_position_config",
		"transfer.transfer", "transfer.withdrawal", "vault.vault_burn_tokens", "vault.vault_invest", "vault.vault_redeem"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("signed calls with signing cases %v, want %v", got, want)
	}
}

// TestOrderChecks: every SDK must reach the same verdict, nearest values and message on the shared
// order-check cases.
func TestOrderChecks(t *testing.T) {
	data, err := grvt.ReadShared("scenarios/signing.json")
	if err != nil {
		t.Fatal(err)
	}
	var all struct {
		TestKey struct {
			PrivateKey string `json:"private_key"`
		} `json:"test_key"`
		OrderChecks []struct {
			Name        string `json:"name"`
			Instruments []struct {
				Instrument     string `json:"instrument"`
				InstrumentHash string `json:"instrument_hash"`
				BaseDecimals   uint32 `json:"base_decimals"`
				TickSize       string `json:"tick_size"`
				MinSize        string `json:"min_size"`
				MinNotional    string `json:"min_notional"`
			} `json:"instruments"`
			Order     json.RawMessage `json:"order"`
			Expect    string          `json:"expect"`
			Rejection struct {
				Instrument string   `json:"instrument"`
				Leg        int      `json:"leg"`
				Field      string   `json:"field"`
				Check      string   `json:"check"`
				Limit      string   `json:"limit"`
				Value      string   `json:"value"`
				LimitValue string   `json:"limit_value"`
				Nearest    []string `json:"nearest"`
				ServerCode int      `json:"server_code"`
				Message    string   `json:"message"`
			} `json:"rejection"`
		} `json:"order_checks"`
	}
	if err = json.Unmarshal(data, &all); err != nil {
		t.Fatal(err)
	}
	if len(all.OrderChecks) == 0 {
		t.Fatal("no order_checks cases")
	}
	key, err := grvt.NewPrivateKeySigner(all.TestKey.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range all.OrderChecks {
		t.Run(v.Name, func(t *testing.T) {
			signer := &recordingSigner{inner: key}
			client, err := grvt.New(grvt.Config{Env: grvt.Testnet(), Signer: signer, Clock: grvt.FixedClock(0)})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			for _, i := range v.Instruments {
				client.Runtime().Metadata().PutInstrument(i.Instrument, grvt.InstrumentMeta{
					InstrumentHash: i.InstrumentHash, BaseDecimals: i.BaseDecimals,
					Limits: map[string]string{"tick_size": i.TickSize, "min_size": i.MinSize, "min_notional": i.MinNotional},
				})
			}
			req := &models.ApiCreateOrderRequestInput{Order: models.OrderInput{}}
			if err = json.Unmarshal(v.Order, &req.Order); err != nil {
				if v.Expect == "invalid" && len(signer.seen) == 0 {
					return
				}
				t.Fatal(err)
			}
			err = grvt.SignOrderCreateOrder(context.Background(), client.Runtime(), req)
			var oob *grvt.OrderOutOfBoundsError
			switch v.Expect {
			case "pass":
				if err != nil || len(signer.seen) != 1 {
					t.Fatalf("want signed, got %v", err)
				}
				return
			case "invalid":
				var se *grvt.SigningError
				if !errors.As(err, &se) || errors.As(err, &oob) {
					t.Fatalf("want a SigningError, got %v", err)
				}
			case "reject":
				if !errors.As(err, &oob) {
					t.Fatalf("want OrderOutOfBoundsError, got %v", err)
				}
				r := v.Rejection
				want := grvt.OrderOutOfBoundsError{Instrument: r.Instrument, Leg: r.Leg, Field: r.Field, Check: r.Check, Limit: r.Limit,
					Value: r.Value, LimitValue: r.LimitValue, Nearest: r.Nearest, ServerCode: r.ServerCode, Message: r.Message}
				if !reflect.DeepEqual(*oob, want) {
					t.Fatalf("got  %+v\nwant %+v", *oob, want)
				}
			}
			if len(signer.seen) != 0 {
				t.Fatal("refused only after signing")
			}
		})
	}
}
