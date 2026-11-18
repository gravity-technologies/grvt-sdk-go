package models

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestModelTimestampRoundtrip(t *testing.T) {
	for _, ns := range []int64{math.MinInt64, -1, 0, 1793172225200080949, math.MaxInt64} {
		instant := time.Unix(0, ns).UTC()
		before := Signature{Expiration: instant}
		raw, err := json.Marshal(before)
		if err != nil {
			t.Fatal(err)
		}
		var after Signature
		if err = json.Unmarshal(raw, &after); err != nil {
			t.Fatal(err)
		}
		if !after.Expiration.Equal(instant) {
			t.Fatalf("timestamp changed: %s", raw)
		}
	}
	distant := time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := json.Marshal(Signature{Expiration: distant}); err == nil {
		t.Fatal("accepted out-of-range timestamp")
	}
}

func TestRequiredModels(t *testing.T) {
	var response ApiGetAllInstrumentsResponse
	if err := json.Unmarshal([]byte(`{}`), &response); err == nil {
		t.Fatal("missing required result accepted")
	}
	if err := json.Unmarshal([]byte(`{"result":null}`), &response); err != nil {
		t.Fatal(err)
	}
	var leg OrderLeg
	if err := json.Unmarshal([]byte(`{"instrument":"BTC_USDT_Perp","size":"1"}`), &leg); err == nil {
		t.Fatal("missing required boolean accepted")
	}
	if err := json.Unmarshal([]byte(`{"instrument":"BTC_USDT_Perp","size":"1","is_buying_asset":false}`), &leg); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(leg)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if value, ok := wire["is_buying_asset"]; !ok || value != false {
		t.Fatalf("false omitted: %s", raw)
	}
}
