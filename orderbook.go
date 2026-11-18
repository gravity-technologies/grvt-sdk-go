package grvt

import (
	"fmt"
	"math/big"
	"slices"
	"sync"
	"time"

	"github.com/gravity-technologies/grvt-sdk-go/models"
)

type BookMode string

const (
	BookDelta     BookMode = "delta"
	BookSnapshots BookMode = "snapshot"
)

type BookError struct {
	Reason string
	Gap    *GapRange
}

func (e *BookError) Error() string {
	return fmt.Sprintf("order book: %s; reset from a fresh subscription snapshot", e.Reason)
}

type BookSnapshot struct {
	Instrument     string
	SequenceNumber uint64
	EventTime      time.Time
	Bids, Asks     []models.OrderbookLevel
}

// OrderBook builds one instrument's levels from an SDK subscription. Forward its
// gap events to OnEvent. In delta mode, Reset after a gap requires a fresh subscription's snapshot;
// an old queued snapshot must not restore readiness. Methods are safe across goroutines.
type OrderBook struct {
	mu                 sync.RWMutex
	instrument         string
	mode               BookMode
	ready, invalidated bool
	gap                *GapRange
	stream, selector   string
	sequence           uint64
	eventTime          time.Time
	bids, asks         map[string]models.OrderbookLevel
}

func NewOrderBook(instrument string, mode BookMode) (*OrderBook, error) {
	if mode != BookDelta && mode != BookSnapshots {
		return nil, &BookError{Reason: "invalid_mode"}
	}
	return &OrderBook{instrument: instrument, mode: mode, bids: map[string]models.OrderbookLevel{}, asks: map[string]models.OrderbookLevel{}}, nil
}
func (b *OrderBook) Ready() bool { b.mu.RLock(); defer b.mu.RUnlock(); return b.ready }
func (b *OrderBook) invalidate(gap *GapRange) {
	b.ready = false
	b.invalidated = true
	b.gap = copyGapRange(gap)
	clear(b.bids)
	clear(b.asks)
}
func (b *OrderBook) Invalidate(gap *GapRange) { b.mu.Lock(); defer b.mu.Unlock(); b.invalidate(gap) }
func (b *OrderBook) OnEvent(event Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.mode == BookDelta && event.Kind == EventGap && (b.stream == "" || (b.stream == event.Stream && b.selector == event.Selector)) {
		b.invalidate(event.Gap)
	}
}
func (b *OrderBook) fail(reason string, gap *GapRange) error {
	b.invalidate(gap)
	return &BookError{Reason: reason, Gap: copyGapRange(gap)}
}
func (b *OrderBook) Reset(frame models.WSOrderbookLevelsFeedDataV1) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.mode == BookDelta && frame.SequenceNumber != 0 {
		return b.fail("snapshot_required", nil)
	}
	b.invalidated = false
	b.ready = false
	return b.apply(frame)
}
func (b *OrderBook) Apply(frame models.WSOrderbookLevelsFeedDataV1) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.apply(frame)
}
func (b *OrderBook) apply(frame models.WSOrderbookLevelsFeedDataV1) error {
	if b.invalidated {
		return &BookError{Reason: "reset_required", Gap: copyGapRange(b.gap)}
	}
	if frame.Feed.Instrument != b.instrument || (b.stream != "" && (frame.Stream != b.stream || frame.Selector != b.selector)) {
		return b.fail("wrong_stream", nil)
	}
	if b.ready && (b.mode != BookSnapshots || frame.SequenceNumber != 0) && frame.SequenceNumber <= b.sequence {
		return b.fail("out_of_order", nil)
	}
	snapshot := b.mode == BookSnapshots || frame.SequenceNumber == 0
	if !snapshot {
		if !b.ready {
			return &BookError{Reason: "not_ready", Gap: copyGapRange(b.gap)}
		}
		if b.sequence != 0 && frame.PrevSequenceNumber != b.sequence {
			return b.fail("gap", &GapRange{AfterSequence: Ptr(b.sequence), Reason: "sequence_mismatch"})
		}
		if frame.SequenceNumber <= b.sequence || frame.SequenceNumber <= frame.PrevSequenceNumber {
			return b.fail("out_of_order", nil)
		}
	}
	for _, levels := range [][]models.OrderbookLevel{frame.Feed.Bids, frame.Feed.Asks} {
		for _, level := range levels {
			if level.Price.Rat().Sign() <= 0 || level.Size.Rat().Sign() < 0 {
				return b.fail("invalid_level", nil)
			}
		}
	}
	if snapshot {
		clear(b.bids)
		clear(b.asks)
	}
	update := func(levels []models.OrderbookLevel, side map[string]models.OrderbookLevel) {
		for _, level := range levels {
			key := level.Price.Rat().RatString()
			if level.Size.Rat().Sign() == 0 {
				delete(side, key)
			} else {
				side[key] = level
			}
		}
	}
	update(frame.Feed.Bids, b.bids)
	update(frame.Feed.Asks, b.asks)
	b.stream = frame.Stream
	b.selector = frame.Selector
	b.sequence = frame.SequenceNumber
	b.eventTime = frame.Feed.EventTime
	b.ready = true
	b.gap = nil
	return nil
}
func (b *OrderBook) Snapshot() (BookSnapshot, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if !b.ready {
		return BookSnapshot{}, &BookError{Reason: "not_ready", Gap: copyGapRange(b.gap)}
	}
	sorted := func(side map[string]models.OrderbookLevel, direction int) []models.OrderbookLevel {
		type pricedLevel struct {
			price *big.Rat
			level models.OrderbookLevel
		}
		levels := make([]pricedLevel, 0, len(side))
		for _, level := range side {
			levels = append(levels, pricedLevel{level.Price.Rat(), level})
		}
		slices.SortFunc(levels, func(a, c pricedLevel) int { return direction * a.price.Cmp(c.price) })
		out := make([]models.OrderbookLevel, len(levels))
		for i, level := range levels {
			out[i] = level.level
		}
		return out
	}
	return BookSnapshot{Instrument: b.instrument, SequenceNumber: b.sequence, EventTime: b.eventTime, Bids: sorted(b.bids, -1), Asks: sorted(b.asks, 1)}, nil
}
