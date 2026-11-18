package grvt

import (
	"math/rand/v2"
	"strconv"
	"time"
)

// WebSocket protocol state as plain types: no sockets, no timers, no goroutines.
//
//   - requestTable: JSON-RPC uint32 id -> pending request
//   - Sequencer: per-subscription sequence tracking and gap detection
//   - Backoff: full-jitter delays
//
// Server behaviour this follows:
//   - The subscribe reply comes first, then snapshot frames (sequence_number 0), then buffered deltas
//     newer than latest_sequence_number, then live deltas, all in order on one connection.
//   - Every delta carries prev_sequence_number; with global sequence numbers the next delta's prev
//     equals the previous delta's number, so a mismatch is a gap.
//   - Subscribing again to the same primary selector replaces the old one and sends a fresh snapshot.
//   - Seen on testnet: after a re-subscribe on the same socket, the first delta's prev_sequence_number
//     is the OLD last sequence number, not the reply's latest_sequence_number. So the first sequenced
//     frame after a (re)subscribe reply is accepted when its prev is 0 or not greater than the latest
//     sequence number known then; after that, prev must equal the last sequence number seen, otherwise
//     it is a gap.

type pending[W any] struct {
	method string
	write  bool
	waiter W
}

// requestTable hands out uint32 ids; 0 is never used (the gateway omits id 0 in replies).
type requestTable[W any] struct {
	next    uint32
	pending map[uint32]*pending[W]
}

func newRequestTable[W any](start uint32) *requestTable[W] {
	return &requestTable[W]{next: max(start, 1), pending: map[uint32]*pending[W]{}}
}

// add stores p under a fresh id; false only when all 2^32-1 ids are in use.
func (t *requestTable[W]) add(p *pending[W]) (uint32, bool) {
	for i := 0; i < 1<<32-1; i++ {
		id := t.next
		if t.next == 1<<32-1 {
			t.next = 1
		} else {
			t.next++
		}
		if _, used := t.pending[id]; !used {
			t.pending[id] = p
			return id, true
		}
	}
	return 0, false
}

func (t *requestTable[W]) pop(id uint32) *pending[W] {
	p := t.pending[id]
	delete(t.pending, id)
	return p
}

func (t *requestTable[W]) drain() []*pending[W] {
	out := make([]*pending[W], 0, len(t.pending))
	for id, p := range t.pending {
		out = append(out, p)
		delete(t.pending, id)
	}
	return out
}

func (t *requestTable[W]) writesInFlight() int {
	n := 0
	for _, p := range t.pending {
		if p.write {
			n++
		}
	}
	return n
}

// Verdict is what to do with one stream frame.
type Verdict int

const (
	Deliver Verdict = iota
	// Drop: a duplicate, or a frame from before the current subscribe reply.
	Drop
	Gap
)

// Sequencer tracks one subscription's sequence numbers.
type Sequencer struct {
	live          bool
	firstDelta    bool
	Last          uint64
	SnapshotsLeft uint64
	Gaps          uint64
	// ResyncStarted is set when the subscription re-subscribes after a gap; cleared by the snapshot.
	ResyncStarted time.Time
}

// Reset drops every frame until the next subscribe reply.
func (s *Sequencer) Reset() {
	s.live, s.firstDelta, s.Last, s.SnapshotsLeft = false, false, 0, 0
}

// OnAck records a subscribe reply.
func (s *Sequencer) OnAck(latest, numSnapshots uint64) {
	s.live, s.firstDelta = true, true
	s.Last, s.SnapshotsLeft = latest, numSnapshots
	if numSnapshots == 0 {
		s.ResyncStarted = time.Time{}
	}
}

// OnFrame judges one frame. seq 0 (or absent) is a snapshot or an unsequenced frame; hasPrev says
// whether the frame carried prev_sequence_number.
func (s *Sequencer) OnFrame(seq, prev uint64, hasPrev bool) Verdict {
	if !s.live {
		return Drop
	}
	if seq == 0 {
		if s.SnapshotsLeft > 0 {
			s.SnapshotsLeft--
			if s.SnapshotsLeft == 0 {
				s.ResyncStarted = time.Time{}
			}
		}
		return Deliver
	}
	if seq <= s.Last {
		return Drop
	}
	first := s.firstDelta
	s.firstDelta = false
	switch {
	case !hasPrev || prev == s.Last:
	case first && prev < s.Last:
		// re-subscribe quirk: prev is the number from before the snapshot; the snapshot covers it
	default:
		s.Gaps++
		return Gap
	}
	s.Last = seq
	return Deliver
}

// ResyncOverdue reports whether a re-subscribe has waited longer than limit for its snapshot.
func (s *Sequencer) ResyncOverdue(now time.Time, limit time.Duration) bool {
	return !s.ResyncStarted.IsZero() && now.Sub(s.ResyncStarted) > limit
}

// Backoff is full jitter: sleep uniform(0, min(cap, base * 2^attempt)).
type Backoff struct {
	Base, Cap time.Duration
	Attempt   int
	rand      func() float64
}

func newBackoff(base, ceiling time.Duration) *Backoff {
	return &Backoff{Base: base, Cap: ceiling, rand: rand.Float64}
}

func (b *Backoff) Next() time.Duration {
	ceiling := b.Cap
	if b.Attempt < 30 && b.Base<<b.Attempt < b.Cap && b.Base<<b.Attempt > 0 {
		ceiling = b.Base << b.Attempt
	}
	b.Attempt = min(b.Attempt+1, 30)
	return time.Duration(float64(ceiling) * b.rand())
}

func (b *Backoff) Reset() { b.Attempt = 0 }

// jsonrpcFrame builds the request text without re-encoding already-encoded params.
func jsonrpcFrame(method string, params []byte, id uint32) []byte {
	return appendFrame(&fullFrame, method, params, id)
}

// liteJSONRPCFrame is jsonrpcFrame in the lite encoding (params must already be lite).
func liteJSONRPCFrame(method string, params []byte, id uint32) []byte {
	return appendFrame(&liteFrame, method, params, id)
}

// frameKeys are the fixed pieces of a request frame in one encoding. Methods are plain ASCII and
// never need escaping.
type frameKeys struct{ head, params, id string }

var (
	fullFrame = frameKeys{`{"jsonrpc":"2.0","method":"`, `","params":`, `,"id":`}
	liteFrame = frameKeys{`{"` + liteRequestJSONRPC + `":"2.0","` + liteRequestMethod + `":"`,
		`","` + liteRequestParams + `":`, `,"` + liteRequestID + `":`}
)

func appendFrame(k *frameKeys, method string, params []byte, id uint32) []byte {
	out := make([]byte, 0, len(params)+64)
	out = append(out, k.head...)
	out = append(out, method...)
	out = append(out, k.params...)
	out = append(out, params...)
	out = append(out, k.id...)
	out = strconv.AppendUint(out, uint64(id), 10)
	return append(out, '}')
}
