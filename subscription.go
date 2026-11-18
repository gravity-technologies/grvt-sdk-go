package grvt

import (
	"context"
	"errors"
	"iter"
	"sync"
)

// sub is one subscription's state, shared by the socket (producer) and the Subscription (consumer).
type sub struct {
	id       uint64
	spec     *StreamSpec
	selector string
	params   []byte // subscribe/unsubscribe params
	capacity int
	notify   chan struct{} // wakes the consumer; buffered 1

	mu        sync.Mutex
	ack       chan error // the waiting subscribe call (buffered 1); nil once answered
	queue     [][]byte
	err       error
	closed    bool
	seq       Sequencer
	key       *[2]string // stream, selector as the server echoes it
	sentEpoch uint64
	dropped   uint64
}

func (sb *sub) wake() {
	select {
	case sb.notify <- struct{}{}:
	default:
	}
}

// pushLocked queues one raw frame. It returns false when the consumer overflowed a stream that may not
// drop items (the queue is cleared and the consumer gets a StreamOverflowError).
func (sb *sub) pushLocked(raw []byte) bool {
	defer sb.wake()
	if len(sb.queue) >= sb.capacity {
		if !sb.spec.Conflate {
			sb.queue = nil
			sb.err = &StreamOverflowError{Stream: sb.spec.Stream, Selector: sb.selector, Capacity: sb.capacity}
			return false
		}
		sb.queue = sb.queue[1:]
		sb.dropped++
	}
	sb.queue = append(sb.queue, raw)
	return true
}

// takeAck returns the waiting subscribe call's channel the first time, nil after.
func (sb *sub) takeAck() chan error {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	ch := sb.ack
	sb.ack = nil
	return ch
}

// fail hands err to the waiting subscribe call, or to the consumer. end closes the subscription.
func (sb *sub) fail(err error, end bool) {
	if ch := sb.takeAck(); ch != nil {
		ch <- err // the waiting subscribe returns it
	} else {
		sb.mu.Lock()
		sb.err = err
		sb.mu.Unlock()
	}
	if end {
		sb.mu.Lock()
		sb.closed = true
		sb.mu.Unlock()
	}
	sb.wake()
}

func (sb *sub) end() {
	sb.mu.Lock()
	sb.closed = true
	sb.mu.Unlock()
	if ch := sb.takeAck(); ch != nil {
		ch <- ErrClosed
	}
	sb.wake()
}

func (sb *sub) next(ctx context.Context) ([]byte, error) {
	for {
		sb.mu.Lock()
		if sb.err != nil {
			err := sb.err
			sb.err = nil
			sb.mu.Unlock()
			return nil, err
		}
		if len(sb.queue) > 0 {
			raw := sb.queue[0]
			sb.queue[0] = nil
			sb.queue = sb.queue[1:]
			sb.mu.Unlock()
			return raw, nil
		}
		if sb.closed {
			sb.mu.Unlock()
			return nil, ErrClosed
		}
		sb.mu.Unlock()
		select {
		case <-sb.notify:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Subscription is a live stream subscription. Read it with All (a Go 1.23 iterator) or Next; Close
// unsubscribes.
//
// After a gap, an overflow or a reconnect the subscription re-syncs: the next frame is a fresh
// snapshot (sequence_number "0") when the stream sends snapshots.
type Subscription[F any] struct {
	sb   *sub
	sock *Socket
	once sync.Once
}

// Next waits for the next frame. Errors that do not end the subscription (StreamOverflowError, a
// failed re-subscribe, DecodeError) are returned once and the next call continues. After Close (or a
// fatal socket error, returned first) it returns ErrClosed.
func (s *Subscription[F]) Next(ctx context.Context) (*F, error) {
	raw, err := s.sb.next(ctx)
	if err != nil {
		return nil, err
	}
	f := new(F)
	if err := decodeWire(s.sock.rt, raw, f); err != nil {
		return nil, &DecodeError{RPC: s.sb.spec.Stream, Err: err}
	}
	return f, nil
}

// All yields frames until the subscription is closed:
//
//	for frame, err := range sub.All() { ... }
//
// A non-nil err is a StreamOverflowError, DecodeError or similar; the loop goes on after it. The loop
// ends after Close, or after a fatal error (which is yielded first). Breaking out of the loop does not
// unsubscribe; call Close.
func (s *Subscription[F]) All() iter.Seq2[*F, error] {
	return func(yield func(*F, error) bool) {
		for {
			f, err := s.Next(context.Background())
			if errors.Is(err, ErrClosed) {
				return
			}
			if !yield(f, err) {
				return
			}
		}
	}
}

// Close unsubscribes. Safe to call more than once.
func (s *Subscription[F]) Close() error {
	s.once.Do(func() {
		s.sb.mu.Lock()
		s.sb.closed = true
		s.sb.err = nil
		s.sb.mu.Unlock()
		s.sb.wake()
		s.sock.unsubscribe(s.sb, true)
	})
	return nil
}

// Stream is the channel, e.g. "v1.book.d".
func (s *Subscription[F]) Stream() string { return s.sb.spec.Stream }

// Selector is the selector sent, e.g. "BTC_USDT_Perp@500".
func (s *Subscription[F]) Selector() string { return s.sb.selector }

// Dropped counts frames dropped because the consumer was slow (conflating streams only).
func (s *Subscription[F]) Dropped() uint64 {
	s.sb.mu.Lock()
	defer s.sb.mu.Unlock()
	return s.sb.dropped
}
