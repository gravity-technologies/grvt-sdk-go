package grvt

// Hot-path benchmarks: go test -run '^$' -bench 'Decode|Route|Sign|Encode|Lite' -benchmem

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/gravity-technologies/grvt-sdk-go/internal/wirejson"
	"github.com/gravity-technologies/grvt-sdk-go/models"
)

// bookFrame is a v1.book.d frame with n bids and n asks (about 1.2 KB for n = 10).
func bookFrame(n int) []byte {
	var b strings.Builder
	b.WriteString(`{"stream":"v1.book.d","selector":"BTC_USDT_Perp@500-10","sequence_number":"0",` +
		`"feed":{"event_time":"1697788800000000000","instrument":"BTC_USDT_Perp","bids":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"price":"%d.5","size":"%d.123456789","num_orders":%d}`, 65000-i, i+1, i+3)
	}
	b.WriteString(`],"asks":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"price":"%d.5","size":"%d.123456789","num_orders":%d}`, 65001+i, i+1, i+3)
	}
	b.WriteString(`]},"prev_sequence_number":"0"}`)
	return []byte(b.String())
}

// openOrdersReply is a v1/open_orders reply with n orders.
func openOrdersReply(n int) []byte {
	var b strings.Builder
	b.WriteString(`{"result":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"order_id":"0x%028x","sub_account_id":"2398479823477","is_market":false,"time_in_force":"GOOD_TILL_TIME",`+
			`"post_only":false,"reduce_only":false,"legs":[{"instrument":"BTC_USDT_Perp","size":"0.01","limit_price":"65000.5","is_buying_asset":true}],`+
			`"signature":{"signer":"0xc73c0c2538fd9b833d20933ccc88fdaa74fcb0d0","r":"0xb788d96fee91c7cdc35918e0441b756d4000ec1d07d900c73347d9abbc20acc8",`+
			`"s":"0x3d786193125f7c29c958647da64d0e2875ece2c3f845a591bdd7dae8c475e26d","v":28,"expiration":"1697788800000000000","nonce":1234567890,"chain_id":"325"},`+
			`"metadata":{"client_order_id":"%d","create_time":"1697788800000000000"},`+
			`"state":{"status":"OPEN","reject_reason":"UNSPECIFIED","book_size":["0.01"],"traded_size":["0"],"update_time":"1697788800000000000","avg_fill_price":["0"]}}`,
			i+1, 1000+i)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

func BenchmarkDecodeBookFrame(b *testing.B) {
	frame := bookFrame(10)
	b.Run("json.Unmarshal", func(b *testing.B) {
		b.SetBytes(int64(len(frame)))
		b.ReportAllocs()
		for b.Loop() {
			var f models.WSOrderbookLevelsFeedDataV1
			if err := json.Unmarshal(frame, &f); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("sdk", func(b *testing.B) {
		b.SetBytes(int64(len(frame)))
		b.ReportAllocs()
		for b.Loop() {
			var f models.WSOrderbookLevelsFeedDataV1
			if err := decodeJSON(frame, &f); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkRouteDecodeBookFrame is the real stream path: the read loop routes the frame to its
// subscription, then Next decodes it.
func BenchmarkRouteDecodeBookFrame(b *testing.B) {
	c, err := New(Config{Env: Testnet()})
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	sock := c.rt.Socket(GatewayMarketData)
	sel := "BTC_USDT_Perp@500-10"
	sb := &sub{id: 1, spec: &StreamOrderbookOrderbookDelta, selector: sel, capacity: 1 << 30, notify: make(chan struct{}, 1)}
	sb.seq.OnAck(0, 0)
	sock.subs[1] = sb
	sock.byKey[sb.spec.Stream] = map[string]*sub{sel: sb}
	s := &Subscription[models.WSOrderbookLevelsFeedDataV1]{sb: sb, sock: sock}
	frame := bookFrame(10)
	ctx := context.Background()
	b.SetBytes(int64(len(frame)))
	b.ReportAllocs()
	for b.Loop() {
		sock.onFrame(ctx, frame)
		f, err := s.Next(ctx)
		if err != nil || len(f.Feed.Bids) != 10 {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeRESTReply(b *testing.B) {
	raw := openOrdersReply(10)
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	for b.Loop() {
		resp := new(models.ApiOpenOrdersResponse)
		if err := decodeJSON(raw, resp); err != nil || len(resp.Result) != 10 {
			b.Fatal(err)
		}
	}
}

func benchSignClient(b *testing.B) *Client {
	signer, err := NewPrivateKeySigner(testKey)
	if err != nil {
		b.Fatal(err)
	}
	c, err := New(Config{Env: Testnet(), Signer: signer, Clock: FixedClock(0)})
	if err != nil {
		b.Fatal(err)
	}
	c.Runtime().Metadata().PutInstrument("BTC_USDT_Perp", InstrumentMeta{InstrumentHash: "0x030501", BaseDecimals: 9,
		Limits: map[string]string{"tick_size": "0.1", "min_size": "0.001", "min_notional": "100"}})
	return c
}

// BenchmarkSignCreateOrder is order build + metadata + checks + EIP-712 hash + secp256k1 signature.
func BenchmarkSignCreateOrder(b *testing.B) {
	c := benchSignClient(b)
	defer c.Close()
	ctx := context.Background()
	req := oneLegOrder()
	b.ReportAllocs()
	for b.Loop() {
		req.Order.Signature = nil
		if err := SignOrderCreateOrder(ctx, c.Runtime(), req); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEncodeCreateOrder is the request body of a signed create_order.
func BenchmarkEncodeCreateOrder(b *testing.B) {
	c := benchSignClient(b)
	defer c.Close()
	req := oneLegOrder()
	if err := SignOrderCreateOrder(context.Background(), c.Runtime(), req); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := json.Marshal(req); err != nil {
			b.Fatal(err)
		}
	}
}

// allInstrumentsReply is a v1/all_instruments reply with n instruments, shaped like testnet's (about
// 450 bytes each).
func allInstrumentsReply(n int) []byte {
	var b strings.Builder
	b.WriteString(`{"result":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"instrument":"A%03d_USDT_Perp","instrument_hash":"0x03%04x","base":"A%03d","quote":"USDT","kind":"PERPETUAL",`+
			`"venues":["ORDERBOOK","RFQ"],"settlement_period":"PERPETUAL","base_decimals":9,"quote_decimals":6,"tick_size":"0.01",`+
			`"min_size":"0.01","create_time":"1794915071795525183","max_position_size":"20000.0","funding_interval_hours":8,`+
			`"adjusted_funding_rate_cap":"2.0","adjusted_funding_rate_floor":"-2.0","min_notional":"5.0","asset_class":"CRYPTO","status":"ACTIVE"}`,
			i, i, i)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// toLiteBench renames a full text of typ to lite with the test gateway's renamer.
func toLiteBench(b *testing.B, typ string, full []byte) []byte {
	b.Helper()
	s, err := loadLiteSchema()
	if err != nil {
		b.Fatal(err)
	}
	lite, err := s.rename(typ, full, true, true)
	if err != nil {
		b.Fatal(err)
	}
	return lite
}

// BenchmarkLiteDecode compares the full and lite decode of the same data: a 10-level order book frame
// and a 200-instrument all_instruments reply.
func BenchmarkLiteDecode(b *testing.B) {
	book := bookFrame(10)
	instruments := allInstrumentsReply(200)
	cases := []struct {
		name string
		data []byte
		lite bool
		run  func([]byte, bool) error
	}{
		{"book/full", book, false, decodeAs[models.WSOrderbookLevelsFeedDataV1]},
		{"book/lite", toLiteBench(b, "WSOrderbookLevelsFeedDataV1", book), true, decodeAs[models.WSOrderbookLevelsFeedDataV1]},
		{"all_instruments/full", instruments, false, decodeAs[models.ApiGetAllInstrumentsResponse]},
		{"all_instruments/lite", toLiteBench(b, "ApiGetAllInstrumentsResponse", instruments), true, decodeAs[models.ApiGetAllInstrumentsResponse]},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.SetBytes(int64(len(c.data)))
			b.ReportMetric(float64(len(c.data)), "bytes")
			b.ReportAllocs()
			for b.Loop() {
				if err := c.run(c.data, c.lite); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func decodeAs[T any](data []byte, lite bool) error {
	v := new(T)
	if lite {
		return wirejson.UnmarshalLite(data, v)
	}
	return wirejson.Unmarshal(data, v)
}

// BenchmarkLiteRouteDecodeBookFrame is BenchmarkRouteDecodeBookFrame for a lite client.
func BenchmarkLiteRouteDecodeBookFrame(b *testing.B) {
	c, err := New(Config{Env: Testnet(), Encoding: EncodingLite})
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	sock := c.rt.Socket(GatewayMarketData)
	sel := "BTC_USDT_Perp@500-10"
	sb := &sub{id: 1, spec: &StreamOrderbookOrderbookDelta, selector: sel, capacity: 1 << 30, notify: make(chan struct{}, 1)}
	sb.seq.OnAck(0, 0)
	sock.subs[1] = sb
	sock.byKey[sb.spec.Stream] = map[string]*sub{sel: sb}
	s := &Subscription[models.WSOrderbookLevelsFeedDataV1]{sb: sb, sock: sock}
	frame := toLiteBench(b, "WSOrderbookLevelsFeedDataV1", bookFrame(10))
	ctx := context.Background()
	b.SetBytes(int64(len(frame)))
	b.ReportAllocs()
	for b.Loop() {
		sock.onFrame(ctx, frame)
		f, err := s.Next(ctx)
		if err != nil || len(f.Feed.Bids) != 10 {
			b.Fatal(err)
		}
	}
}

// BenchmarkLiteEncodeCreateOrder is the extra cost of a lite request body: the rename after
// json.Marshal.
func BenchmarkLiteEncodeCreateOrder(b *testing.B) {
	c := benchSignClient(b)
	defer c.Close()
	req := oneLegOrder()
	if err := SignOrderCreateOrder(context.Background(), c.Runtime(), req); err != nil {
		b.Fatal(err)
	}
	full, err := json.Marshal(req)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := wirejson.ToLite[models.ApiCreateOrderRequestInput](full); err != nil {
			b.Fatal(err)
		}
	}
}
