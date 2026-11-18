package grvt_test

// Read-only testnet check, unauthenticated market data only:
//
//	GRVT_SMOKE=1 go test -run TestSmokeTestnet -v .

import (
	"context"
	"os"
	"testing"
	"time"

	grvt "github.com/gravity-technologies/grvt-sdk-go"
	"github.com/gravity-technologies/grvt-sdk-go/models"
)

func TestSmokeTestnet(t *testing.T) {
	if os.Getenv("GRVT_SMOKE") != "1" {
		t.Skip("set GRVT_SMOKE=1 to run against testnet")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := grvt.New(grvt.Config{Env: grvt.Testnet()})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	inst, err := client.GetInstrument(ctx, &models.ApiGetInstrumentRequest{Instrument: "BTC_USDT_Perp"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("instrument %s hash %s base_decimals %d", inst.Result.Instrument, inst.Result.InstrumentHash, inst.Result.BaseDecimals)
	tk, err := client.Ticker(ctx, &models.ApiTickerRequest{Instrument: "BTC_USDT_Perp"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ticker mark %s", tk.Result.MarkPrice)

	book, err := client.OrderbookDeltaStream(ctx, &models.WSOrderbookLevelsFeedSelectorV1{Instrument: "BTC_USDT_Perp", Rate: grvt.Ptr[uint32](100)})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	for i := 0; i < 3; i++ {
		f, err := book.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 && f.SequenceNumber != 0 {
			t.Fatalf("first frame is not a snapshot: %d", f.SequenceNumber)
		}
		t.Logf("book seq %d bids %d asks %d", f.SequenceNumber, len(f.Feed.Bids), len(f.Feed.Asks))
	}
}
