package grvt

// Read-only testnet check of the re-subscribe quirk: after a re-subscribe on the same socket the
// first delta's prev_sequence_number is the number from before it. GRVT_SMOKE=1 only.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/gravity-technologies/grvt-sdk-go/models"
)

func TestSmokeTestnetResubscribe(t *testing.T) {
	if os.Getenv("GRVT_SMOKE") != "1" {
		t.Skip("set GRVT_SMOKE=1 to run against testnet")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ev := &events{}
	client, err := New(Config{Env: Testnet(), OnEvent: ev.add})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	book, err := client.OrderbookDeltaStream(ctx, &models.WSOrderbookLevelsFeedSelectorV1{Instrument: "BTC_USDT_Perp", Rate: Ptr[uint32](100)})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	read := func(n int) {
		for i := 0; i < n; i++ {
			f, err := book.Next(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("seq %d prev %d", f.SequenceNumber, f.PrevSequenceNumber)
		}
	}
	read(3)
	client.Runtime().Socket(GatewayMarketData).resync(book.sb, "smoke test")
	read(3)
	if g := ev.of(EventGap); len(g) != 0 {
		t.Fatalf("gaps after a re-subscribe: %+v", g)
	}
	acks := ev.of(EventSubscribe)
	if len(acks) != 2 {
		t.Fatalf("subscribe acks %+v", acks)
	}
	for _, a := range acks {
		t.Logf("ack latest_sequence_number %d", a.LatestSequenceNumber)
	}
}
