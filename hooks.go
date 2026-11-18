package grvt

import (
	"log/slog"
	"time"
)

// Via is how an RPC travels.
type Via string

const (
	ViaREST Via = "rest"
	ViaWS   Via = "ws"
)

// OutgoingCall is one RPC as Config.BeforeSend sees it. The hook runs before signing, so nothing can
// change a request after it is signed.
type OutgoingCall struct {
	// RPC is the method, e.g. "v1/create_order".
	RPC string
	// Params is the request: a pointer to the generated request model (e.g.
	// *models.ApiCreateOrderRequest). Change it in place. For signed calls it is a copy of the
	// caller's request.
	Params    any
	Transport Via
}

// EventKind names an Event.
type EventKind string

const (
	EventConnect        EventKind = "connect"
	EventReconnect      EventKind = "reconnect"
	EventDisconnect     EventKind = "disconnect"
	EventSubscribe      EventKind = "subscribe"
	EventGap            EventKind = "gap"
	EventRetry          EventKind = "retry"
	EventFrame          EventKind = "frame"
	EventSessionRotated EventKind = "session_rotated"
	EventError          EventKind = "error"
)

// GapRange bounds potentially missed sequences: exclusive start, inclusive end.
// A nil bound is unknown. An open end includes re-subscription; REST history
// cannot use these sequence numbers as resume cursors.
type GapRange struct {
	AfterSequence   *uint64
	ThroughSequence *uint64
	Reason          string
}

// Event is what Config.OnEvent receives. Only the fields that apply to Kind are set.
type Event struct {
	Kind EventKind
	Gap  *GapRange
	// Socket is "md" (market data) or "td" (trading); empty for REST and session events.
	Socket string

	// Disconnect: the close code (0 if none) and why.
	Code   int
	Reason string

	// Subscribe and Gap.
	Stream   string
	Selector string
	// Subscribe: the server's latest sequence number and how many snapshot frames follow.
	LatestSequenceNumber uint64
	NumSnapshots         uint64
	// Sequence-mismatch gaps only: expected prev and the frame's prev / sequence.
	// For disconnect and overflow these zero values are not sequence bounds; use Gap.
	ExpectedPrev uint64
	Prev         uint64
	Seq          uint64

	// Retry: a REST read retried after Delay, or a socket reconnect after a failed connect.
	RPC     string
	Attempt int
	Delay   time.Duration

	// Frame: raw bytes as sent (Outgoing) or received. NOT redacted. Read-only: a received frame is the
	// buffer the SDK decodes from.
	Raw      []byte
	Outgoing bool

	// SessionRotated: login completed; authenticated WebSockets still need to reconnect.
	WebsocketReconnectRequired bool
	Generation                 uint64
	ExpiresAt                  time.Time

	// Error and Retry: the error text. Fatal: the socket stopped for good.
	Err   string
	Fatal bool
}

type hooks struct {
	beforeSend func(*OutgoingCall)
	onEvent    func(Event)
	log        *slog.Logger
}

// emit calls OnEvent when set. Build the event inside build so nothing is allocated when unset. A
// panicking listener never breaks the runtime.
func (h *hooks) emit(build func() Event) {
	if h.onEvent == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			h.log.Error("grvt: on_event hook panicked", "panic", r)
		}
	}()
	h.onEvent(build())
}

func copyGapRange(gap *GapRange) *GapRange {
	if gap == nil {
		return nil
	}
	out := *gap
	if gap.AfterSequence != nil {
		out.AfterSequence = Ptr(*gap.AfterSequence)
	}
	if gap.ThroughSequence != nil {
		out.ThroughSequence = Ptr(*gap.ThroughSequence)
	}
	return &out
}
