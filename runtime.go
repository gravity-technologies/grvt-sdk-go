// Package grvt is the Go SDK for Grvt: REST, WebSocket streams, WebSocket order entry, EIP-712 signing
// and login in one package. Everything that grows with the API (models, enums, error codes, RPC and
// stream methods, sign functions) is generated from sdk/contract/contract.json into *_gen.go files and
// package models; the hand-written runtime stays the same size however many APIs exist. See README.md
// and RUNTIME_API.md.
package grvt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gravity-technologies/grvt-sdk-go/internal/behavior"
	"github.com/gravity-technologies/grvt-sdk-go/internal/wirejson"
)

// Runtime is the one object generated code talks to. It holds the environment, both sockets, the
// session and the metadata cache. Same size however many RPCs or streams exist.
type Runtime struct {
	subAccountID       uint64
	env                Environment
	signer             Signer
	orderTransport     OrderTransport
	lite               bool // Config.Encoding is EncodingLite
	checkOrders        bool
	clock              Clock
	nonces             NonceProvider
	clientOrderIDs     ClientOrderIDProvider
	http               *http.Client
	hooks              *hooks
	log                *slog.Logger
	errors             ErrorTable
	httpAttemptTimeout time.Duration
	callTimeout        time.Duration
	wsRequestTimeout   time.Duration
	queueSize          int
	// sessionRetryDelay is the wait after a failed session renewal.
	sessionRetryDelay time.Duration
	metadata          MetadataCache
	session           *session
	domain            *Domain
	md, td            *Socket

	ctx      context.Context //nolint:containedctx // lifetime of background work, not a request; cancelled by Close
	cancel   context.CancelFunc
	closed   atomic.Bool
	rotation sync.Once
}

// NewRuntime builds the runtime. errors is the generated error table; New passes it.
func NewRuntime(cfg Config, errors ErrorTable) (*Runtime, error) {
	if cfg.CallTimeout < 0 || cfg.HTTPAttemptTimeout < 0 || cfg.WSRequestTimeout < 0 || cfg.ResyncTimeout < 0 || cfg.IdleTimeout < 0 {
		return nil, &ConfigError{Message: "CallTimeout, HTTPAttemptTimeout, WSRequestTimeout, ResyncTimeout and IdleTimeout must be positive (zero uses defaults)"}
	}
	if cfg.QueueSize != 0 && cfg.QueueSize < behavior.QueueSizeMin {
		return nil, &ConfigError{Message: fmt.Sprintf("QueueSize must be at least %d (zero uses the default)", behavior.QueueSizeMin)}
	}
	if cfg.Env == (Environment{}) {
		return nil, &ConfigError{Message: "Config.Env is required: grvt.Testnet(), grvt.Prod() or your own Environment"}
	}
	if err := cfg.Env.Validate(); err != nil {
		return nil, err
	}
	if cfg.Encoding != EncodingFull && cfg.Encoding != EncodingLite {
		return nil, &ConfigError{Message: fmt.Sprintf("unknown Encoding %d: use grvt.EncodingFull or grvt.EncodingLite", int(cfg.Encoding))}
	}
	mdURL, err := cfg.Env.wsURL(GatewayMarketData, cfg.Encoding == EncodingLite)
	if err != nil {
		return nil, err
	}
	tdURL, err := cfg.Env.wsURL(GatewayTrading, cfg.Encoding == EncodingLite)
	if err != nil {
		return nil, err
	}
	rt := &Runtime{
		subAccountID: cfg.SubAccountID, env: cfg.Env, signer: cfg.Signer, orderTransport: cfg.OrderTransport,
		lite: cfg.Encoding == EncodingLite, clock: cfg.Clock, nonces: cfg.Nonces, clientOrderIDs: cfg.ClientOrderIDs,
		http: cfg.HTTPClient, log: cfg.Logger, errors: errors, checkOrders: cfg.CheckOrders == nil || *cfg.CheckOrders,
		callTimeout: cfg.CallTimeout, httpAttemptTimeout: cfg.HTTPAttemptTimeout, wsRequestTimeout: cfg.WSRequestTimeout, queueSize: cfg.QueueSize,
	}
	if rt.clock == nil {
		rt.clock = &SystemClock{}
	}
	if rt.nonces == nil {
		rt.nonces = RandomNonce{}
	}
	if rt.clientOrderIDs == nil {
		rt.clientOrderIDs = &TimeCounterClientOrderID{}
	}
	if rt.http == nil {
		transport := http.DefaultTransport
		if t, ok := transport.(*http.Transport); ok {
			transport = t.Clone()
		}
		rt.http = &http.Client{Transport: transport}
	}
	// A redirect can replay a signed POST. Keep a private client and never follow one.
	httpClient := *rt.http
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	rt.http = &httpClient
	if rt.log == nil {
		rt.log = loggerFromEnv()
	}
	if rt.callTimeout <= 0 {
		rt.callTimeout = behavior.CallTimeout
	}
	if rt.httpAttemptTimeout <= 0 {
		rt.httpAttemptTimeout = behavior.HTTPAttemptTimeout
	}
	if rt.wsRequestTimeout <= 0 {
		rt.wsRequestTimeout = behavior.WSRequestTimeout
	}
	if rt.queueSize == 0 {
		rt.queueSize = behavior.QueueSize
	}
	rt.sessionRetryDelay = behavior.SessionRetryDelay
	rt.hooks = &hooks{beforeSend: cfg.BeforeSend, onEvent: cfg.OnEvent, log: rt.log}
	rt.session = &session{apiKey: cfg.APIKey, loginURL: rt.env.EdgeURL + LoginPath, http: rt.http,
		log: func(msg string, args ...any) { rt.log.Debug(msg, args...) }}
	rt.domain = ExchangeDomain(rt.env.ChainID)
	rt.ctx, rt.cancel = context.WithCancel(context.Background())
	rt.session.owner = rt.ctx
	rt.session.attemptTimeout = rt.httpAttemptTimeout
	rt.metadata.owner = rt.ctx
	rt.metadata.loadTimeout = rt.callTimeout
	rt.md = newSocket(rt, "md", mdURL)
	rt.td = newSocket(rt, "td", tdURL)
	if cfg.ResyncTimeout > 0 {
		rt.md.SetResyncTimeout(cfg.ResyncTimeout)
		rt.td.SetResyncTimeout(cfg.ResyncTimeout)
	}
	if cfg.IdleTimeout > 0 {
		rt.md.idleTimeout, rt.td.idleTimeout = cfg.IdleTimeout, cfg.IdleTimeout
	}
	return rt, nil
}

// Encoding is the JSON encoding of this runtime's gateway calls and streams.
func (rt *Runtime) Encoding() Encoding {
	if rt.lite {
		return EncodingLite
	}
	return EncodingFull
}

// Env is the environment this runtime talks to.
func (rt *Runtime) Env() Environment { return rt.env }

// Signer is the configured signer (nil if none).
func (rt *Runtime) Signer() Signer { return rt.signer }

// Metadata is the signing metadata cache (instrument hashes and decimals, currency ids).
func (rt *Runtime) Metadata() *MetadataCache { return &rt.metadata }

// Domain is the EIP-712 domain for this environment's chain id.
func (rt *Runtime) Domain() *Domain { return rt.domain }

// Session is the current login (zero before the first authenticated call).
func (rt *Runtime) Session() SessionState { return rt.session.snapshot() }

// Socket is the managed socket for a gateway (market data, or trading for everything else).
func (rt *Runtime) Socket(g Gateway) *Socket {
	if g == GatewayMarketData {
		return rt.md
	}
	return rt.td
}

// Close closes both sockets and stops background work. Pending writes become
// *OrderStateUnknownError.
func (rt *Runtime) Close() error {
	if rt.closed.Swap(true) {
		return nil
	}
	rt.cancel()
	var wg sync.WaitGroup
	for _, s := range []*Socket{rt.md, rt.td} {
		wg.Add(1)
		go func(s *Socket) {
			defer wg.Done()
			s.close()
		}(s)
	}
	wg.Wait()
	return nil
}

// -- RPC ------------------------------------------------------------------------------------------

// RequestInfo is what generated code knows about a request type.
type RequestInfo[Req any] struct {
	DefaultAccount func(*Req, uint64)
	// Missing returns the first required field that is not set ("" when all are).
	Missing func(*Req) string
	// ClientOrderID (order writes only) sets order.metadata.client_order_id when absent and returns it.
	ClientOrderID func(*Req, ClientOrderIDProvider) uint64
	// ClientOrderIDs (writes carrying a list of orders, e.g. bulk orders) sets each order's
	// metadata.client_order_id when absent and returns them all, in list order.
	ClientOrderIDs func(*Req, ClientOrderIDProvider) []uint64
}

// SignFunc is a generated sign function: it fills the request's signature in place.
type SignFunc[Req any] func(ctx context.Context, rt *Runtime, req *Req) error

// Call runs one RPC: required-field check -> client order id -> BeforeSend -> sign -> encode ->
// REST or WS -> decode. With EncodingLite the encoded request is renamed to lite just before it is
// sent and the reply decodes from lite; everything before (and the model returned) is full. Signed requests are copied first, so the caller's request is never changed.
func Call[Req, Resp any](ctx context.Context, rt *Runtime, spec *RPCSpec, req *Req, info *RequestInfo[Req], sign SignFunc[Req]) (*Resp, error) {
	if rt.closed.Load() {
		return nil, ErrClosed
	}
	ctx, cancel := context.WithTimeout(ctx, rt.callTimeout)
	defer cancel()
	if deadlineErr := deadlineError(ctx); deadlineErr != nil {
		return nil, deadlineErr
	}
	if req == nil {
		req = new(Req)
	}
	if info == nil {
		info = &RequestInfo[Req]{}
	}
	if sign != nil || info.ClientOrderID != nil || info.ClientOrderIDs != nil || info.DefaultAccount != nil {
		c, err := cloneJSON(req)
		if err != nil {
			return nil, &ConfigError{Message: fmt.Sprintf("%s: cannot copy request: %v", spec.Method, err)}
		}
		req = c
	}
	if info.DefaultAccount != nil {
		info.DefaultAccount(req, rt.subAccountID)
	}
	if info.Missing != nil {
		if f := info.Missing(req); f != "" {
			return nil, missingField(spec.Method, f)
		}
	}
	// Fill each order's client order id when absent and return them all. On a request that already has
	// them, it only reads them.
	orderIDs := func(req *Req) []uint64 {
		if info.ClientOrderIDs != nil {
			return info.ClientOrderIDs(req, rt.clientOrderIDs)
		}
		if info.ClientOrderID != nil {
			if id := info.ClientOrderID(req, rt.clientOrderIDs); id != 0 {
				return []uint64{id}
			}
		}
		return nil
	}
	clientOrderIDs := orderIDs(req)
	via := ViaREST
	if spec.OrderEntry && rt.orderTransport == OrderTransportWS {
		via = ViaWS
	}
	if rt.hooks.beforeSend != nil {
		call := &OutgoingCall{RPC: spec.Method, Params: req, Transport: via}
		rt.hooks.beforeSend(call)
		p, ok := call.Params.(*Req)
		if !ok || p == nil {
			return nil, &ConfigError{Message: fmt.Sprintf("%s: BeforeSend replaced Params with %T, want %T", spec.Method, call.Params, req)}
		}
		req = p
		// The IDs are how an unknown outcome is looked up, so they must be the ones the caller knows.
		// An id the hook removed is filled again with a new one, which fails here too.
		if !slices.Equal(orderIDs(req), clientOrderIDs) {
			return nil, &ConfigError{Message: fmt.Sprintf("%s: BeforeSend must not add, remove or reorder orders, or change their client order IDs", spec.Method)}
		}
	}
	if deadlineErr := deadlineError(ctx); deadlineErr != nil {
		return nil, deadlineErr
	}
	if sign != nil {
		if err := sign(ctx, rt, req); err != nil { // nothing may change the request after this
			return nil, err
		}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, &ConfigError{Message: fmt.Sprintf("%s: cannot encode request: %v", spec.Method, err)}
	}
	lite := rt.wireLite(spec)
	wire := body
	if lite {
		if wire, err = wirejson.ToLite[Req](body); err != nil {
			return nil, &ConfigError{Message: fmt.Sprintf("%s: cannot encode request as lite: %v", spec.Method, err)}
		}
	}
	if spec.Auth {
		if err = rt.ensureSession(ctx); err != nil {
			return nil, err
		}
	}
	if deadlineErr := deadlineError(ctx); deadlineErr != nil {
		return nil, deadlineErr
	}
	var raw []byte
	if via == ViaWS {
		raw, err = rt.td.request(ctx, spec.Method, wire, spec.Write, clientOrderIDs)
	} else {
		raw, err = rt.restCall(ctx, spec, wire, clientOrderIDs)
	}
	if err != nil {
		var apiErr *APIError
		var unknown *OrderStateUnknownError
		if spec.Write && !errors.As(err, &unknown) && errors.As(err, &apiErr) && rt.errors.uncertain(apiErr) {
			return nil, unknownWrite(spec, err, clientOrderIDs)
		}
		// An uncertain response's cause is diagnostic, not a trustworthy metadata rejection.
		if !errors.As(err, &unknown) && errors.As(err, &apiErr) && slices.Contains(spec.RefreshMetadataOn, apiErr.Code) {
			for _, name := range legInstruments(body, "legs", "instrument") {
				rt.metadata.DropInstrument(name)
			}
		}
		return nil, err
	}

	resp := new(Resp)
	decodeErr := error(nil)
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		decodeErr = fmt.Errorf("expected a response object")
	} else if lite {
		decodeErr = wirejson.UnmarshalLite(raw, resp)
	} else {
		decodeErr = decodeJSON(raw, resp)
	}
	if err := decodeErr; err != nil {
		decodeErr := &DecodeError{RPC: spec.Method, Err: err}
		if spec.Write {
			return nil, unknownWrite(spec, decodeErr, clientOrderIDs)
		}
		return nil, decodeErr
	}
	return resp, nil
}

// decodeJSON is json.Unmarshal with the generated one-pass decoder of T when it has one: same
// accept/reject rules and values, one read of the text.
func decodeJSON[T any](data []byte, v *T) error { return wirejson.Unmarshal(data, v) }

// decodeWire decodes a gateway reply or frame in the runtime's encoding.
func decodeWire[T any](rt *Runtime, data []byte, v *T) error {
	if rt.lite {
		return wirejson.UnmarshalLite(data, v)
	}
	return wirejson.Unmarshal(data, v)
}

// wireLite reports whether spec's call goes out lite: the client is lite and the route has a lite
// form (edge routes do not).
func (rt *Runtime) wireLite(spec *RPCSpec) bool { return rt.lite && spec.LitePath != "" }

func cloneJSON[T any](v *T) (*T, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	out := new(T)
	return out, json.Unmarshal(data, out)
}

// -- streams --------------------------------------------------------------------------------------

// Subscribe subscribes to a stream (waits for the server's ack) and returns the subscription. Frames
// are decoded into F when the consumer reads them.
func Subscribe[F any](ctx context.Context, rt *Runtime, spec *StreamSpec, selector string) (*Subscription[F], error) {
	if rt.closed.Load() {
		return nil, ErrClosed
	}
	if spec.Auth {
		if err := rt.ensureSession(ctx); err != nil {
			return nil, err
		}
	}
	sock := rt.Socket(spec.Gateway)
	sb, err := sock.subscribe(ctx, spec, selector, rt.queueSize)
	if err != nil {
		return nil, err
	}
	return &Subscription[F]{sb: sb, sock: sock}, nil
}

// Selector joins the set fields as "p1-p2@s1-s2" (empty values skipped).
func Selector(primary, secondary []string) string {
	join := func(xs []string) string {
		var out []string
		for _, x := range xs {
			if x != "" {
				out = append(out, x)
			}
		}
		return strings.Join(out, "-")
	}
	p, s := join(primary), join(secondary)
	if s == "" {
		return p
	}
	return p + "@" + s
}

// SelectorInt formats an optional integer selector field ("" when nil).
func SelectorInt[T ~uint8 | ~uint16 | ~uint32 | ~uint64](v *T) string {
	if v == nil {
		return ""
	}
	return strconv.FormatUint(uint64(*v), 10)
}

// -- signing --------------------------------------------------------------------------------------

// PrepareSignature fills signer, nonce, expiration and chain_id when absent (expiration 0 and an
// empty signer count as absent).
func (rt *Runtime) PrepareSignature(signer *string, nonce **uint32, expiration **time.Time, chainID **uint64, ttlNs int64) error {
	if rt.signer == nil {
		return &ConfigError{Message: "this call needs a signer: set Config.Signer"}
	}
	if *signer == "" {
		*signer = rt.signer.Address()
	}
	if *nonce == nil {
		n := rt.nonces.NextNonce()
		*nonce = &n
	}
	if *expiration == nil || (*expiration).IsZero() || (*expiration).Equal(time.Unix(0, 0)) {
		e := time.Unix(0, rt.clock.NowNs()+ttlNs).UTC()
		*expiration = &e
	}
	if *chainID == nil {
		c := rt.env.ChainID
		*chainID = &c
	}
	return nil
}

// Sign signs with the Grvt domain for chainID (nil or 0: this environment's chain id).
func (rt *Runtime) Sign(ctx context.Context, chainID *uint64, st *StructType, message Value) (Signature, error) {
	if rt.signer == nil {
		return Signature{}, &ConfigError{Message: "this call needs a signer: set Config.Signer"}
	}
	d := rt.domain
	if chainID != nil && *chainID != 0 && *chainID != rt.env.ChainID {
		d = ExchangeDomain(*chainID)
	}
	if _, local := rt.signer.(*PrivateKeySigner); local {
		return rt.signer.SignTyped(ctx, d, st, message)
	}
	// External signers (wallet, KMS) sign the typed data themselves: check the result against our
	// own digest before it goes on the wire.
	digest, err := st.Digest(d, message)
	if err != nil {
		return Signature{}, err
	}
	sig, err := rt.signer.SignTyped(ctx, d, st, message)
	if err != nil {
		return Signature{}, err
	}
	if err := verifySignature(digest, sig, rt.signer.Address()); err != nil {
		return Signature{}, err
	}
	return sig, nil
}

// -- session --------------------------------------------------------------------------------------

func (rt *Runtime) ensureSession(ctx context.Context) error {
	if !rt.session.enabled() {
		return &ConfigError{Message: "this call needs a session: set Config.APIKey"}
	}
	if err := rt.session.ensure(ctx); err != nil {
		return err
	}
	rt.rotation.Do(func() { go rt.rotateForever() })
	return nil
}

// rotateForever re-logs in at 80% of the cookie lifetime. Sockets notice the new generation and
// reconnect when no write is in flight. Each wait is until the renewal is due, but at least
// SessionRenewMinWait, or sessionRetryDelay after a failed login (behavior.json).
func (rt *Runtime) rotateForever() {
	floor := behavior.SessionRenewMinWait
	for {
		t := time.NewTimer(max(time.Until(rt.session.snapshot().RefreshDue()), floor))
		select {
		case <-rt.ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		if err := rt.session.login(rt.ctx); err != nil {
			if rt.ctx.Err() != nil {
				return
			}
			rt.log.Warn("grvt: session re-login failed", "err", err)
			rt.hooks.emit(func() Event { return Event{Kind: EventError, Fatal: false, Err: "session: " + err.Error()} })
			floor = rt.sessionRetryDelay
			continue
		}
		floor = behavior.SessionRenewMinWait
		s := rt.session.snapshot()
		rt.hooks.emit(func() Event {
			return Event{Kind: EventSessionRotated, WebsocketReconnectRequired: true, Generation: s.Generation, ExpiresAt: s.ExpiresAt}
		})
	}
}

// SyncClock reads GET {market data}/time ({"server_time": ms}) and feeds it to clock.
func (rt *Runtime) SyncClock(ctx context.Context, clock *SystemClock) error {
	ctx, cancel := context.WithTimeout(ctx, rt.httpAttemptTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rt.env.MarketDataURL+"/time", nil)
	if err != nil {
		return &ConfigError{Message: err.Error()}
	}
	t0 := time.Now()
	resp, err := rt.http.Do(req)
	if err != nil {
		return &TransportError{Message: err.Error(), RPC: "time", Err: err}
	}
	defer resp.Body.Close()
	var body struct {
		ServerTime json.Number `json:"server_time"`
	}
	if resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&body) == nil {
		if ms, err := strconv.ParseInt(body.ServerTime.String(), 10, 64); err == nil {
			clock.Observe(ms, time.Since(t0))
		}
	}
	return nil
}

// Check elapsed time as well as timer delivery after synchronous user code.
func deadlineError(ctx context.Context) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}
