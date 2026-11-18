package grvt

// Replays sdk/scenarios/runtime.json, the runtime scenarios every SDK shares, against the fake gateway.
// The format is described in sdk/scenarios/README.md.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gravity-technologies/grvt-sdk-go/models"
)

type scenarioReply struct {
	Status    int
	Body      json.RawMessage
	Raw       *string
	Error     json.RawMessage
	Refuse    bool
	Drop      bool
	Hang      bool
	SendFails bool `json:"send_fails"`
	Next      string
	Fail      bool
}

type scenarioGap struct {
	Reason         string
	After, Through *uint64
	Frame          []uint64
}

type runtimeScenario struct {
	Name string
	Call struct {
		Kind, Transport, Stream string
		ClientOrderID           string  `json:"client_order_id"`
		Seconds                 float64 `json:"seconds"`
		WSRequestTimeoutMS      int     `json:"ws_request_timeout_ms"`
		ResyncTimeoutMS         int     `json:"resync_timeout_ms"`
		Instruments             []string
		QueueSize               int `json:"queue_size"`
		CookieMaxAgeS           int `json:"cookie_max_age_s"`
		Take                    int
		Gap                     scenarioGap
	}
	Replies []scenarioReply
	Steps   []struct {
		Push             []uint64
		Feed             *struct{ Value json.RawMessage }
		Drop             bool
		IgnoreSubscribes int    `json:"ignore_subscribes"`
		AckLatest        uint64 `json:"ack_latest"`
		WaitAcks         int    `json:"wait_acks"`
		WaitReconnects   int    `json:"wait_reconnects"`
		WaitDropped      uint64 `json:"wait_dropped"`
	}
	Expect struct {
		Outcome, RPC      string
		Code              int
		Status            json.RawMessage // absent: not checked; null: no HTTP status (WebSocket)
		Retryable, Sent   *bool
		Attempts, Retries *int
		ClientOrderIDs    []string `json:"client_order_ids"`
		SentClientOrderID string   `json:"sent_client_order_id"`
		Items             []any
		Gaps              []scenarioGap
		Others            []struct {
			Items []any
			Gaps  []scenarioGap
		} `json:"other_subscriptions"`
		OverflowCapacity int `json:"overflow_capacity"`
		Subscribes       int
		Unsubscribes     *int
		SubscribeRequest map[string]any `json:"subscribe_request"`
		Logins           int
		ReloginAfterMS   []float64 `json:"relogin_after_ms"`
		SessionRotated   []uint64  `json:"session_rotated"`
		HandshakeLogins  []int     `json:"handshake_logins"`
		Pages            int
		Cursors          []*string
		Cursor           string
		HistoryExhausted []bool `json:"history_exhausted"`
		Continuity       string
	}
	Skip map[string]string
}

var behaviorRef = regexp.MustCompile(`^\$([a-z_]+)(?:\.(\d+))?(?:\*(\d+))?([+-]\d+)?$`)

// resolveBehavior replaces every "$key", "$key.index", "$key*n" and "$key+n" string with the value from
// behavior.json.
func resolveBehavior(t *testing.T, v any, behavior map[string]struct{ Value any }) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = resolveBehavior(t, e, behavior)
		}
	case []any:
		for i, e := range x {
			x[i] = resolveBehavior(t, e, behavior)
		}
	case string:
		m := behaviorRef.FindStringSubmatch(x)
		if m == nil {
			return x
		}
		entry, ok := behavior[m[1]]
		if !ok {
			t.Fatalf("unknown behavior key %q", m[1])
		}
		value := entry.Value
		if m[2] != "" {
			i, _ := strconv.Atoi(m[2])
			value = value.([]any)[i]
		}
		if m[3] != "" {
			n, _ := strconv.Atoi(m[3])
			value = value.(float64) * float64(n)
		}
		if m[4] != "" {
			n, _ := strconv.Atoi(m[4])
			value = value.(float64) + float64(n)
		}
		return value
	}
	return v
}

// loadRuntimeScenarios reads the scenarios; go test ./sdk/generator/... checks them against
// runtime.schema.json.
func loadRuntimeScenarios(t *testing.T) []runtimeScenario {
	var behavior map[string]struct{ Value any }
	var raw any
	for path, dst := range map[string]any{"contract/behavior.json": &behavior, "scenarios/runtime.json": &raw} {
		data, err := ReadShared(path)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(data, dst); err != nil {
			t.Fatal(err)
		}
	}
	data, err := json.Marshal(resolveBehavior(t, raw, behavior))
	if err != nil {
		t.Fatal(err)
	}
	var out []runtimeScenario
	if err = json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// scenarioScript hands out the scripted replies in order; the last one repeats.
type scenarioScript struct {
	mu       sync.Mutex
	replies  []scenarioReply
	current  scenarioReply
	attempts int
}

func (s *scenarioScript) next() scenarioReply {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts++
	if len(s.replies) > 1 {
		s.current, s.replies = s.replies[0], s.replies[1:]
	} else if len(s.replies) == 1 {
		s.current = s.replies[0]
	}
	return s.current
}

func (s *scenarioScript) last() (scenarioReply, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current, s.attempts
}

// scenarioHost names the gateway the scenario's call goes to. The test transport dials it once per attempt
// (no keep-alive), takes the next scripted reply, and fails a "refuse" reply at the dial, before anything is
// written. Login goes to the edge URL, which stays on the fake gateway's own address.
const scenarioHost = "scenario.invalid"

func scenarioTransport(g *fakeGateway, script *scenarioScript) *http.Transport {
	gateway := strings.TrimPrefix(g.srv.URL, "http://")
	var dialer net.Dialer
	return &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if strings.HasPrefix(addr, scenarioHost+":") {
			if script.next().Refuse {
				return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("connection refused")}
			}
			addr = gateway
		}
		return dialer.DialContext(ctx, network, addr)
	}}
}

// The scenarios run in both encodings: the outcome of a call or stream must not depend on it.
func TestSharedRuntimeScenarios(t *testing.T) {
	for _, enc := range encodings {
		t.Run(enc.String(), func(t *testing.T) {
			for _, sc := range loadRuntimeScenarios(t) {
				t.Run(sc.Name, func(t *testing.T) {
					if reason, ok := sc.Skip["go"]; ok {
						t.Skip(reason)
					}
					switch sc.Call.Kind {
					case "stream":
						runStreamScenario(t, sc, enc)
					case "pages", "backfill":
						runPagesScenario(t, sc) // no gateway: the same in both
					default:
						runCallScenario(t, sc, enc)
					}
				})
			}
		})
	}
}

func runCallScenario(t *testing.T, sc runtimeScenario, enc Encoding) {
	g := newFakeGatewayFor(t, enc)
	script := &scenarioScript{replies: sc.Replies}
	path := map[string]string{"read": RPCTickerTicker.Path, "write": RPCOrderCreateOrder.Path,
		"countdown": RPCOrderCancelOnDisconnect.Path}[sc.Call.Kind]
	ws := sc.Call.Transport == "ws"
	g.rest = func(p string, _ []byte) (int, string) {
		if p != path || ws {
			return 200, fixtureJSON(responseFixture(p))
		}
		switch r, _ := script.last(); {
		case r.Drop:
			return -1, ""
		case r.Raw != nil:
			return r.Status, *r.Raw
		default:
			return r.Status, string(r.Body)
		}
	}
	g.rpc = func(string, json.RawMessage) (any, wsAction) {
		switch r := script.next(); {
		case r.Drop:
			return nil, wsDrop
		case r.Hang:
			return nil, wsHang
		default:
			return r.Error, wsError
		}
	}
	signer, err := NewPrivateKeySigner(testKey)
	if err != nil {
		t.Fatal(err)
	}
	c, ev := testClient(t, g, func(c *Config) {
		target := "http://" + scenarioHost + ":" + g.srv.URL[strings.LastIndex(g.srv.URL, ":")+1:]
		c.Env.MarketDataURL, c.Env.TradingURL = target, target
		c.APIKey = "key"
		c.Signer = signer
		c.HTTPClient = &http.Client{Transport: scenarioTransport(g, script)}
		if ws {
			c.OrderTransport = OrderTransportWS
		}
		if sc.Call.WSRequestTimeoutMS != 0 {
			c.WSRequestTimeout = time.Duration(sc.Call.WSRequestTimeoutMS) * time.Millisecond
		}
	})
	c.Runtime().Metadata().PutInstrument("BTC_USDT_Perp", InstrumentMeta{InstrumentHash: "0x030501", BaseDecimals: 9})

	ctx := context.Background()
	switch sc.Call.Kind {
	case "read":
		_, err = c.Ticker(ctx, &models.ApiTickerRequest{Instrument: "BTC_USDT_Perp"})
	case "countdown":
		_, err = RefreshCancelCountdown(ctx, c, 123, uint16(sc.Call.Seconds))
	default:
		req := oneLegOrder()
		if sc.Call.ClientOrderID != "" {
			id, perr := strconv.ParseUint(sc.Call.ClientOrderID, 10, 64)
			if perr != nil {
				t.Fatal(perr)
			}
			req.Order.Metadata = &models.OrderMetadataInput{ClientOrderID: Ptr(id)}
		}
		_, err = c.CreateOrder(ctx, req)
	}

	want := sc.Expect
	checkScenarioError(t, err, sc)
	_, attempts := script.last()
	if ws {
		attempts = g.calls("v1/create_order")
	}
	if want.Attempts != nil && attempts != *want.Attempts {
		t.Errorf("%d attempts, want %d", attempts, *want.Attempts)
	}
	if n := len(ev.of(EventRetry)); want.Retries != nil && n != *want.Retries {
		t.Errorf("%d retry events, want %d", n, *want.Retries)
	}
	if want.SentClientOrderID != "" || want.Outcome == "order_state_unknown" {
		// The order the gateway got: the WS frame's params, or the last REST body (login has none).
		var sent struct {
			Order struct {
				Metadata struct {
					ClientOrderID string `json:"client_order_id"`
				}
			}
		}
		var body []byte
		var merr error
		if ws {
			body, merr = json.Marshal(g.frames("v1/create_order")[0]["params"])
		} else {
			g.mu.Lock()
			body = g.restBodies[len(g.restBodies)-1]
			g.mu.Unlock()
		}
		if merr != nil {
			t.Fatal(merr)
		}
		if uerr := json.Unmarshal(body, &sent); uerr != nil {
			t.Fatal(uerr)
		}
		id := sent.Order.Metadata.ClientOrderID
		var unknown *OrderStateUnknownError
		if errors.As(err, &unknown) && (len(unknown.ClientOrderIDs) != 1 || strconv.FormatUint(unknown.ClientOrderIDs[0], 10) != id) {
			t.Errorf("order state unknown reports client order ids %v, want the sent %s", unknown.ClientOrderIDs, id)
		}
		if want.SentClientOrderID == "" {
			return
		}
		if n, perr := strconv.ParseUint(id, 10, 64); want.SentClientOrderID == "generated" && (perr != nil || n < 1<<63) {
			t.Errorf("generated client order id %q is outside [2^63, 2^64)", id)
		} else if want.SentClientOrderID != "generated" && id != want.SentClientOrderID {
			t.Errorf("sent client order id %q, want %q", id, want.SentClientOrderID)
		}
	}
}

func checkScenarioError(t *testing.T, err error, sc runtimeScenario) {
	t.Helper()
	want := sc.Expect
	var unknown *OrderStateUnknownError
	var decode *DecodeError
	var rateLimit *RateLimitError
	var api *APIError
	var transport *TransportError
	var config *ConfigError
	got, status := "ok", 0
	switch {
	case err == nil:
	case errors.As(err, &unknown):
		got = "order_state_unknown"
	case errors.As(err, &decode):
		got = "decode_error"
	case errors.As(err, &rateLimit):
		got, status = "rate_limited", rateLimit.Status
	case errors.As(err, &api):
		got, status = "api_error", api.Status
	case errors.As(err, &transport):
		got = "transport_error"
	case errors.As(err, &config):
		got = "validation_error"
	default:
		got = fmt.Sprintf("unexpected %T", err)
	}
	if got != want.Outcome {
		t.Fatalf("outcome %s (%v), want %s", got, err, want.Outcome)
	}
	if want.Status != nil && string(want.Status) != map[bool]string{true: "null", false: strconv.Itoa(status)}[status == 0] {
		t.Errorf("status %d, want %s", status, want.Status)
	}
	switch got {
	case "api_error":
		if api.Code != want.Code {
			t.Errorf("code %d, want %d", api.Code, want.Code)
		}
		if want.Retryable != nil && api.Retryable != *want.Retryable {
			t.Errorf("retryable %t, want %t", api.Retryable, *want.Retryable)
		}
	case "transport_error":
		if want.Sent != nil && transport.Sent != *want.Sent {
			t.Errorf("sent %t, want %t", transport.Sent, *want.Sent)
		}
	case "order_state_unknown":
		ids := []string{}
		for _, id := range unknown.ClientOrderIDs {
			ids = append(ids, strconv.FormatUint(id, 10))
		}
		if (want.ClientOrderIDs != nil && !slices.Equal(ids, want.ClientOrderIDs)) || (want.RPC != "" && unknown.RPC != want.RPC) {
			t.Errorf("rpc %s client order ids %v, want %s %v", unknown.RPC, ids, want.RPC, want.ClientOrderIDs)
		}
	}
}

// scenarioSub is one subscription of a stream scenario, whatever its feed type.
type scenarioSub interface {
	take(t *testing.T, n int, capacity int) []any
	dropped() uint64
	close()
}

type typedSub[F any] struct {
	sub *Subscription[F]
	seq func(*F) uint64
}

func (s typedSub[F]) dropped() uint64 { return s.sub.Dropped() }
func (s typedSub[F]) close()          { _ = s.sub.Close() }

// take reads n items in order: each frame's sequence number, "decode_error", "stream_overflow" (whose
// capacity must be the given one) or the error text.
func (s typedSub[F]) take(t *testing.T, n int, capacity int) []any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out := make([]any, 0, n)
	for len(out) < n {
		f, err := s.sub.Next(ctx)
		var decode *DecodeError
		var overflow *StreamOverflowError
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			t.Fatalf("got %d of %d items in time", len(out), n)
		case err == nil:
			out = append(out, float64(s.seq(f)))
		case errors.As(err, &decode):
			out = append(out, "decode_error")
		case errors.As(err, &overflow):
			out = append(out, "stream_overflow")
			if overflow.Capacity != capacity {
				t.Errorf("overflow capacity %d, want %d", overflow.Capacity, capacity)
			}
		default:
			out = append(out, err.Error())
		}
	}
	return out
}

func subscribeScenario(t *testing.T, c *Client, stream, instrument string) (scenarioSub, string) {
	t.Helper()
	ctx := context.Background()
	selector := &models.WSTickerFeedSelectorV1{Instrument: instrument, Rate: Ptr[uint32](500)}
	var sub scenarioSub
	var err error
	switch stream {
	case "v1.ticker.d":
		var s *Subscription[models.PartialWSTickerFeedDataV1]
		s, err = c.TickerDeltaStream(ctx, selector)
		sub = typedSub[models.PartialWSTickerFeedDataV1]{s, func(f *models.PartialWSTickerFeedDataV1) uint64 { return f.SequenceNumber }}
	case "v1.ticker.s":
		var s *Subscription[models.WSTickerFeedDataV1]
		s, err = c.TickerSnapStream(ctx, selector)
		sub = typedSub[models.WSTickerFeedDataV1]{s, func(f *models.WSTickerFeedDataV1) uint64 { return f.SequenceNumber }}
	default:
		var s *Subscription[models.WSOrderFeedDataV1]
		s, err = c.OrderStream(ctx, &models.WSOrderFeedSelectorV1{SubAccountID: 42})
		sub = typedSub[models.WSOrderFeedDataV1]{s, func(f *models.WSOrderFeedDataV1) uint64 { return f.SequenceNumber }}
		return sub, "42"
	}
	if err != nil {
		t.Fatal(err)
	}
	return sub, instrument + "@500"
}

func runStreamScenario(t *testing.T, sc runtimeScenario, enc Encoding) {
	g := newFakeGatewayFor(t, enc)
	g.firstCookieMaxAge = sc.Call.CookieMaxAgeS
	c, ev := testClient(t, g, func(c *Config) {
		c.APIKey = "key"
		if sc.Call.ResyncTimeoutMS != 0 {
			c.ResyncTimeout = time.Duration(sc.Call.ResyncTimeoutMS) * time.Millisecond
		}
		if sc.Call.QueueSize != 0 {
			c.QueueSize = sc.Call.QueueSize
		}
	})
	instruments := sc.Call.Instruments
	if len(instruments) == 0 {
		instruments = []string{"BTC_USDT_Perp"}
	}
	var subs []scenarioSub
	var selectors []string
	for _, instrument := range instruments {
		sub, selector := subscribeScenario(t, c, sc.Call.Stream, instrument)
		subs, selectors = append(subs, sub), append(selectors, selector)
	}
	for _, step := range sc.Steps {
		switch {
		case step.Push != nil && step.Feed != nil:
			g.push(sc.Call.Stream, selectors[0], step.Push[0], step.Push[1], step.Feed.Value)
		case step.Push != nil:
			g.push(sc.Call.Stream, selectors[0], step.Push[0], step.Push[1])
		case step.Drop:
			g.dropConn()
		case step.IgnoreSubscribes != 0:
			g.mu.Lock()
			g.ignoreSubscribes = step.IgnoreSubscribes
			g.mu.Unlock()
		case step.AckLatest != 0:
			g.mu.Lock()
			g.ackLatest = step.AckLatest
			g.mu.Unlock()
		case step.WaitAcks != 0:
			waitUntil(t, "subscribe acks", func() bool { return len(ev.of(EventSubscribe)) >= step.WaitAcks })
		case step.WaitReconnects != 0:
			waitUntil(t, "reconnects", func() bool { return len(ev.of(EventReconnect)) >= step.WaitReconnects })
		case step.WaitDropped != 0:
			waitUntil(t, "dropped items", func() bool { return subs[0].dropped() >= step.WaitDropped })
		}
	}

	want := sc.Expect
	deref := func(p *uint64) any {
		if p == nil {
			return nil
		}
		return *p
	}
	// Each subscription in order: the first's expectations, then other_subscriptions.
	wantItems, wantGaps := [][]any{want.Items}, [][]scenarioGap{want.Gaps}
	for _, o := range want.Others {
		wantItems, wantGaps = append(wantItems, o.Items), append(wantGaps, o.Gaps)
	}
	if len(wantItems) != len(subs) {
		t.Fatalf("%d subscriptions, %d expectations", len(subs), len(wantItems))
	}
	for i, sub := range subs {
		if got := sub.take(t, len(wantItems[i]), want.OverflowCapacity); fmt.Sprint(got) != fmt.Sprint(wantItems[i]) {
			t.Errorf("%s: items %v, want %v", selectors[i], got, wantItems[i])
		}
		// A gap's frame, when the data gives one, is the legacy seq and prev; its expected prev is after.
		var got, wanted []string
		for _, e := range ev.of(EventGap) {
			if e.Selector == selectors[i] {
				got = append(got, fmt.Sprint(e.Gap.Reason, deref(e.Gap.AfterSequence), deref(e.Gap.ThroughSequence)))
				if n := len(got) - 1; n < len(wantGaps[i]) && wantGaps[i][n].Frame != nil {
					got[n] += fmt.Sprint(" frame ", e.Seq, e.Prev, " expected ", e.ExpectedPrev)
				}
			}
		}
		for _, w := range wantGaps[i] {
			wanted = append(wanted, fmt.Sprint(w.Reason, deref(w.After), deref(w.Through)))
			if w.Frame != nil {
				wanted[len(wanted)-1] += fmt.Sprint(" frame ", w.Frame[0], w.Frame[1], " expected ", deref(w.After))
			}
		}
		if !slices.Equal(got, wanted) {
			t.Errorf("%s: gaps %q, want %q", selectors[i], got, wanted)
		}
	}
	subscribes := g.frames("subscribe")
	for _, selector := range selectors {
		n := 0
		for _, f := range subscribes {
			if slices.Contains(f["params"].(map[string]any)["selectors"].([]any), any(selector)) {
				n++
			}
		}
		if n != want.Subscribes {
			t.Errorf("%d subscribes for %s, want %d", n, selector, want.Subscribes)
		}
	}
	if want.SubscribeRequest != nil && !reflect.DeepEqual(subscribes[0]["params"], want.SubscribeRequest) {
		t.Errorf("subscribe request %v, want %v", subscribes[0]["params"], want.SubscribeRequest)
	}
	if want.Unsubscribes != nil {
		subs[0].close()
		waitUntil(t, "unsubscribes", func() bool { return g.calls("unsubscribe") >= *want.Unsubscribes })
		if n := g.calls("unsubscribe"); n != *want.Unsubscribes {
			t.Errorf("%d unsubscribes, want %d", n, *want.Unsubscribes)
		}
	}
	checkSessionExpectations(t, g, ev, sc)
}

func checkSessionExpectations(t *testing.T, g *fakeGateway, ev *events, sc runtimeScenario) {
	want := sc.Expect
	g.mu.Lock()
	logins := slices.Clone(g.loginTimes)
	var handshakes []int
	for _, h := range g.handshakes {
		n, _ := strconv.Atoi(strings.TrimPrefix(h.Get("Cookie"), CookieName+"=sess-"))
		handshakes = append(handshakes, n)
	}
	g.mu.Unlock()
	if want.Logins != 0 && len(logins) != want.Logins {
		t.Errorf("%d logins, want %d", len(logins), want.Logins)
	}
	if r := want.ReloginAfterMS; r != nil {
		if len(logins) < 2 {
			t.Fatalf("%d logins, want a re-login", len(logins))
		}
		if after := float64(logins[1].Sub(logins[0]).Milliseconds()); after < r[0] || after > r[1] {
			t.Errorf("re-login after %vms, want within %v", after, r)
		}
	}
	if want.SessionRotated != nil {
		var generations []uint64
		for _, e := range ev.of(EventSessionRotated) {
			if !e.WebsocketReconnectRequired {
				t.Error("session_rotated without a WebSocket reconnect")
			}
			generations = append(generations, e.Generation)
		}
		if !slices.Equal(generations, want.SessionRotated) {
			t.Errorf("session_rotated generations %v, want %v", generations, want.SessionRotated)
		}
	}
	if want.HandshakeLogins != nil && !slices.Equal(handshakes, want.HandshakeLogins) {
		t.Errorf("handshakes carried the cookies of logins %v, want %v", handshakes, want.HandshakeLogins)
	}
}

func runPagesScenario(t *testing.T, sc runtimeScenario) {
	script := &scenarioScript{replies: sc.Replies}
	fetchErr := errors.New("lookup failed")
	var cursors []*string
	fetch := func(_ context.Context, cursor string) (*models.ApiFillHistoryResponse, error) {
		if cursor == "" {
			cursors = append(cursors, nil)
		} else {
			cursors = append(cursors, &cursor)
		}
		if r := script.next(); !r.Fail {
			return &models.ApiFillHistoryResponse{Next: r.Next}, nil
		}
		return nil, fetchErr
	}
	want := sc.Expect
	ctx := context.Background()
	var err error
	var exhausted []bool
	pages := 0
	type page = BackfillPage[*models.ApiFillHistoryResponse]
	gap := GapRange{Reason: sc.Call.Gap.Reason, AfterSequence: sc.Call.Gap.After, ThroughSequence: sc.Call.Gap.Through}
	seq := BackfillPages(ctx, fetch, gap)
	if sc.Call.Kind == "pages" {
		seq = func(yield func(page, error) bool) {
			for p, err := range Pages(ctx, fetch, "") {
				if !yield(page{Page: p}, err) {
					return
				}
			}
		}
	}
	if len(cursors) != 0 {
		t.Fatal("fetched before the first page was read")
	}
	var p page
	for p, err = range seq {
		if err != nil {
			break
		}
		if pages++; sc.Call.Kind == "backfill" {
			exhausted = append(exhausted, p.HistoryExhausted)
			if p.Continuity() != want.Continuity || !reflect.DeepEqual(p.Gap, gap) {
				t.Errorf("page continuity %s gap %+v, want %s %+v", p.Continuity(), p.Gap, want.Continuity, gap)
			}
			if p.Gap.AfterSequence != nil {
				*p.Gap.AfterSequence = 999 // a page's gap is its own copy: the next page still reports the original
			}
		}
		if pages == sc.Call.Take {
			break
		}
	}
	var cycle *PaginationCycleError
	got := "ok"
	switch {
	case errors.As(err, &cycle):
		got = "pagination_cycle"
		if cycle.Cursor != want.Cursor {
			t.Errorf("cycle at cursor %q, want %q", cycle.Cursor, want.Cursor)
		}
	case errors.Is(err, fetchErr):
		got = "fetch_error"
	case err != nil:
		got = fmt.Sprintf("unexpected %v", err)
	}
	if got != want.Outcome || pages != want.Pages {
		t.Errorf("outcome %s after %d pages, want %s after %d", got, pages, want.Outcome, want.Pages)
	}
	if fmt.Sprint(derefAll(cursors)) != fmt.Sprint(derefAll(want.Cursors)) {
		t.Errorf("fetched cursors %v, want %v", derefAll(cursors), derefAll(want.Cursors))
	}
	if want.HistoryExhausted != nil && !slices.Equal(exhausted, want.HistoryExhausted) {
		t.Errorf("history exhausted %v, want %v", exhausted, want.HistoryExhausted)
	}
}

func derefAll(cursors []*string) []any {
	out := []any{}
	for _, c := range cursors {
		if c == nil {
			out = append(out, nil)
		} else {
			out = append(out, *c)
		}
	}
	return out
}
