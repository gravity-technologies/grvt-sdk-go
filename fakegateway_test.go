package grvt

// An in-process fake gateway for tests: REST (login + RPC handler) and the JSON-RPC WebSocket with
// subscribe/unsubscribe, snapshots and test-controlled stream frames.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type wsAction int

const (
	wsReply wsAction = iota // reply with the result
	wsHang                  // never reply
	wsDrop                  // close the connection without replying
	wsError                 // reply with the result as the JSON-RPC error object
)

type fakeGateway struct {
	t   *testing.T
	srv *httptest.Server
	// lite is set for a lite client: requests are checked and renamed to full, replies renamed to lite.
	lite *liteSchema

	// streamMu keeps a subscribe ack and its snapshot together: the client returns on the ack, so a test's
	// push could otherwise land between them and arrive before the snapshot.
	streamMu sync.Mutex

	mu         sync.Mutex
	conn       *websocket.Conn
	received   []map[string]any // every client WS frame
	handshakes []http.Header
	restCalls  []string // paths, in order (a lite path as its full path)
	wirePaths  []string // paths as called
	restBodies [][]byte
	restHeads  []http.Header
	last       map[string]uint64 // "stream selector" -> last sequence number pushed
	loginTimes []time.Time

	// knobs
	ignoreSubscribes  int
	ackLatest         uint64 // when set, the next subscribe ack uses it as latest_sequence_number
	rejectHandshake   int
	firstCookieMaxAge int // seconds, for the first login's cookie; later ones (and the default) get 3600
	rest              func(path string, body []byte) (int, string)
	rpc               func(method string, params json.RawMessage) (any, wsAction)
}

func newFakeGateway(t *testing.T) *fakeGateway { return newFakeGatewayFor(t, testEncoding()) }

// newFakeGatewayFor serves a client in enc; testClient configures the client to match.
func newFakeGatewayFor(t *testing.T, enc Encoding) *fakeGateway {
	g := &fakeGateway{t: t, last: map[string]uint64{}}
	if enc == EncodingLite {
		g.lite = mustLiteSchema(t)
	}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

// newReachabilityServer answers complete wire models to REST calls (used by the generated api_gen_test.go).
func newReachabilityServer(t *testing.T, enc Encoding) *fakeGateway { return newFakeGatewayFor(t, enc) }

// encoding is the encoding this gateway serves; clients talking to it must use the same one.
func (g *fakeGateway) encoding() Encoding {
	if g.lite != nil {
		return EncodingLite
	}
	return EncodingFull
}

func (g *fakeGateway) env() Environment {
	ws := "ws" + strings.TrimPrefix(g.srv.URL, "http")
	return Environment{Name: "fake", MarketDataURL: g.srv.URL, TradingURL: g.srv.URL, EdgeURL: g.srv.URL,
		MarketDataWS: ws + "/ws/full", TradingWS: ws + "/ws/full", MarketDataWSLite: ws + LiteWSPath, TradingWSLite: ws + LiteWSPath, ChainID: 326}
}

func (g *fakeGateway) lastPath() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.restCalls) == 0 {
		return ""
	}
	return g.restCalls[len(g.restCalls)-1]
}

// lastWirePath is the last REST path as the client called it (lite paths stay lite).
func (g *fakeGateway) lastWirePath() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.wirePaths) == 0 {
		return ""
	}
	return g.wirePaths[len(g.wirePaths)-1]
}

func (g *fakeGateway) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/ws/full", LiteWSPath:
		if (r.URL.Path == LiteWSPath) != (g.lite != nil) {
			g.t.Errorf("fake gateway: %s client connected to %s", map[bool]string{true: "lite", false: "full"}[g.lite != nil], r.URL.Path)
		}
		g.serveWS(w, r)
		return
	case LoginPath:
		g.mu.Lock()
		g.loginTimes = append(g.loginTimes, time.Now())
		n := len(g.loginTimes)
		maxAge := 3600
		if n == 1 && g.firstCookieMaxAge != 0 {
			maxAge = g.firstCookieMaxAge
		}
		g.restCalls = append(g.restCalls, r.URL.Path)
		g.wirePaths = append(g.wirePaths, r.URL.Path)
		g.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: CookieName, Value: fmt.Sprintf("sess-%d", n), MaxAge: maxAge,
			Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		w.Header().Set(AccountHeader, "acct-1")
		_, _ = io.WriteString(w, `{"status":"success","funding_account_address":"0xabc"}`)
		return
	}
	body, _ := io.ReadAll(r.Body)
	path, rpc := r.URL.Path, (*liteRPC)(nil)
	if g.lite != nil { // handlers see the full path and body
		if rpc = g.lite.byLite[path]; rpc == nil {
			g.t.Errorf("fake gateway: lite client called %s", path)
		} else {
			path = rpc.HTTPPath
			full, err := g.lite.rename(rpc.Request, body, false, true)
			if err != nil {
				g.t.Errorf("fake gateway: lite request %s: %v in %s", r.URL.Path, err, body)
			}
			body = full
		}
	}
	g.mu.Lock()
	g.restCalls = append(g.restCalls, path)
	g.wirePaths = append(g.wirePaths, r.URL.Path)
	g.restBodies = append(g.restBodies, body)
	g.restHeads = append(g.restHeads, r.Header.Clone())
	h := g.rest
	g.mu.Unlock()
	status, resp := 200, fixtureJSON(responseFixture(path))
	if h != nil {
		status, resp = h(path, body)
	}
	if rpc != nil && status == 200 {
		if b, err := g.lite.rename(rpc.Response, []byte(resp), true, false); err == nil {
			resp = string(b)
		}
	} else if rpc != nil {
		resp = g.lite.httpErrorToLite(resp)
	}
	if status == -1 { // drop the connection after reading the request
		hj, _ := w.(http.Hijacker)
		c, _, _ := hj.Hijack()
		_ = c.Close()
		return
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, resp)
}

func (g *fakeGateway) serveWS(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.handshakes = append(g.handshakes, r.Header.Clone())
	reject := g.rejectHandshake
	g.mu.Unlock()
	if reject != 0 {
		http.Error(w, "no", reject)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	g.mu.Lock()
	g.conn = c
	g.mu.Unlock()
	ctx := r.Context()
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if g.lite != nil {
			full, ferr := g.lite.requestToFull(data)
			if ferr != nil {
				g.t.Errorf("fake gateway: lite frame %s: %v", data, ferr)
			}
			data = full
		}
		var m map[string]any
		_ = json.Unmarshal(data, &m)
		g.mu.Lock()
		g.received = append(g.received, m)
		g.mu.Unlock()
		id := m["id"]
		params, err := json.Marshal(m["params"])
		if err != nil {
			g.t.Errorf("fake gateway: encoding params: %v", err)
			return
		}
		switch m["method"] {
		case "subscribe":
			g.onSubscribe(ctx, c, id, m["params"].(map[string]any))
		case "unsubscribe":
			g.reply(ctx, c, "unsubscribe", map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
		default:
			g.mu.Lock()
			h := g.rpc
			g.mu.Unlock()
			result, action := any(responseFixture(m["method"].(string))), wsReply
			if h != nil {
				result, action = h(m["method"].(string), params)
			}
			switch action {
			case wsReply:
				g.reply(ctx, c, m["method"].(string), map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
			case wsError:
				g.reply(ctx, c, m["method"].(string), map[string]any{"jsonrpc": "2.0", "id": id, "error": result, "method": m["method"]})
			case wsDrop:
				_ = c.CloseNow()
				return
			}
		}
	}
}

func (g *fakeGateway) onSubscribe(ctx context.Context, c *websocket.Conn, id any, params map[string]any) {
	stream := params["stream"].(string)
	sel := params["selectors"].([]any)[0].(string)
	g.mu.Lock()
	if g.ignoreSubscribes > 0 {
		g.ignoreSubscribes--
		g.mu.Unlock()
		return
	}
	key := stream + " " + sel
	latest, ok := g.last[key]
	if !ok {
		latest = 100
	}
	if g.ackLatest != 0 {
		latest, g.ackLatest = g.ackLatest, 0
	}
	g.last[key] = latest
	g.mu.Unlock()
	g.streamMu.Lock()
	defer g.streamMu.Unlock()
	g.reply(ctx, c, "subscribe", map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
		"stream": stream, "subs": []string{sel}, "unsubs": []string{}, "first_sequence_number": []string{"0"}, "num_snapshots": []int{1},
		"latest_sequence_number": []string{strconv.FormatUint(latest, 10)}}})
	g.send(ctx, c, frame(stream, sel, 0, 0))
}

func frame(stream, sel string, seq, prev uint64) map[string]any {
	return map[string]any{"stream": stream, "selector": sel, "sequence_number": strconv.FormatUint(seq, 10),
		"prev_sequence_number": strconv.FormatUint(prev, 10), "feed": feedFixture(stream)}
}

// send writes a stream frame.
func (g *fakeGateway) send(ctx context.Context, c *websocket.Conn, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		g.t.Errorf("fake gateway: encoding frame: %v", err)
		return
	}
	if g.lite != nil {
		b = g.lite.frameToLite(b)
	}
	_ = c.Write(ctx, websocket.MessageText, b)
}

// reply writes the reply to a request for method.
func (g *fakeGateway) reply(ctx context.Context, c *websocket.Conn, method string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		g.t.Errorf("fake gateway: encoding reply: %v", err)
		return
	}
	if g.lite != nil {
		b = g.lite.replyToLite(method, b)
	}
	_ = c.Write(ctx, websocket.MessageText, b)
}

// push sends the next delta (seq = last+1, prev = last) unless seq/prev are given (non-zero).
func (g *fakeGateway) push(stream, sel string, seq, prev uint64, feed ...any) {
	g.mu.Lock()
	key := stream + " " + sel
	if seq == 0 {
		seq, prev = g.last[key]+1, g.last[key]
	}
	g.last[key] = seq
	c := g.conn
	g.mu.Unlock()
	f := frame(stream, sel, seq, prev)
	if len(feed) > 0 { // a feed in place of the stream's fixture
		f["feed"] = feed[0]
	}
	g.streamMu.Lock()
	defer g.streamMu.Unlock()
	g.send(context.Background(), c, f)
}

// dropConn closes the server side of the current connection. Like push, it waits for an in-flight
// subscribe ack to be followed by its snapshot.
func (g *fakeGateway) dropConn() {
	g.mu.Lock()
	c := g.conn
	g.mu.Unlock()
	g.streamMu.Lock()
	defer g.streamMu.Unlock()
	_ = c.CloseNow()
}

func (g *fakeGateway) calls(method string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, m := range g.received {
		if m["method"] == method {
			n++
		}
	}
	return n
}

func (g *fakeGateway) frames(method string) []map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []map[string]any
	for _, m := range g.received {
		if m["method"] == method {
			out = append(out, m)
		}
	}
	return out
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// events records every non-frame event.
type events struct {
	mu   sync.Mutex
	list []Event
}

func (e *events) add(ev Event) {
	if ev.Kind == EventFrame {
		return
	}
	e.mu.Lock()
	e.list = append(e.list, ev)
	e.mu.Unlock()
}

func (e *events) of(k EventKind) []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []Event
	for _, ev := range e.list {
		if ev.Kind == k {
			out = append(out, ev)
		}
	}
	return out
}
