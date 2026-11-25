package grvt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gravity-technologies/grvt-sdk-go/models"
)

// The fake gateway's lite mode, written from contract.json alone (not from the SDK's generated codecs):
// it renames what the client sends from lite to full, failing the test on any key that is not a lite
// name of its type (a missed rename would be silently ignored by a real lite gateway), and renames
// what it answers from full to lite. Every fake gateway test runs in both encodings (eachEncoding).

type liteField struct {
	Name, Lite, Type, Kind string
	ArrayDepth             int `json:"array_depth"`
}

type liteRPC struct {
	HTTPPath string `json:"http_path"`
	WSMethod string `json:"ws_method"`
	Version  int
	Route    string
	Request  string
	Response string
}

type liteSchema struct {
	types     map[string][]liteField
	rpcs      []liteRPC
	byLite    map[string]*liteRPC // lite HTTP path -> rpc
	byMethod  map[string]*liteRPC
	feeds     map[string]string // channel -> feed type
	httpError map[string]string // full key -> lite key
	wsPath    string
}

var loadLiteSchema = sync.OnceValues(func() (*liteSchema, error) {
	raw, err := ReadShared("contract/contract.json")
	if err != nil {
		return nil, err
	}
	var c struct {
		Types []struct {
			Name   string
			Fields []liteField
		}
		RPCs    []liteRPC `json:"rpcs"`
		Streams []struct {
			Channel string
			Feed    string `json:"feed_type"`
		}
		Transport struct {
			Lite struct {
				HTTPPathTemplate string            `json:"http_path_template"`
				WSPath           string            `json:"ws_path"`
				HTTPErrorBody    map[string]string `json:"http_error_body"`
			}
		}
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	s := &liteSchema{types: map[string][]liteField{}, rpcs: c.RPCs, byLite: map[string]*liteRPC{},
		byMethod: map[string]*liteRPC{}, feeds: map[string]string{}, httpError: c.Transport.Lite.HTTPErrorBody, wsPath: c.Transport.Lite.WSPath}
	for _, t := range c.Types {
		s.types[t.Name] = t.Fields
	}
	for i := range s.rpcs {
		r := &s.rpcs[i]
		path := strings.NewReplacer("{version}", fmt.Sprint(r.Version), "{route}", r.Route).Replace(c.Transport.Lite.HTTPPathTemplate)
		s.byLite[path] = r
		s.byMethod[r.WSMethod] = r
	}
	for _, st := range c.Streams {
		s.feeds[st.Channel] = st.Feed
	}
	return s, nil
})

func mustLiteSchema(t *testing.T) *liteSchema {
	t.Helper()
	s, err := loadLiteSchema()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// rename renames the keys of a JSON value of type typ, in order: full to lite (toLite) or lite to full.
// Strict fails on a key the type has no name for; otherwise such a key is kept. "any" fields are kept
// as is.
func (s *liteSchema) rename(typ string, raw []byte, toLite, strict bool) ([]byte, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return raw, nil
	}
	fields, ok := s.types[typ]
	if !ok {
		return nil, fmt.Errorf("unknown type %s", typ)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteByte('{')
	for first := true; dec.More(); first = false {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, _ := tok.(string)
		var v json.RawMessage
		if err = dec.Decode(&v); err != nil {
			return nil, err
		}
		var f *liteField
		for i := range fields {
			if from := map[bool]string{true: fields[i].Name, false: fields[i].Lite}[toLite]; from == k {
				f = &fields[i]
			}
		}
		switch {
		case f == nil && strict:
			return nil, fmt.Errorf("%s has no %s field %q", typ, map[bool]string{true: "full", false: "lite"}[toLite], k)
		case f != nil:
			if f.Kind == "struct" {
				if v, err = s.renameStruct(f, v, toLite, strict); err != nil {
					return nil, err
				}
			}
			k = map[bool]string{true: f.Lite, false: f.Name}[toLite]
		}
		if !first {
			out.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		out.Write(kb)
		out.WriteByte(':')
		out.Write(v)
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

func (s *liteSchema) renameStruct(f *liteField, v []byte, toLite, strict bool) ([]byte, error) {
	if f.ArrayDepth == 0 || len(v) == 0 || v[0] != '[' {
		return s.rename(f.Type, v, toLite, strict)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(v, &items); err != nil {
		return nil, err
	}
	for i := range items {
		b, err := s.rename(f.Type, items[i], toLite, strict)
		if err != nil {
			return nil, err
		}
		items[i] = b
	}
	return json.Marshal(items)
}

// requestToFull turns a lite JSON-RPC request into full, strictly: the envelope, then the params as
// the method's params type.
func (s *liteSchema) requestToFull(data []byte) ([]byte, error) {
	env, err := s.rename("JSONRPCRequest", data, false, true)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err = json.Unmarshal(env, &m); err != nil {
		return nil, err
	}
	var method string
	_ = json.Unmarshal(m["method"], &method)
	typ := map[string]string{"subscribe": "WSSubscribeParams", "unsubscribe": "WSUnsubscribeParams"}[method]
	if r := s.byMethod[method]; r != nil {
		typ = r.Request
	}
	if typ == "" { // a method the contract does not have (a test's own): params as sent
		return json.Marshal(m)
	}
	if m["params"], err = s.rename(typ, m["params"], false, true); err != nil {
		return nil, fmt.Errorf("%s params: %w", method, err)
	}
	return json.Marshal(m)
}

// replyToLite turns a full JSON-RPC reply to method into lite.
func (s *liteSchema) replyToLite(method string, data []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return data
	}
	typ := map[string]string{"subscribe": "WSSubscribeResult", "unsubscribe": "WSUnsubscribeResult"}[method]
	if r := s.byMethod[method]; r != nil {
		typ = r.Response
	}
	if res, ok := m["result"]; ok && typ != "" {
		if b, err := s.rename(typ, res, true, false); err == nil {
			m["result"] = b
		}
	}
	if e, ok := m["error"]; ok {
		if b, err := s.rename("Error", e, true, false); err == nil {
			m["error"] = b
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return data
	}
	out, err := s.rename("JSONRPCResponse", b, true, false)
	if err != nil {
		return data
	}
	return out
}

// frameToLite turns a full stream frame into lite.
func (s *liteSchema) frameToLite(data []byte) []byte {
	var head struct{ Stream string }
	if json.Unmarshal(data, &head) != nil || s.feeds[head.Stream] == "" {
		return data
	}
	out, err := s.rename(s.feeds[head.Stream], data, true, false)
	if err != nil {
		return data
	}
	return out
}

// httpErrorToLite renames the keys of a full HTTP error body.
func (s *liteSchema) httpErrorToLite(body string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(body), &m) != nil {
		return body
	}
	out := map[string]json.RawMessage{}
	for k, v := range m {
		if l, ok := s.httpError[k]; ok {
			k = l
		}
		out[k] = v
	}
	b, err := json.Marshal(out)
	if err != nil {
		return body
	}
	return string(b)
}

// -- tests ---------------------------------------------------------------------------------------

func TestLiteConfig(t *testing.T) {
	for _, env := range []Environment{Testnet(), Production()} {
		if !strings.HasSuffix(env.MarketDataWSLite, "/ws/lite") || !strings.HasSuffix(env.TradingWSLite, "/ws/lite") {
			t.Errorf("%s: lite WebSocket URLs %q %q", env.Name, env.MarketDataWSLite, env.TradingWSLite)
		}
	}
	c, err := New(Config{Env: Testnet()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Runtime().Encoding() != EncodingLite || c.Runtime().Socket(GatewayMarketData).url != Testnet().MarketDataWSLite {
		t.Fatal("the default client is not lite on the lite WebSocket URL")
	}
	var cfgErr *ConfigError
	if _, err := New(Config{Env: Testnet(), Encoding: Encoding(7)}); !errors.As(err, &cfgErr) {
		t.Fatalf("unknown encoding: want a ConfigError, got %v", err)
	}
	for _, r := range RPCs {
		if r.Gateway == GatewayEdge {
			if r.LitePath != "" {
				t.Errorf("%s: edge routes have no lite path", r.Method)
			}
			continue
		}
		if r.LitePath != strings.Replace(r.Path, "/full/", "/lite/", 1) {
			t.Errorf("%s: lite path %q for %q", r.Method, r.LitePath, r.Path)
		}
	}
}

// A lite client reads the same values a full client reads, over REST and WebSocket, and sends only
// lite keys (the fake gateway fails the test otherwise). Login stays full.
func TestLiteRESTAndWSMatchFull(t *testing.T) {
	type result struct {
		instrument *models.ApiGetInstrumentResponse
		frame      *models.WSOrderbookLevelsFeedDataV1
		order      *models.ApiCreateOrderResponse
	}
	run := func(t *testing.T, enc Encoding) result {
		g := newFakeGateway(t, enc)
		inst := wireFixture("InstrumentDisplay", map[string]any{"instrument": "BTC_USDT_Perp", "instrument_hash": "0x030501", "base_decimals": 9,
			"quote_decimals": 6, "tick_size": "0.1", "min_size": "0.01", "min_notional": "0", "status": "ACTIVE"})
		g.rest = func(path string, _ []byte) (int, string) {
			if path == RPCInstrumentGetInstrument.Path {
				return 200, fixtureJSON(map[string]any{"result": inst})
			}
			return 200, fixtureJSON(responseFixture(path))
		}
		signer, err := NewPrivateKeySigner(testKey)
		if err != nil {
			t.Fatal(err)
		}
		c, _ := testClient(t, g, func(c *Config) {
			c.Encoding = enc
			c.APIKey = "key"
			c.Signer = signer
			c.OrderTransport = OrderTransportWS
		})
		ctx := context.Background()
		var out result
		if out.instrument, err = c.GetInstrument(ctx, &models.ApiGetInstrumentRequest{Instrument: "BTC_USDT_Perp"}); err != nil {
			t.Fatal(err)
		}
		if out.order, err = c.CreateOrder(ctx, oneLegOrder()); err != nil {
			t.Fatal(err)
		}
		sub, err := c.OrderbookDeltaStream(ctx, &models.WSOrderbookLevelsFeedSelectorV1{Instrument: "BTC_USDT_Perp", Rate: Ptr[uint32](500)})
		if err != nil {
			t.Fatal(err)
		}
		if out.frame, err = sub.Next(ctx); err != nil { // the snapshot
			t.Fatal(err)
		}
		_ = sub.Close()
		waitUntil(t, "unsubscribe", func() bool { return g.calls("unsubscribe") == 1 })
		g.mu.Lock()
		defer g.mu.Unlock()
		if !slices.Contains(g.wirePaths, LoginPath) {
			t.Errorf("no full login in %v", g.wirePaths)
		}
		for _, p := range g.wirePaths {
			if p == LoginPath {
				continue
			}
			if want := map[Encoding]string{EncodingFull: "/full/", EncodingLite: "/lite/"}[enc]; !strings.HasPrefix(p, want) {
				t.Errorf("%s client called %s", enc, p)
			}
		}
		return out
	}
	full, lite := run(t, EncodingFull), run(t, EncodingLite)
	if fmt.Sprintf("%+v", *full.instrument) != fmt.Sprintf("%+v", *lite.instrument) {
		t.Errorf("instrument\nfull %+v\nlite %+v", *full.instrument, *lite.instrument)
	}
	if fmt.Sprintf("%+v", *full.frame) != fmt.Sprintf("%+v", *lite.frame) {
		t.Errorf("book frame\nfull %+v\nlite %+v", *full.frame, *lite.frame)
	}
	if full.frame.Feed.Instrument != "X" {
		t.Errorf("frame not decoded: %+v", full.frame)
	}
	if fmt.Sprintf("%+v", *full.order) != fmt.Sprintf("%+v", *lite.order) {
		t.Errorf("order reply\nfull %+v\nlite %+v", *full.order, *lite.order)
	}
}

// Lite error replies decode to the same APIError as full ones: HTTP bodies (c, m, s) and WebSocket
// error objects (c, m), including a reply with id 0 and an empty method.
func TestLiteErrors(t *testing.T) {
	for _, enc := range encodings {
		t.Run(enc.String(), func(t *testing.T) {
			g := newFakeGateway(t, enc)
			g.rest = func(string, []byte) (int, string) {
				return 400, `{"code":1003,"message":"bad","status":400}`
			}
			g.rpc = func(string, json.RawMessage) (any, wsAction) {
				return map[string]any{"code": 2010, "message": "nope"}, wsError
			}
			c, _ := testClient(t, g, func(c *Config) { c.Encoding = enc })
			_, err := c.Ticker(context.Background(), &models.ApiTickerRequest{Instrument: "X"})
			var api *APIError
			if !errors.As(err, &api) || api.Code != 1003 || api.Status != 400 || !strings.Contains(api.Error(), "bad") {
				t.Fatalf("REST: %v", err)
			}
			_, err = c.Runtime().Socket(GatewayMarketData).request(context.Background(), "v1/ticker", mustLite(t, enc, `{"instrument":"X"}`), false, nil)
			if !errors.As(err, &api) || api.Code != 2010 || !strings.Contains(api.Error(), "nope") {
				t.Fatalf("WS: %v", err)
			}
		})
	}
	// A lite gateway error frame with no id and an empty method still routes nowhere and decodes.
	var f envelope
	if err := decodeEnvelopeLite([]byte(`{"j":"2.0","i":0,"m":"","e":{"c":1003,"m":"bad"}}`), &f); err != nil || f.ID != 0 || f.Error == nil {
		t.Fatalf("%+v %v", f, err)
	}
	if err := decodeEnvelopeLite([]byte(`{"c":1003,"m":"bad","s":400}`), &f); err != nil || f.Code != 1003 || len(f.Stream) != 0 {
		t.Fatalf("an error body's status is not a stream: %+v %v", f, err)
	}
}

func mustLite(t *testing.T, enc Encoding, full string) []byte {
	t.Helper()
	if enc == EncodingFull {
		return []byte(full)
	}
	b, err := mustLiteSchema(t).rename("ApiTickerRequest", []byte(full), true, true)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestLiteFrames(t *testing.T) {
	if got := string(liteJSONRPCFrame("v1/instrument", []byte(`{"i":"X"}`), 7)); got != `{"j":"2.0","m":"v1/instrument","p":{"i":"X"},"i":7}` {
		t.Fatal(got)
	}
	var f envelope
	frame := `{"s":"v1.book.d","s1":"BTC_USDT_Perp@500","sn":"12","ps":"11","f":{"i":"BTC_USDT_Perp"},"fi":1}`
	if err := decodeEnvelopeLite([]byte(frame), &f); err != nil {
		t.Fatal(err)
	}
	if string(f.Stream) != "v1.book.d" || string(f.Selector) != "BTC_USDT_Perp@500" || string(f.Seq) != `"12"` || string(f.Prev) != `"11"` {
		t.Fatalf("%+v", f)
	}
	// Double-wrapped RPC result, as on testnet.
	if err := decodeEnvelopeLite([]byte(`{"j":"2.0","r":{"r":{"i":"X"}},"i":3,"m":"v1/instrument"}`), &f); err != nil || f.ID != 3 || string(f.Result) != `{"r":{"i":"X"}}` {
		t.Fatalf("%+v %v", f, err)
	}
}
