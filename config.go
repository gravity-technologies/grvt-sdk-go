package grvt

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// OrderTransport is how order-entry writes travel. Chosen once per client.
type OrderTransport int

const (
	// OrderTransportREST sends orders as HTTP POSTs (the default).
	OrderTransportREST OrderTransport = iota
	// OrderTransportWS sends orders over the trading WebSocket (lower latency). A drop or timeout is
	// *OrderStateUnknownError.
	OrderTransportWS
)

// Encoding is the JSON encoding of every gateway call and stream. Chosen once per client.
type Encoding int

const (
	// EncodingLite (the default) uses the gateways' short field names: the same API and values in
	// smaller messages (order book frames are about a third smaller). Only the wire changes; models,
	// errors and signing stay the same. Login and the server-time call stay full.
	EncodingLite Encoding = iota
	// EncodingFull uses the full JSON field names, as shown in the API reference.
	EncodingFull
)

func (e Encoding) String() string {
	switch e {
	case EncodingFull:
		return "full"
	case EncodingLite:
		return "lite"
	}
	return fmt.Sprintf("Encoding(%d)", int(e))
}

// Config holds the client options. Only Env is required.
//
//	grvt.New(grvt.Config{Env: grvt.Testnet(), APIKey: key, Signer: signer})
type Config struct {
	// SubAccountID fills omitted request and stream account IDs. Zero means unset.
	SubAccountID uint64
	// Env is where to connect: Testnet(), Production() or your own Environment.
	Env Environment
	// APIKey logs in on the first authenticated call (gravity cookie + X-Grvt-Account-Id).
	APIKey string
	// Signer signs orders, transfers, withdrawals and wallet logins.
	Signer Signer
	// OrderTransport: OrderTransportREST (default) or OrderTransportWS.
	OrderTransport OrderTransport
	// Encoding: EncodingLite (default) or EncodingFull, for both HTTP and WebSocket.
	Encoding Encoding
	// CheckOrders checks each order against its instrument's limits before signing (nil means on).
	// grvt.Ptr(false) skips the check and leaves the limits to the exchange.
	CheckOrders *bool

	// Clock, Nonces and ClientOrderIDs default to the wall clock, random nonces and
	// 1<<63|seconds<<31|counter ids. Tests pin them.
	Clock          Clock
	Nonces         NonceProvider
	ClientOrderIDs ClientOrderIDProvider

	// HTTPClient is the HTTP plug-in (default: a keep-alive client). A custom client Timeout
	// is an additional cap; HTTPAttemptTimeout and the caller deadline still apply.
	// The SDK copies client settings and disables redirects. Custom RoundTrippers
	// must not replay POSTs and must honor request contexts. Once handed to a custom
	// transport, a write with no trustworthy reply is conservatively uncertain.
	HTTPClient *http.Client

	// BeforeSend runs for every RPC before signing (REST or WS). It may change call.Params in place,
	// but must not add, remove or reorder orders, or change their client order IDs (prices, sizes and other fields may change): that is a ConfigError and nothing
	// is sent, since the IDs are how an unknown outcome is looked up.
	BeforeSend func(call *OutgoingCall)
	// OnEvent gets connection, stream and retry events. It runs on the socket's read goroutine and
	// must not block.
	OnEvent func(e Event)

	// CallTimeout bounds the whole RPC, including signing, login and retries (default 30s).
	CallTimeout time.Duration
	// HTTPAttemptTimeout bounds each HTTP attempt (default 10s).
	HTTPAttemptTimeout time.Duration
	// WSRequestTimeout bounds a WS request or subscribe (default 5s).
	WSRequestTimeout time.Duration
	// ResyncTimeout bounds the wait for a fresh snapshot after a gap re-subscribe; past it the socket
	// reconnects (default 5s).
	ResyncTimeout time.Duration
	// IdleTimeout: a WebSocket connection with no frame at all, not even a ping or pong, for this long
	// is dropped and reconnected (default 30s). The client pings every third of it.
	IdleTimeout time.Duration
	// QueueSize bounds each subscription's queue (default 4096; at least 1).
	QueueSize int
	// Logger receives debug logs (redacted). Default: GRVT_LOG=debug logs to stderr, otherwise nothing.
	Logger *slog.Logger
}

// Ptr returns a pointer to v, for optional model fields: Ptr(true), Ptr[uint64](123).
func Ptr[T any](v T) *T { return &v }

// Deref returns p, or a pointer to a zero T when p is nil. Generated signing code reads nested
// fields through it.
func Deref[T any](p *T) *T {
	if p == nil {
		return new(T)
	}
	return p
}

// String keeps the API key out of %v and %+v.
func (c Config) String() string {
	key := ""
	if c.APIKey != "" {
		key = redacted
	}
	return fmt.Sprintf("{Env:%s APIKey:%s Signer:%t OrderTransport:%d Encoding:%s}", c.Env.Name, key, c.Signer != nil, c.OrderTransport, c.Encoding)
}

// GoString keeps the API key out of %#v.
func (c Config) GoString() string { return "grvt.Config" + c.String() }

// LogValue keeps the API key out of slog output.
func (c Config) LogValue() slog.Value { return slog.StringValue(c.String()) }
