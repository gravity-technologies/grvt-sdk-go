package main

import (
	"github.com/gravity-technologies/grvt-sdk-go/models"
	"testing"
)

func TestOrderAmounts(t *testing.T) {
	for _, tt := range []struct {
		name, bid, tick, size, minimum, cap, price, wantSize string
		invalid                                              bool
	}{
		{"fractional increments", "101.13", "0.25", "0.03", "5", "200", "100.75", "0.06", false},
		{"exact minimum", "101.25", "0.25", "0.03", "6.06", "200", "101.00", "0.06", false},
		{"no minimum notional", "101.13", "0.25", "0.03", "0", "200", "100.75", "0.03", false},
		{"cap too small", "101.13", "0.25", "0.03", "5", "6", "", "", true},
		{"no passive price", "0.2", "0.25", "0.03", "5", "200", "", "", true},
		{"invalid tick", "101.13", "0", "0.03", "5", "200", "", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := models.MustParseDecimal
			price, size, err := orderAmounts(d(tt.bid), d(tt.tick), d(tt.size), d(tt.minimum), d(tt.cap))
			if tt.invalid {
				if err == nil {
					t.Fatal("expected failure")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if price.String() != tt.price || size.String() != tt.wantSize {
				t.Fatalf("price=%s size=%s", price, size)
			}
		})
	}
}
