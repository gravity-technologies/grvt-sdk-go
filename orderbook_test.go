package grvt

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/gravity-technologies/grvt-sdk-go/models"
)

func TestSharedOrderBookScenarios(t *testing.T) {
	data, err := ReadShared("scenarios/orderbook.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name  string
		Mode  BookMode
		Steps []struct {
			Action, Error, Reason string
			Frame                 models.WSOrderbookLevelsFeedDataV1
			Bids, Asks            []string
		}
	}
	if err = json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range cases {
		t.Run(scenario.Name, func(t *testing.T) {
			book, err := NewOrderBook("X", scenario.Mode)
			if err != nil {
				t.Fatal(err)
			}
			for _, step := range scenario.Steps {
				if step.Action == "gap" {
					book.OnEvent(Event{Kind: EventGap, Stream: "book.d", Selector: "X@100", Gap: &GapRange{Reason: step.Reason}})
					continue
				}
				if step.Action == "invalidate" {
					book.Invalidate(nil)
					continue
				}
				if step.Action == "reset" {
					err = book.Reset(step.Frame)
				} else {
					err = book.Apply(step.Frame)
				}
				if step.Error != "" {
					var failure *BookError
					if !errors.As(err, &failure) || failure.Reason != step.Error {
						t.Fatalf("want %s got %v", step.Error, err)
					}
					if step.Error == "gap" && (failure.Gap == nil || failure.Gap.AfterSequence == nil || *failure.Gap.AfterSequence != 101 || failure.Gap.ThroughSequence != nil) {
						t.Fatalf("wrong gap: %+v", failure.Gap)
					}
					if book.Ready() {
						t.Fatal("invalid book is ready")
					}
					if _, snapshotErr := book.Snapshot(); snapshotErr == nil {
						t.Fatal("invalid snapshot readable")
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					snapshot, err := book.Snapshot()
					if err != nil {
						t.Fatal(err)
					}
					prices := func(levels []models.OrderbookLevel) []string {
						out := []string{}
						for _, v := range levels {
							out = append(out, v.Price.String())
						}
						return out
					}
					if !reflect.DeepEqual(prices(snapshot.Bids), step.Bids) || !reflect.DeepEqual(prices(snapshot.Asks), step.Asks) {
						t.Fatalf("unexpected book: %+v", snapshot)
					}
					if snapshot.EventTime.UnixNano() != 1793172225200080949 {
						t.Fatal("lost timestamp precision")
					}
					if len(step.Frame.Feed.Bids) > 0 {
						step.Frame.Feed.Bids[0].NumOrders = 999
					}
					if len(snapshot.Bids) > 0 {
						snapshot.Bids[0].NumOrders = 888
						next, _ := book.Snapshot()
						if next.Bids[0].NumOrders != 1 {
							t.Fatal("aliased levels")
						}
					}
				}
			}
		})
	}
}

func TestOrderBookRejectsUnrepresentableLevels(t *testing.T) {
	one := models.MustParseDecimal("1")
	tiny := models.MustParseDecimal("0." + strings.Repeat("0", 1_000_000) + "1")
	for _, level := range []models.OrderbookLevel{
		{Size: tiny}, {Price: one, Size: tiny}, {Price: tiny, Size: one},
	} {
		book, _ := NewOrderBook("X", BookDelta)
		frame := models.WSOrderbookLevelsFeedDataV1{Feed: models.OrderbookLevels{
			Instrument: "X", Bids: []models.OrderbookLevel{{Price: one, Size: one}},
		}}
		if err := book.Apply(frame); err != nil {
			t.Fatal(err)
		}
		frame.SequenceNumber = 1
		frame.Feed.Asks = []models.OrderbookLevel{level}
		var failure *BookError
		if err := book.Apply(frame); !errors.As(err, &failure) || failure.Reason != "invalid_level" {
			t.Fatalf("want invalid_level, got %v", err)
		}
		if _, err := book.Snapshot(); err == nil {
			t.Fatal("invalid book still readable")
		}
	}
}

func TestOrderBookGapEvent(t *testing.T) {
	book, _ := NewOrderBook("X", BookDelta)
	gap := &GapRange{AfterSequence: Ptr(uint64(10)), Reason: "disconnect"}
	event := Event{Kind: EventGap, Stream: "book.d", Selector: "X@100", Gap: gap}
	book.OnEvent(event)
	frame := models.WSOrderbookLevelsFeedDataV1{Stream: "book.d", Selector: "X@100", Feed: models.OrderbookLevels{Instrument: "X"}}
	if err := book.Apply(frame); err == nil {
		t.Fatal("early gap accepted queued snapshot")
	}
	if err := book.Reset(frame); err != nil {
		t.Fatal(err)
	}
	event.Selector = "Y@100"
	book.OnEvent(event)
	if !book.Ready() {
		t.Fatal("unrelated stream invalidated book")
	}
	event.Selector = "X@100"
	book.OnEvent(event)
	if book.Ready() {
		t.Fatal("gap did not invalidate book")
	}
}

func BenchmarkOrderBookSnapshot(b *testing.B) {
	book, _ := NewOrderBook("X", BookSnapshots)
	frame := models.WSOrderbookLevelsFeedDataV1{Feed: models.OrderbookLevels{Instrument: "X"}}
	for i := 1; i <= 1000; i++ {
		frame.Feed.Bids = append(frame.Feed.Bids, models.OrderbookLevel{Price: models.MustParseDecimal(fmt.Sprint(i)), Size: models.MustParseDecimal("1")})
	}
	if err := book.Apply(frame); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := book.Snapshot(); err != nil {
			b.Fatal(err)
		}
	}
}
