package grvt

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type deadlineTransport func(*http.Request) (*http.Response, error)

func (f deadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCallDeadlineStopsLateHookBeforeDispatch(t *testing.T) {
	var calls atomic.Int32
	rt, err := NewRuntime(Config{Env: Testnet(), CallTimeout: time.Millisecond,
		BeforeSend: func(*OutgoingCall) { time.Sleep(5 * time.Millisecond) },
		HTTPClient: &http.Client{Transport: deadlineTransport(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return nil, errors.New("unexpected dispatch")
		})},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	_, err = Call[struct{}, struct{}](context.Background(), rt, &RPCSpec{Method: "write", Write: true}, nil, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 0 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestCallDeadlineAfterDispatchKeepsCauseAndIDs(t *testing.T) {
	type request struct {
		Order struct {
			Metadata struct {
				ID uint64 `json:"client_order_id,string"`
			} `json:"metadata"`
		} `json:"order"`
	}
	rt, err := NewRuntime(Config{Env: Testnet(), CallTimeout: 10 * time.Millisecond,
		HTTPClient: &http.Client{Transport: deadlineTransport(func(r *http.Request) (*http.Response, error) {
			httptrace.ContextClientTrace(r.Context()).WroteHeaders()
			<-r.Context().Done()
			return nil, r.Context().Err()
		})},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	info := &RequestInfo[request]{ClientOrderID: func(r *request, _ ClientOrderIDProvider) uint64 { r.Order.Metadata.ID = 7; return 7 }}
	_, err = Call[request, struct{}](context.Background(), rt, &RPCSpec{Method: "write", Write: true, Gateway: GatewayTrading, Path: "/write"}, nil, info, nil)
	var unknown *OrderStateUnknownError
	if !errors.As(err, &unknown) || !errors.Is(err, context.DeadlineExceeded) || unknown.ClientOrderID != 7 {
		t.Fatalf("%+v", err)
	}
}

func TestMetadataWaiterCancellationDoesNotCancelSharedLoad(t *testing.T) {
	var cache MetadataCache
	started, finish := make(chan struct{}), make(chan struct{})
	var loads atomic.Int32
	loader := func(ctx context.Context, _ string) (InstrumentMeta, error) {
		loads.Add(1)
		close(started)
		select {
		case <-finish:
			return InstrumentMeta{BaseDecimals: 9}, nil
		case <-ctx.Done():
			return InstrumentMeta{}, ctx.Err()
		}
	}
	first, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() { _, e := cache.Instrument(first, "ETH", loader); firstDone <- e }()
	<-started
	secondDone := make(chan error, 1)
	go func() {
		v, e := cache.Instrument(context.Background(), "ETH", loader)
		if e == nil && v.BaseDecimals != 9 {
			e = errors.New("wrong metadata")
		}
		secondDone <- e
	}()
	cancel()
	if !errors.Is(<-firstDone, context.Canceled) {
		t.Fatal("first waiter was not cancelled")
	}
	close(finish)
	if e := <-secondDone; e != nil || loads.Load() != 1 {
		t.Fatalf("err=%v loads=%d", e, loads.Load())
	}
}

func TestSessionWaiterCancellationDoesNotCancelSharedLogin(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s := &session{apiKey: "key", loginURL: "https://example.test/login", log: func(string, ...any) {}, http: &http.Client{Transport: deadlineTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		close(started)
		select {
		case <-finish:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Set-Cookie": []string{"gravity=test; Max-Age=3600"}}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})}}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { first <- s.ensure(ctx) }()
	<-started
	second := make(chan error, 1)
	go func() { second <- s.ensure(context.Background()) }()
	cancel()
	if !errors.Is(<-first, context.Canceled) {
		t.Fatal("first waiter was not cancelled")
	}
	close(finish)
	if e := <-second; e != nil || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", e, calls.Load())
	}
}

func TestReadRetriesShareOneCallDeadline(t *testing.T) {
	var calls atomic.Int32
	rt, err := NewRuntime(Config{Env: Testnet(), CallTimeout: 10 * time.Millisecond,
		HTTPClient: &http.Client{Transport: deadlineTransport(func(_ *http.Request) (*http.Response, error) {
			calls.Add(1)
			time.Sleep(5 * time.Millisecond)
			return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":1002,"message":"busy"}`))}, nil
		})},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	_, err = Call[struct{}, struct{}](context.Background(), rt, &RPCSpec{Method: "read", Gateway: GatewayMarketData, Path: "/read"}, nil, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v (attempts %d)", err, calls.Load())
	}
}

func TestNegativeCallTimeoutIsInvalid(t *testing.T) {
	if _, err := NewRuntime(Config{Env: Testnet(), CallTimeout: -time.Second}, nil); err == nil {
		t.Fatal("negative timeout accepted")
	}
	if _, err := NewRuntime(Config{Env: Testnet(), HTTPAttemptTimeout: -time.Second}, nil); err == nil {
		t.Fatal("negative attempt timeout accepted")
	}
}

type overdueContext struct{}

func (overdueContext) Done() <-chan struct{} { return nil }
func (overdueContext) Err() error            { return nil }
func (overdueContext) Value(any) any         { return nil }

func (overdueContext) Deadline() (time.Time, bool) { return time.Now().Add(-time.Second), true }
func TestExpiredClockDeadlineStopsDispatchBeforeTimerDelivery(t *testing.T) {
	var calls atomic.Int32
	rt, err := NewRuntime(Config{Env: Testnet(), HTTPClient: &http.Client{Transport: deadlineTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected dispatch")
	})}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	_, err = Call[struct{}, struct{}](overdueContext{}, rt, &RPCSpec{Method: "write", Write: true}, nil, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 0 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestOpaqueTransportCancellationWithoutTraceIsUnknown(t *testing.T) {
	var dispatched atomic.Bool
	rt, err := NewRuntime(Config{Env: Testnet(), CallTimeout: 10 * time.Millisecond,
		HTTPClient: &http.Client{Transport: deadlineTransport(func(r *http.Request) (*http.Response, error) {
			dispatched.Store(true)
			<-r.Context().Done()
			return nil, r.Context().Err()
		})},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	_, err = Call[struct{}, struct{}](context.Background(), rt, &RPCSpec{Method: "write", Write: true, Gateway: GatewayTrading, Path: "/write"}, nil, nil, nil)
	var unknown *OrderStateUnknownError
	if !dispatched.Load() || !errors.As(err, &unknown) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
}

func TestQueuedWSWriteCancellationDoesNotDropSharedConnection(t *testing.T) {
	eachEncoding(t, func(t *testing.T, enc Encoding) {
		g := newFakeGateway(t, enc)
		entered, release := make(chan struct{}), make(chan struct{})
		c, _ := wsTradingClient(t, g, func(cfg *Config) {
			cfg.OnEvent = func(e Event) {
				if e.Outgoing && bytes.Contains(e.Raw, []byte("active_read")) {
					close(entered)
					<-release
				}
			}
		})
		s := c.rt.Socket(GatewayTrading)
		s.start()
		epoch, _, err := s.waitUp(context.Background(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		// Hold a real concurrent request in its outgoing hook while it owns the writer.
		active := make(chan error, 1)
		go func() {
			_, requestErr := s.request(context.Background(), "active_read", []byte(`{}`), false, nil)
			active <- requestErr
		}()
		<-entered
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		_, err = s.request(ctx, "queued_write", []byte(`{}`), true, []uint64{42})
		var transport *TransportError
		if !errors.As(err, &transport) || transport.Sent || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("queued write must be unsent: %v", err)
		}
		close(release)
		if activeErr := <-active; activeErr != nil {
			t.Fatal(activeErr)
		}
		if g.calls("queued_write") != 0 {
			t.Fatal("cancelled write reached gateway")
		}
		_, err = s.request(context.Background(), "unrelated_read", []byte(`{}`), false, nil)
		if err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.epoch != epoch || s.state != connUp {
			t.Fatal("caller cancellation dropped shared socket")
		}
	})
}

func TestActiveWSWriteCancellationLeavesOtherCallAlive(t *testing.T) {
	release, started, received := make(chan struct{}), make(chan error, 1), make(chan error, 1)
	unblock := sync.OnceFunc(func() { close(release) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			started <- err
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(32 << 20)
		// Reader returns after the frame header arrives, before its payload is drained.
		_, reader, err := conn.Reader(r.Context())
		started <- err
		if err != nil {
			return
		}
		<-release
		if _, drainErr := io.Copy(io.Discard, reader); drainErr != nil {
			received <- drainErr
			return
		}
		_, _, err = conn.Read(r.Context())
		received <- err
	}))
	defer server.Close()
	defer unblock() // release the handler even if an assertion fails
	testCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	conn, _, err := websocket.Dial(testCtx, "ws"+strings.TrimPrefix(server.URL, "http"), nil) //nolint:bodyclose // websocket.Dial owns the response body
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	rt, err := NewRuntime(Config{Env: Testnet()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	s := newSocket(rt, "td", "")
	ctx, cancel := context.WithCancel(testCtx)
	defer cancel()
	frame := bytes.Repeat([]byte("x"), 16<<20)
	active := make(chan error, 1)
	go func() { active <- s.writeContext(ctx, conn, frame) }()
	select {
	case startupErr := <-started:
		if startupErr != nil {
			t.Fatal(startupErr)
		}
	case activeErr := <-active:
		t.Fatalf("active write finished before the server barrier: %v", activeErr)
	case <-testCtx.Done():
		t.Fatal("active writer did not reach the server")
	}
	// The server has the header but does not drain the large frame until after cancellation.
	cancel()
	var transport *TransportError
	select {
	case activeErr := <-active:
		if !errors.As(activeErr, &transport) || !transport.Sent || !errors.Is(activeErr, context.Canceled) {
			t.Fatalf("active write must return cancellation uncertainty: %v", activeErr)
		}
	case <-testCtx.Done():
		t.Fatal("active caller did not return on cancellation")
	}
	unrelated := make(chan error, 1)
	go func() { unrelated <- s.writeContext(testCtx, conn, []byte(`{}`)) }()
	unblock()
	select {
	case unrelatedErr := <-unrelated:
		if unrelatedErr != nil {
			t.Fatalf("unrelated caller lost shared connection: %v", unrelatedErr)
		}
	case <-testCtx.Done():
		t.Fatal("unrelated write did not finish")
	}
	select {
	case receivedErr := <-received:
		if receivedErr != nil {
			t.Fatal(receivedErr)
		}
	case <-testCtx.Done():
		t.Fatal("server did not receive both frames")
	}
}

func TestRuntimeCloseReleasesQueuedWSWriter(t *testing.T) {
	rt, err := NewRuntime(Config{Env: Testnet()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := rt.Socket(GatewayTrading)
	s.writeGate <- struct{}{}
	result := make(chan error, 1)
	go func() { result <- s.writeContext(context.Background(), nil, []byte(`{}`)) }()
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued caller survived runtime close")
	}
	<-s.writeGate
}

func TestOldWSWriteFailureCannotDropReplacementConnection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			kind, frame, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			if err := conn.Write(r.Context(), kind, frame); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	dial := func() *websocket.Conn {
		conn, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), nil) //nolint:bodyclose // websocket.Dial owns the response body
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}
	old, replacement := dial(), dial()
	defer old.CloseNow()
	defer replacement.CloseNow()
	rt, err := NewRuntime(Config{Env: Testnet()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	s := rt.Socket(GatewayTrading)
	s.mu.Lock()
	s.conn = replacement
	s.state = connUp
	s.mu.Unlock()
	// The old writer finishes only after the supervisor has installed its replacement.
	_ = old.CloseNow()
	if err := s.write(old, []byte(`{}`)); err == nil {
		t.Fatal("old write unexpectedly succeeded")
	}
	if err := s.write(replacement, []byte(`{"alive":true}`)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, frame, err := replacement.Read(ctx); err != nil || string(frame) != `{"alive":true}` {
		t.Fatalf("replacement failed: frame=%s err=%v", frame, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dropReason != "" {
		t.Fatalf("old writer marked replacement failed: %s", s.dropReason)
	}
}
