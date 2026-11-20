package grvt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/gravity-technologies/grvt-sdk-go/internal/behavior"
	"github.com/gravity-technologies/grvt-sdk-go/internal/wirejson"
	"github.com/gravity-technologies/grvt-sdk-go/models"
)

// One managed WebSocket per gateway ("md" market data, "td" trading).
//
// What a socket does:
//   - connects lazily, with the session cookie + X-Grvt-Account-Id on the handshake when logged in
//   - keeps the desired subscriptions and re-sends them after every reconnect
//   - JSON-RPC requests: uint32 id -> waiting caller, per-request timeout
//   - reconnects with full-jitter backoff; stops for good on an auth failure
//   - detects sequence gaps and re-subscribes the same selector for a fresh snapshot; escalates to a
//     reconnect if that snapshot does not come within the resync timeout (5s)
//   - the read loop only parses the envelope and enqueues raw bytes; consumers decode. It never waits
//     for a consumer: each subscription has a bounded queue.
//
// One goroutine (supervise) owns the connection's life. A frame is written to the connection it was
// built for; if that connection died, the write fails and nothing goes out on the next one.

const (
	// ConnectTimeout bounds one connect (login + handshake).
	ConnectTimeout = behavior.WSConnectTimeout
	// IdleTimeout is the default Config.IdleTimeout: no frame at all, not even a ping or pong, for this
	// long means the connection is dead.
	IdleTimeout = behavior.WSIdleTimeout
	// Heartbeat is the client ping interval with the default IdleTimeout: a third of it.
	Heartbeat = IdleTimeout / 3
	// ResyncTimeout: no snapshot this long after a gap re-subscribe means reconnect.
	ResyncTimeout = behavior.ResyncTimeout
)

type connState int

const (
	connDown connState = iota
	connUp
	connFatal
	connClosed
)

type rpcResult struct {
	data []byte
	err  error
}

// waiter is what a pending request resolves: an RPC caller, a subscription's subscribe, or nothing.
type waiter struct {
	rpc            chan rpcResult // buffered 1
	clientOrderIDs []uint64
	sub            *sub
}

// Socket is one managed WebSocket. Safe for concurrent use.
type Socket struct {
	name          string
	url           string
	rt            *Runtime
	resyncTimeout atomic.Int64
	idleTimeout   time.Duration // set before the first connect
	lastRx        atomic.Int64  // UnixNano of the current connection's last frame of any kind

	mu         sync.Mutex
	state      connState
	fatalErr   error
	changed    chan struct{} // closed and replaced on every state change
	conn       *websocket.Conn
	epoch      uint64 // current connection, 0 when down
	epochs     uint64
	dropReason string
	requests   *requestTable[waiter]
	subs       map[uint64]*sub
	byKey      map[string]map[string]*sub // stream -> selector as the server echoes it
	sessionGen uint64
	started    bool
	closing    bool
	closeCh    chan struct{}
	done       chan struct{}
	nextSubID  uint64
	writeGate  chan struct{}
}

func newSocket(rt *Runtime, name, url string) *Socket {
	s := &Socket{name: name, url: url, rt: rt, changed: make(chan struct{}), requests: newRequestTable[waiter](1),
		subs: map[uint64]*sub{}, byKey: map[string]map[string]*sub{}, closeCh: make(chan struct{}), done: make(chan struct{}), writeGate: make(chan struct{}, 1)}
	s.resyncTimeout.Store(int64(ResyncTimeout))
	s.idleTimeout = IdleTimeout
	return s
}

// Name is "md" or "td".
func (s *Socket) Name() string { return s.name }

// SetResyncTimeout sets how long to wait for the snapshot after a gap re-subscribe before reconnecting.
func (s *Socket) SetResyncTimeout(d time.Duration) { s.resyncTimeout.Store(int64(d)) }

// ForceReconnect drops the connection; it reconnects and every subscription re-syncs from a snapshot.
func (s *Socket) ForceReconnect(reason string) { s.drop(reason) }

// setStateLocked changes the state and wakes everyone waiting in waitUp.
func (s *Socket) setStateLocked(st connState) {
	s.state = st
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *Socket) start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.closing {
		return
	}
	s.started = true
	go s.supervise()
}

// waitUp waits until the socket is connected and returns that connection.
func (s *Socket) waitUp(ctx context.Context, timeout time.Duration) (uint64, *websocket.Conn, error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	for {
		s.mu.Lock()
		switch s.state {
		case connUp:
			e, c := s.epoch, s.conn
			s.mu.Unlock()
			return e, c, nil
		case connFatal:
			err := s.fatalErr
			s.mu.Unlock()
			return 0, nil, err
		case connClosed:
			s.mu.Unlock()
			return 0, nil, ErrClosed
		}
		ch := s.changed
		s.mu.Unlock()
		select {
		case <-ch:
		case <-t.C:
			return 0, nil, &TransportError{Message: fmt.Sprintf("%s socket not connected after %s", s.name, timeout)}
		case <-ctx.Done():
			return 0, nil, &TransportError{Message: s.name + " socket: " + ctx.Err().Error(), Err: ctx.Err()}
		}
	}
}

// write sends one text frame on conn. A cancelled write kills the connection, so the deadline comes
// from the runtime, not from the caller.
//
//nolint:contextcheck // bound to the runtime lifetime (Close), not to the call that triggered it
func (s *Socket) write(conn *websocket.Conn, frame []byte) error {
	return s.writeContext(s.rt.ctx, conn, frame)
}

func (s *Socket) writeContext(ctx context.Context, conn *websocket.Conn, frame []byte) error {
	// Waiters belong to callers; an active socket write belongs to the runtime.
	select {
	case s.writeGate <- struct{}{}:
	case <-ctx.Done():
		return &TransportError{Message: ctx.Err().Error(), Err: context.Cause(ctx)}
	case <-s.rt.ctx.Done():
		return ErrClosed
	}
	s.rt.hooks.emit(func() Event { return Event{Kind: EventFrame, Socket: s.name, Raw: frame, Outgoing: true} })
	if s.rt.log.Enabled(ctx, slog.LevelDebug) {
		s.rt.log.Debug(s.name+" ->", "frame", Redact(frame, 2000))
	}
	if err := deadlineError(ctx); err != nil {
		<-s.writeGate
		return &TransportError{Message: err.Error(), Err: err}
	}
	result := make(chan error, 1)
	go func() {
		defer func() { <-s.writeGate }()
		// The worker may have been scheduled after the caller stopped waiting.
		if err := deadlineError(ctx); err != nil {
			result <- &TransportError{Message: err.Error(), Err: err}
			return
		}
		writeCtx, cancel := context.WithTimeout(s.rt.ctx, 5*time.Second)
		defer cancel()
		err := conn.Write(writeCtx, websocket.MessageText, frame) //nolint:contextcheck // caller cancellation must not close the shared connection
		if err != nil {
			s.dropConn(conn, "send failed: "+err.Error())
		}
		result <- err
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return &TransportError{Message: ctx.Err().Error(), Sent: true, Err: context.Cause(ctx)}
	}
}

// writeAsync is write from the read goroutine, which must never block.
func (s *Socket) writeAsync(conn *websocket.Conn, frame []byte) {
	go func() {
		_ = s.write(conn, frame) //nolint:errcheck // the writer handles connection failure
	}()
}

// request sends one JSON-RPC request and returns the raw result JSON.
func (s *Socket) request(ctx context.Context, method string, params []byte, write bool, clientOrderIDs []uint64) ([]byte, error) {
	s.start()
	epoch, conn, err := s.waitUp(ctx, ConnectTimeout) // failure here: nothing was sent
	if err != nil {
		return nil, err
	}
	w := waiter{rpc: make(chan rpcResult, 1), clientOrderIDs: clientOrderIDs}
	s.mu.Lock()
	if s.epoch != epoch {
		s.mu.Unlock()
		return nil, &TransportError{Message: "socket reconnecting", RPC: method}
	}
	id, ok := s.requests.add(&pending[waiter]{method: method, write: write, waiter: w})
	s.mu.Unlock()
	if !ok {
		return nil, &TransportError{Message: "request table full", RPC: method}
	}
	frame := s.frame(method, params, id)
	if limit := s.rt.env.WSMaxFrameBytes; limit > 0 && len(frame) > limit {
		// TODO: fall back to REST once the runtime can tell the RPC is safe to reroute.
		s.forget(id)
		return nil, &FrameTooLargeError{Size: len(frame), Limit: limit}
	}
	unknown := func(cause error) error {
		if write {
			return unknownWrite(&RPCSpec{Method: method}, cause, clientOrderIDs)
		}
		return cause
	}
	if err := s.writeContext(ctx, conn, frame); err != nil {
		s.forget(id)
		var transport *TransportError
		if errors.As(err, &transport) && !transport.Sent {
			return nil, transport
		}
		if errors.Is(err, net.ErrClosed) {
			return nil, &TransportError{Message: "socket closed before send", RPC: method, Err: err}
		}
		return nil, unknown(&TransportError{Message: "send failed: " + err.Error(), RPC: method, Sent: true, Err: err}) // may have left partly
	}
	t := time.NewTimer(s.rt.wsRequestTimeout)
	defer t.Stop()
	select {
	case r := <-w.rpc:
		return r.data, r.err
	case <-t.C:
		s.forget(id)
		return nil, unknown(&TransportError{Message: fmt.Sprintf("no reply within %s", s.rt.wsRequestTimeout), RPC: method, Sent: true, Err: context.DeadlineExceeded})
	case <-ctx.Done():
		s.forget(id)
		if write {
			return nil, unknownWrite(&RPCSpec{Method: method, Write: true}, context.Cause(ctx), clientOrderIDs)
		}
		return nil, &TransportError{Message: ctx.Err().Error(), RPC: method, Sent: true, Err: context.Cause(ctx)}
	}
}

func (s *Socket) forget(id uint32) {
	s.mu.Lock()
	s.requests.pop(id)
	s.mu.Unlock()
}

// subscribeLocked registers a subscribe for sb on connection epoch and returns the frame to send.
// The caller holds s.mu.
func (s *Socket) subscribeLocked(sb *sub, epoch uint64) []byte {
	sb.mu.Lock()
	sb.seq.Reset() // drop every frame until the new subscribe reply arrives
	sb.sentEpoch = epoch
	sb.mu.Unlock()
	id, ok := s.requests.add(&pending[waiter]{method: "subscribe", waiter: waiter{sub: sb}})
	if !ok {
		return nil
	}
	return s.frame("subscribe", sb.params, id)
}

func (s *Socket) subscribe(ctx context.Context, spec *StreamSpec, selector string, capacity int) (*sub, error) {
	params, err := json.Marshal(models.WSSubscribeParams{Stream: spec.Stream, Selectors: []string{selector}, UseGlobalSequenceNumber: Ptr(true)})
	if err == nil && s.rt.lite {
		// WSUnsubscribeParams has the same fields and lite names, so the same params unsubscribe.
		params, err = wirejson.ToLite[models.WSSubscribeParams](params)
	}
	if err != nil {
		return nil, &ConfigError{Message: fmt.Sprintf("subscribe %s: cannot encode params: %v", spec.Stream, err)}
	}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	s.nextSubID++
	sb := &sub{id: s.nextSubID, spec: spec, selector: selector, params: params, capacity: max(capacity, 1),
		ack: make(chan error, 1), notify: make(chan struct{}, 1)}
	s.subs[sb.id] = sb
	ack := sb.ack
	s.mu.Unlock()
	s.start()
	fail := func(err error) (*sub, error) {
		s.unsubscribe(sb, false)
		return nil, err
	}
	epoch, conn, err := s.waitUp(ctx, ConnectTimeout)
	if err != nil {
		return fail(err)
	}
	s.mu.Lock()
	var frame []byte
	sb.mu.Lock()
	sent := sb.sentEpoch == epoch // a fresh connect re-sends every desired sub; only send if it did not
	sb.mu.Unlock()
	if !sent && s.epoch == epoch {
		frame = s.subscribeLocked(sb, epoch)
	}
	s.mu.Unlock()
	if frame != nil {
		_ = s.write(conn, frame) //nolint:errcheck // the writer handles connection failure
	}
	t := time.NewTimer(s.rt.wsRequestTimeout)
	defer t.Stop()
	select {
	case err := <-ack:
		if err != nil {
			return fail(err)
		}
		return sb, nil
	case <-t.C:
		return fail(&TransportError{Message: fmt.Sprintf("no subscribe reply within %s", s.rt.wsRequestTimeout), RPC: "subscribe " + spec.Stream})
	case <-ctx.Done():
		return fail(&TransportError{Message: ctx.Err().Error(), RPC: "subscribe " + spec.Stream, Err: ctx.Err()})
	}
}

// unsubscribe forgets sb and, when connected and asked to, tells the server.
func (s *Socket) unsubscribe(sb *sub, tell bool) {
	s.mu.Lock()
	delete(s.subs, sb.id)
	sb.mu.Lock()
	key := sb.key
	sb.key = nil
	sb.mu.Unlock()
	if key != nil {
		if m := s.byKey[key[0]]; m != nil && m[key[1]] == sb {
			delete(m, key[1])
		}
	}
	var frame []byte
	conn := s.conn
	if tell && s.state == connUp {
		if id, ok := s.requests.add(&pending[waiter]{method: "unsubscribe"}); ok {
			frame = s.frame("unsubscribe", sb.params, id)
		}
	}
	s.mu.Unlock()
	if frame != nil {
		s.writeAsync(conn, frame)
	}
}

// frame is a JSON-RPC request in the socket's encoding.
func (s *Socket) frame(method string, params []byte, id uint32) []byte {
	if s.rt.lite {
		return liteJSONRPCFrame(method, params, id)
	}
	return jsonrpcFrame(method, params, id)
}

// drop closes the current connection (if any); supervise reconnects unless closing.
func (s *Socket) drop(reason string) {
	s.mu.Lock()
	c := s.conn
	if c != nil && s.dropReason == "" {
		s.dropReason = reason
	}
	s.mu.Unlock()
	if c != nil {
		s.closeNow(c)
	}
}

// dropConn closes one connection without affecting a replacement connection.
func (s *Socket) dropConn(conn *websocket.Conn, reason string) {
	s.mu.Lock()
	if s.conn == conn && s.dropReason == "" {
		s.dropReason = reason
	}
	s.mu.Unlock()
	s.closeNow(conn)
}

// closeNow closes c without a close handshake; closing an already closed connection is fine.
func (s *Socket) closeNow(c *websocket.Conn) {
	if err := c.CloseNow(); err != nil && !errors.Is(err, net.ErrClosed) {
		s.rt.log.Debug(s.name+" close failed", "err", err)
	}
}

// connect dials one connection for the supervisor.
//
//nolint:contextcheck // bound to the runtime lifetime (Close), not to the call that triggered it
func (s *Socket) connect() (*websocket.Conn, error) {
	ctx, cancel := context.WithTimeout(s.rt.ctx, ConnectTimeout)
	defer cancel()
	h := http.Header{}
	if s.rt.session.enabled() {
		if err := s.rt.session.ensure(ctx); err != nil {
			return nil, err
		}
		h = s.rt.session.headers()
		gen := s.rt.session.snapshot().Generation
		s.mu.Lock()
		s.sessionGen = gen
		s.mu.Unlock()
	}
	// Read returns data frames only, so pings and pongs reach the idle check through these callbacks.
	seen := func(context.Context, []byte) { s.lastRx.Store(time.Now().UnixNano()) }
	opts := &websocket.DialOptions{HTTPHeader: h, HTTPClient: s.rt.http, OnPongReceived: seen,
		OnPingReceived: func(ctx context.Context, b []byte) bool { seen(ctx, b); return true }}
	conn, resp, err := websocket.Dial(ctx, s.url, opts) //nolint:bodyclose // websocket.Dial owns resp.Body: nil on success, an in-memory copy on failure
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			// Rejected handshake (e.g. sticky-session check): retrying will not help.
			return nil, &APIError{Code: 1000, Message: "websocket handshake rejected: " + resp.Status, Status: resp.StatusCode}
		}
		return nil, &TransportError{Message: "connect failed: " + err.Error(), Err: err}
	}
	conn.SetReadLimit(behavior.WSMaxIncomingFrame)
	return conn, nil
}

func fatal(err error) bool {
	var api *APIError
	var cfg *ConfigError
	return (errors.As(err, &api) && !api.Retryable) || errors.As(err, &cfg)
}

// newReconnectBackoff is the backoff between reconnects.
func newReconnectBackoff() *Backoff {
	return newBackoff(behavior.ReconnectBackoffBase, behavior.ReconnectBackoffCap)
}

func (s *Socket) supervise() {
	defer close(s.done)
	backoff := newReconnectBackoff()
	connectedOnce := false
	for {
		if s.isClosing() {
			return
		}
		conn, err := s.connect()
		if err != nil {
			if s.isClosing() {
				return
			}
			if fatal(err) {
				s.goFatal(err)
				return
			}
			delay := backoff.Next()
			s.rt.log.Debug(s.name+" connect failed", "err", err, "retry_in", delay)
			s.rt.hooks.emit(func() Event {
				return Event{Kind: EventRetry, Socket: s.name, Attempt: backoff.Attempt, Delay: delay, Err: err.Error()}
			})
			if s.sleep(delay) {
				return
			}
			continue
		}
		s.mu.Lock()
		if s.closing {
			s.mu.Unlock()
			s.closeNow(conn)
			return
		}
		s.epochs++
		epoch := s.epochs
		s.epoch, s.conn, s.dropReason = epoch, conn, ""
		var frames [][]byte
		for _, sb := range s.subs {
			if f := s.subscribeLocked(sb, epoch); f != nil {
				frames = append(frames, f)
			}
		}
		s.setStateLocked(connUp)
		s.mu.Unlock()
		for _, f := range frames {
			if err := s.write(conn, f); err != nil {
				break
			}
		}
		kind := EventConnect
		if connectedOnce {
			kind = EventReconnect
		}
		connectedOnce = true
		s.rt.hooks.emit(func() Event { return Event{Kind: kind, Socket: s.name} })

		upSince := time.Now()
		code, reason := s.run(conn)
		if time.Since(upSince) > behavior.ReconnectBackoffResetAfter {
			backoff.Reset()
		}

		s.mu.Lock()
		s.epoch, s.conn = 0, nil
		if s.state == connUp {
			s.setStateLocked(connDown)
		}
		drained := s.requests.drain()
		var gaps []Event
		for _, sb := range s.subs {
			sb.mu.Lock()
			if !s.closing {
				var after *uint64
				if sb.seq.Last != 0 {
					after = Ptr(sb.seq.Last)
				}
				gaps = append(gaps, Event{Kind: EventGap, Socket: s.name, Stream: sb.spec.Stream, Selector: sb.selector, Gap: &GapRange{AfterSequence: after, Reason: "disconnect"}})
			}
			sb.seq.Reset()
			sb.mu.Unlock()
		}
		closing := s.closing
		s.mu.Unlock()
		for _, event := range gaps {
			s.rt.hooks.emit(func() Event { return event })
		}
		failPending(drained, fmt.Sprintf("socket closed (%s, code %d)", reason, code))
		s.rt.hooks.emit(func() Event { return Event{Kind: EventDisconnect, Socket: s.name, Code: code, Reason: reason} })
		if closing || s.sleep(backoff.Next()) {
			return
		}
	}
}

func (s *Socket) isClosing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closing
}

// sleep waits for d; it returns true at once if the socket is closing.
func (s *Socket) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-s.closeCh:
		return true
	case <-t.C:
		return s.isClosing()
	}
}

// failPending fails everything still waiting for a reply. Writes become OrderStateUnknownError.
func failPending(drained []*pending[waiter], reason string) {
	for _, p := range drained {
		if p.waiter.rpc == nil {
			continue
		}
		var err error
		if p.write {
			err = unknownWrite(&RPCSpec{Method: p.method}, &TransportError{Message: reason, RPC: p.method, Sent: true}, p.waiter.clientOrderIDs)
		} else {
			err = &TransportError{Message: reason, RPC: p.method, Sent: true}
		}
		p.waiter.rpc <- rpcResult{err: err}
	}
}

func (s *Socket) goFatal(err error) {
	s.mu.Lock()
	s.fatalErr = err
	s.setStateLocked(connFatal)
	subs := make([]*sub, 0, len(s.subs))
	for _, sb := range s.subs {
		subs = append(subs, sb)
	}
	s.mu.Unlock()
	s.rt.hooks.emit(func() Event { return Event{Kind: EventError, Socket: s.name, Fatal: true, Err: err.Error()} })
	for _, sb := range subs {
		sb.fail(err, true)
	}
}

func (s *Socket) close() {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	s.closing = true
	close(s.closeCh)
	c := s.conn
	started := s.started
	s.dropReason = "client closed"
	s.setStateLocked(connClosed)
	s.mu.Unlock()
	if c != nil {
		s.closeNow(c)
	}
	if started {
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
			s.rt.log.Debug(s.name + " socket did not close in time")
		}
	}
	s.mu.Lock()
	drained := s.requests.drain()
	subs := make([]*sub, 0, len(s.subs))
	for id, sb := range s.subs {
		subs = append(subs, sb)
		delete(s.subs, id)
	}
	s.mu.Unlock()
	failPending(drained, "client closed")
	for _, sb := range subs {
		sb.end()
	}
}

// run drives one connection until it ends and returns the close code and why it ended.
//
//nolint:contextcheck // bound to the runtime lifetime (Close), not to the call that triggered it
func (s *Socket) run(conn *websocket.Conn) (int, string) {
	ctx, cancel := context.WithCancel(s.rt.ctx)
	defer cancel()
	s.lastRx.Store(time.Now().UnixNano())
	go s.watchdog(ctx, conn)
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			code := int(websocket.CloseStatus(err))
			s.mu.Lock()
			reason := s.dropReason
			s.mu.Unlock()
			switch {
			case reason != "":
			case code == 1008:
				reason = "slow consumer (server 1008)"
			case code > 0:
				reason = "closed by server"
			default:
				reason, code = "read error: "+err.Error(), 0
			}
			if code < 0 {
				code = 0
			}
			s.closeNow(conn)
			return code, reason
		}
		s.lastRx.Store(time.Now().UnixNano())
		s.onFrame(ctx, data)
	}
}

// watchdog drops the connection when a re-sync is overdue, the session rotated (and no write is in
// flight), or no frame of any kind arrived for the idle timeout. It pings every third of that timeout;
// a missing pong is left to the idle check, so silence has one rule. Only one ping waits for its pong at
// a time, so a server that sends data but never answers pings doesn't pile up waiting goroutines.
func (s *Socket) watchdog(ctx context.Context, conn *websocket.Conn) {
	tick := time.NewTicker(behavior.WatchdogInterval)
	defer tick.Stop()
	lastPing := time.Now()
	var pinging atomic.Bool
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			if reason := s.dropWhy(now); reason != "" {
				s.dropConn(conn, reason)
				return
			}
			if now.Sub(time.Unix(0, s.lastRx.Load())) > s.idleTimeout {
				s.dropConn(conn, fmt.Sprintf("no frames for %s", s.idleTimeout))
				return
			}
			if now.Sub(lastPing) >= s.idleTimeout/3 && pinging.CompareAndSwap(false, true) {
				lastPing = now
				go func() {
					defer pinging.Store(false)
					// Ping waits for its pong until the connection ends; only a failed write drops it.
					if err := conn.Ping(ctx); err != nil && ctx.Err() == nil {
						s.dropConn(conn, "ping failed: "+err.Error())
					}
				}()
			}
		}
	}
}

func (s *Socket) dropWhy(now time.Time) string {
	limit := time.Duration(s.resyncTimeout.Load())
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sb := range s.subs {
		sb.mu.Lock()
		overdue := sb.seq.ResyncOverdue(now, limit)
		if overdue {
			sb.seq.ResyncStarted = time.Time{}
		}
		sb.mu.Unlock()
		if overdue {
			return fmt.Sprintf("no snapshot within %s of re-subscribe", limit)
		}
	}
	// Break-before-make: wait for a moment with no writes in flight, then reconnect with the
	// new cookie; subscriptions re-sync from snapshots.
	if s.rt.session.enabled() && s.rt.session.snapshot().Generation != s.sessionGen && s.requests.writesInFlight() == 0 {
		return "session rotated"
	}
	return ""
}

// -- incoming (runs on the read goroutine; never waits) -----------------------------------------------

// envelope is what the read loop needs from a frame to route it. decodeEnvelope fills it in one pass
// with the rules encoding/json used for it (TestEnvelopeMatchesEncodingJSON); Stream and Selector are
// strings on the wire, kept as bytes so routing does not allocate.
type envelope struct {
	ID       uint32
	Stream   []byte
	Selector []byte
	Seq      json.RawMessage // sequence_number
	Prev     json.RawMessage // prev_sequence_number
	Result   json.RawMessage
	Error    *frameError
	Code     int
	Message  string
}

type frameError struct {
	Body json.RawMessage
}

// decodeEnvelope is json.Unmarshal(data, f) without reflection or copies: the raw fields point into
// data, and every byte of data is validated. That is safe because each WebSocket message is read into
// its own buffer, which the runtime never reuses or changes; OnEvent hooks get the same buffer as
// Event.Raw and must not change it either.
func decodeEnvelope(data []byte, f *envelope) error {
	r := wirejson.NewReader(data)
	if err := f.decode(&r); err != nil {
		return r.Fail(err)
	}
	return r.End()
}

// decodeEnvelopeLite is decodeEnvelope for a lite frame: lite keys, matched exactly.
func decodeEnvelopeLite(data []byte, f *envelope) error {
	r := wirejson.NewLiteReader(data)
	if err := f.decode(&r); err != nil {
		return r.Fail(err)
	}
	return r.End()
}

func (f *envelope) decode(r *wirejson.Reader) error {
	if open, err := openPlainObject(r); !open {
		return err
	}
	for first := true; ; first = false {
		key, ok, err := r.Member(first)
		if err != nil || !ok {
			return err
		}
		field := ""
		if r.Lite() {
			field = envelopeFieldLite(key)
			// In lite, a gateway error body's status shares the key of a frame's stream; only a
			// string is a stream.
			if field == "stream" && r.Next() != '"' {
				field = ""
			}
		} else {
			field = envelopeField(key)
		}
		switch field {
		case "id":
			err = wirejson.DecodeUint(r, &f.ID)
		case "stream":
			err = decodeBytes(r, &f.Stream)
		case "selector":
			err = decodeBytes(r, &f.Selector)
		case "sequence_number":
			f.Seq, err = r.Raw()
		case "prev_sequence_number":
			f.Prev, err = r.Raw()
		case "result":
			f.Result, err = r.Raw()
		case "code":
			err = wirejson.DecodeInt(r, &f.Code)
		case "message":
			err = wirejson.DecodeString(r, &f.Message)
		case "error":
			if r.Next() == 'n' {
				f.Error = nil
				_, err = r.Null()
				break
			}
			if f.Error == nil {
				f.Error = &frameError{}
			}
			err = f.Error.decode(r)
		default:
			err = r.Skip()
		}
		if err != nil {
			return err
		}
	}
}

func (e *frameError) decode(r *wirejson.Reader) error {
	var err error
	e.Body, err = r.Raw()
	return err
}

// decodeBytes is wirejson.DecodeString for a string kept as bytes (null keeps the old value).
func decodeBytes(r *wirejson.Reader, dst *[]byte) error {
	switch r.Next() {
	case '"':
		b, err := r.String()
		if err == nil {
			*dst = b
		}
		return err
	case 'n':
		_, err := r.Null()
		return err
	}
	return wirejson.Unexpected[string](r)
}

// openPlainObject starts decoding a struct the way encoding/json does: an object is opened (true),
// null leaves the struct as it is (false, nil), anything else is an error.
func openPlainObject(r *wirejson.Reader) (bool, error) {
	switch r.Next() {
	case '{':
		return true, r.OpenObject()
	case 'n':
		_, err := r.Null()
		return false, err
	}
	return false, wirejson.Unexpected[struct{}](r)
}

// envelopeField matches a key like encoding/json: exactly, then without case.
func envelopeField(key []byte) string {
	switch string(key) {
	case "id":
		return "id"
	case "stream":
		return "stream"
	case "selector":
		return "selector"
	case "sequence_number":
		return "sequence_number"
	case "prev_sequence_number":
		return "prev_sequence_number"
	case "result":
		return "result"
	case "error":
		return "error"
	case "code":
		return "code"
	case "message":
		return "message"
	}
	var buf [64]byte
	switch string(wirejson.Fold(buf[:0], key)) {
	case "ID":
		return "id"
	case "STREAM":
		return "stream"
	case "SELECTOR":
		return "selector"
	case "SEQUENCE_NUMBER":
		return "sequence_number"
	case "PREV_SEQUENCE_NUMBER":
		return "prev_sequence_number"
	case "RESULT":
		return "result"
	case "ERROR":
		return "error"
	case "CODE":
		return "code"
	case "MESSAGE":
		return "message"
	}
	return ""
}

// envelopeFieldLite matches a lite key (exactly: lite has no case folding). The method key of a
// reply is not read, and the error message only inside the error object, so "m" is not here.
func envelopeFieldLite(key []byte) string {
	switch string(key) {
	case liteResponseID:
		return "id"
	case liteFrameStream:
		return "stream"
	case liteFrameSelector:
		return "selector"
	case liteFrameSequence:
		return "sequence_number"
	case liteFramePrevSequence:
		return "prev_sequence_number"
	case liteResponseResult:
		return "result"
	case liteResponseError:
		return "error"
	case liteHTTPErrorCode:
		return "code"
	}
	return ""
}

// wireUint reads a 64-bit integer sent as a JSON string or number; ok=false when absent.
func wireUint(raw json.RawMessage) (uint64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	t := string(raw)
	if t[0] == '"' {
		t = t[1 : len(t)-1]
	}
	n, err := strconv.ParseUint(t, 10, 64)
	return n, err == nil
}

func (s *Socket) onFrame(ctx context.Context, data []byte) {
	s.rt.hooks.emit(func() Event { return Event{Kind: EventFrame, Socket: s.name, Raw: data} })
	if s.rt.log.Enabled(ctx, slog.LevelDebug) {
		s.rt.log.Debug(s.name+" <-", "frame", Redact(data, 2000))
	}
	var f envelope
	decode := decodeEnvelope
	if s.rt.lite {
		decode = decodeEnvelopeLite
	}
	if err := decode(data, &f); err != nil {
		s.rt.log.Warn("grvt: undecodable frame dropped", "socket", s.name, "err", err)
		return
	}
	if f.ID == 0 {
		if len(f.Stream) == 0 || len(f.Selector) == 0 {
			return
		}
		s.mu.Lock()
		sb := s.byKey[string(f.Stream)][string(f.Selector)]
		s.mu.Unlock()
		if sb != nil {
			seq, _ := wireUint(f.Seq)
			prev, hasPrev := wireUint(f.Prev)
			s.onFeed(sb, seq, prev, hasPrev, data)
		}
		return
	}
	s.mu.Lock()
	p := s.requests.pop(f.ID)
	s.mu.Unlock()
	if p == nil {
		s.rt.log.Debug(s.name+" reply for unknown id (late or not ours)", "id", f.ID)
		return
	}
	if f.Error != nil || (f.Code != 0 && len(f.Result) == 0) {
		var err error
		switch {
		case !s.rt.lite && f.Error != nil:
			err = s.rt.errors.responseError(f.Error.Body, 0, p.method)
		case !s.rt.lite:
			err = s.rt.errors.responseError(data, 0, p.method)
		case f.Error != nil:
			err = s.rt.errors.responseErrorKeys(f.Error.Body, 0, p.method, liteErrorCode, liteErrorMessage)
		default:
			err = s.rt.errors.responseErrorKeys(data, 0, p.method, liteHTTPErrorCode, liteHTTPErrorMessage)
		}
		switch {
		case p.waiter.rpc != nil:
			p.waiter.rpc <- rpcResult{err: err}
		case p.waiter.sub != nil:
			p.waiter.sub.fail(err, false)
		}
		return
	}
	// A copy, so a caller keeping the result does not keep the whole frame.
	result := bytes.Clone(f.Result)
	if len(result) == 0 {
		result = []byte("null")
	}
	switch {
	case p.waiter.rpc != nil:
		p.waiter.rpc <- rpcResult{data: result}
	case p.waiter.sub != nil:
		s.onSubscribeReply(p.waiter.sub, result)
	}
}

func (s *Socket) onSubscribeReply(sb *sub, result []byte) {
	// Runs before the snapshot frames that follow the ack.
	var r models.WSSubscribeResult
	if err := decodeWire(s.rt, result, &r); err != nil {
		s.rt.log.Warn("grvt: undecodable subscribe reply", "socket", s.name, "stream", sb.spec.Stream, "err", err)
	}
	canonical := sb.selector
	if len(r.Subs) > 0 {
		canonical = r.Subs[0]
	}
	var latest, snaps uint64
	if len(r.LatestSequenceNumber) > 0 {
		latest = r.LatestSequenceNumber[0]
	}
	if len(r.NumSnapshots) > 0 {
		snaps = uint64(r.NumSnapshots[0])
	}
	s.mu.Lock()
	if _, ok := s.subs[sb.id]; !ok {
		s.mu.Unlock()
		return // unsubscribed meanwhile
	}
	sb.mu.Lock()
	old := sb.key
	sb.key = &[2]string{sb.spec.Stream, canonical}
	sb.mu.Unlock()
	if old != nil && old[1] != canonical {
		delete(s.byKey[old[0]], old[1])
	}
	if s.byKey[sb.spec.Stream] == nil {
		s.byKey[sb.spec.Stream] = map[string]*sub{}
	}
	s.byKey[sb.spec.Stream][canonical] = sb
	s.mu.Unlock()

	sb.mu.Lock()
	sb.seq.OnAck(latest, snaps)
	sb.mu.Unlock()
	if ch := sb.takeAck(); ch != nil {
		ch <- nil
	}
	s.rt.hooks.emit(func() Event {
		return Event{Kind: EventSubscribe, Socket: s.name, Stream: sb.spec.Stream, Selector: canonical,
			LatestSequenceNumber: latest, NumSnapshots: snaps}
	})
}

func (s *Socket) onFeed(sb *sub, seq, prev uint64, hasPrev bool, data []byte) {
	sb.mu.Lock()
	switch sb.seq.OnFrame(seq, prev, hasPrev) {
	case Deliver:
		ok := sb.pushLocked(data)
		sb.mu.Unlock()
		if !ok {
			s.resync(sb, "overflow")
		}
	case Drop:
		sb.mu.Unlock()
	case Gap:
		expected := sb.seq.Last
		snapshotFeed := sb.spec.SnapshotFeed
		if snapshotFeed { // every frame is complete; just move on
			sb.seq.Last = seq
			sb.pushLocked(data)
		}
		sb.mu.Unlock()
		s.rt.hooks.emit(func() Event {
			var through *uint64
			if snapshotFeed && prev >= expected {
				through = Ptr(prev)
			}
			return Event{Kind: EventGap, Gap: &GapRange{AfterSequence: Ptr(expected), ThroughSequence: through, Reason: "sequence_mismatch"}, Socket: s.name, Stream: sb.spec.Stream, Selector: sb.selector,
				ExpectedPrev: expected, Prev: prev, Seq: seq}
		})
		if !snapshotFeed {
			s.resync(sb, "gap")
		}
	}
}

// resync re-subscribes the same selector; frames are dropped until the new snapshot.
func (s *Socket) resync(sb *sub, why string) {
	if why == "overflow" {
		s.rt.hooks.emit(func() Event {
			return Event{Kind: EventGap, Socket: s.name, Stream: sb.spec.Stream, Selector: sb.selector, Gap: &GapRange{Reason: "overflow"}}
		})
	}
	s.rt.log.Debug(s.name+" re-subscribing", "stream", sb.spec.Stream, "selector", sb.selector, "why", why)
	sb.mu.Lock()
	sb.seq.ResyncStarted = time.Now()
	sb.mu.Unlock()
	s.mu.Lock()
	var frame []byte
	conn := s.conn
	if s.epoch != 0 {
		frame = s.subscribeLocked(sb, s.epoch)
	}
	s.mu.Unlock()
	if frame != nil {
		s.writeAsync(conn, frame)
	}
}
