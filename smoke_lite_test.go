package grvt_test

// Read-only testnet check of the lite encoding against full, unauthenticated market data only:
//
//	GRVT_SMOKE=1 go test -run TestSmokeTestnetLite -v .

import (
	"context"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	grvt "github.com/gravity-technologies/grvt-sdk-go"
	"github.com/gravity-technologies/grvt-sdk-go/models"
)

func TestSmokeTestnetLite(t *testing.T) {
	if os.Getenv("GRVT_SMOKE") != "1" {
		t.Skip("set GRVT_SMOKE=1 to run against testnet")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	type sized struct {
		client *grvt.Client
		book   atomic.Int64 // bytes of book frames
		frames atomic.Int64
	}
	open := func(enc grvt.Encoding) *sized {
		s := &sized{}
		c, err := grvt.New(grvt.Config{Env: grvt.Testnet(), Encoding: enc, OnEvent: func(e grvt.Event) {
			if e.Kind == grvt.EventFrame && len(e.Raw) > 200 {
				s.book.Add(int64(len(e.Raw)))
				s.frames.Add(1)
			}
		}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		s.client = c
		return s
	}
	full, lite := open(grvt.EncodingFull), open(grvt.EncodingLite)

	// HTTP: the same reply decoded both ways.
	all := func(c *grvt.Client) *models.ApiGetAllInstrumentsResponse {
		r, err := c.GetAllInstruments(ctx, &models.ApiGetAllInstrumentsRequest{IsActive: grvt.Ptr(true)})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	a, b := all(full.client), all(lite.client)
	if !reflect.DeepEqual(a, b) {
		a = all(full.client) // instruments rarely change; once more before failing
		if !reflect.DeepEqual(a, all(lite.client)) {
			t.Fatal("all_instruments differs between full and lite")
		}
	}
	if len(a.Result) == 0 || a.Result[0].TickSize.String() == "" || a.Result[0].Instrument == "" {
		t.Fatalf("all_instruments decoded empty: %+v", a)
	}
	t.Logf("all_instruments: %d instruments, identical in full and lite", len(a.Result))

	// Streams: book snapshots from both, matched by event time.
	sel := &models.WSOrderbookLevelsFeedSelectorV1{Instrument: "BTC_USDT_Perp", Rate: grvt.Ptr[uint32](500), Depth: grvt.Ptr[uint32](10)}
	frames := func(s *sized) map[int64]models.OrderbookLevels {
		sub, err := s.client.OrderbookSnapStream(ctx, sel)
		if err != nil {
			t.Fatal(err)
		}
		defer sub.Close()
		out := map[int64]models.OrderbookLevels{}
		for len(out) < 12 {
			f, err := sub.Next(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if f.Stream != "v1.book.s" || f.Feed.Instrument != "BTC_USDT_Perp" || len(f.Feed.Bids) == 0 {
				t.Fatalf("frame not decoded: %+v", f)
			}
			out[f.Feed.EventTime.UnixNano()] = f.Feed
		}
		return out
	}
	type res struct {
		m map[int64]models.OrderbookLevels
	}
	fc, lc := make(chan res), make(chan res)
	go func() { fc <- res{frames(full)} }()
	go func() { lc <- res{frames(lite)} }()
	ff, lf := (<-fc).m, (<-lc).m
	matched := 0
	for ts, f := range ff {
		if l, ok := lf[ts]; ok {
			matched++
			if !reflect.DeepEqual(f, l) {
				t.Fatalf("book at %d differs:\nfull %+v\nlite %+v", ts, f, l)
			}
		}
	}
	if matched == 0 {
		t.Fatal("no book snapshot seen by both clients")
	}
	t.Logf("book: %d snapshots seen by both, identical; mean frame %d bytes full, %d lite",
		matched, full.book.Load()/max(full.frames.Load(), 1), lite.book.Load()/max(lite.frames.Load(), 1))

	// Ticker frames carry a key the contract does not list; lite must skip it.
	tk, err := lite.client.TickerSnapStream(ctx, &models.WSTickerFeedSelectorV1{Instrument: "BTC_USDT_Perp", Rate: grvt.Ptr[uint32](500)})
	if err != nil {
		t.Fatal(err)
	}
	defer tk.Close()
	f, err := tk.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if f.Feed.MarkPrice == nil {
		t.Fatalf("lite ticker not decoded: %+v", f.Feed)
	}
	t.Logf("lite ticker mark %s", f.Feed.MarkPrice)
}
