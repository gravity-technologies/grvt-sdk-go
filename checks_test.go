package grvt

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/gravity-technologies/grvt-sdk-go/internal/wirejson"
	"github.com/gravity-technologies/grvt-sdk-go/models"
)

// The old JSON projection is independent of the generated typed projection. Both feed the same
// rule engine, so parity covers the complete error, including nearest values and the first failure.
func referenceOrderChecks(order models.OrderInput, meta *Meta) error {
	raw, err := json.Marshal(order)
	if err != nil {
		return &SigningError{Message: fmt.Sprintf("order: %v", err)}
	}
	r := wirejson.NewReader(raw)
	value, err := wirejson.DecodeAny(&r)
	if err != nil {
		return err
	}
	if err = r.End(); err != nil {
		return err
	}
	object := value.(map[string]any)
	skip := map[string]bool{}
	for _, rule := range orderChecks.Rules {
		for _, path := range rule.SkipIf {
			var current any = object
			for _, part := range strings.Split(path, ".") {
				parent, ok := current.(map[string]any)
				if !ok {
					current = nil
					break
				}
				current = parent[part]
			}
			skip[path], _ = current.(bool)
		}
	}
	legs, _ := object["legs"].([]any)
	for index, item := range legs {
		leg, _ := item.(map[string]any)
		values := map[string]string{}
		values[orderChecks.Instrument], _ = leg[orderChecks.Instrument].(string)
		for field := range orderChecks.Scales {
			values[field], _ = leg[field].(string)
		}
		if err := checkLeg(&orderChecks, skip, values, index, meta); err != nil {
			return err
		}
	}
	return nil
}

func checkOrderParity(t *testing.T, order models.OrderInput, meta *Meta) error {
	t.Helper()
	before, err := json.Marshal(order)
	if err != nil {
		t.Fatal(err)
	}
	got, want := CheckOrder(&orderChecks, order, meta), referenceOrderChecks(order, meta)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("typed: %#v\nJSON:  %#v", got, want)
	}
	after, err := json.Marshal(order)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("order checks changed the request")
	}
	return got
}

func TestTypedOrderChecksMatchSharedCases(t *testing.T) {
	data, err := ReadShared("scenarios/signing.json")
	if err != nil {
		t.Fatal(err)
	}
	var all struct {
		OrderChecks []struct {
			Name        string          `json:"name"`
			Expect      string          `json:"expect"`
			Order       json.RawMessage `json:"order"`
			Instruments []struct {
				Instrument     string `json:"instrument"`
				InstrumentHash string `json:"instrument_hash"`
				BaseDecimals   uint32 `json:"base_decimals"`
				TickSize       string `json:"tick_size"`
				MinSize        string `json:"min_size"`
				MinNotional    string `json:"min_notional"`
			} `json:"instruments"`
		} `json:"order_checks"`
	}
	if err = json.Unmarshal(data, &all); err != nil {
		t.Fatal(err)
	}
	if len(all.OrderChecks) == 0 {
		t.Fatal("no order-check cases")
	}
	for _, check := range all.OrderChecks {
		t.Run(check.Name, func(t *testing.T) {
			var order models.OrderInput
			if err := json.Unmarshal(check.Order, &order); err != nil {
				// Invalid Decimal text is refused by the model before either check path can run.
				if check.Expect != "invalid" {
					t.Fatalf("valid check refused by model: %v", err)
				}
				t.Logf("model refused input: %v", err)
				return
			}
			meta := &Meta{Instruments: map[string]InstrumentMeta{}}
			for _, instrument := range check.Instruments {
				meta.Instruments[instrument.Instrument] = InstrumentMeta{InstrumentHash: instrument.InstrumentHash, BaseDecimals: instrument.BaseDecimals,
					Limits: map[string]string{"tick_size": instrument.TickSize, "min_size": instrument.MinSize, "min_notional": instrument.MinNotional}}
			}
			checkOrderParity(t, order, meta)
		})
	}
}

func TestTypedOrderChecksBoundaries(t *testing.T) {
	meta := &Meta{Instruments: map[string]InstrumentMeta{"BTC": {BaseDecimals: 0, Limits: map[string]string{"min_size": "1", "tick_size": "0.1", "min_notional": "1"}}}}
	leg := func(size string, price *models.Decimal) models.OrderLeg {
		return models.OrderLeg{Instrument: "BTC", Size: models.MustParseDecimal(size), LimitPrice: price}
	}
	price := models.MustParseDecimal("1")
	tests := []struct {
		name      string
		order     models.OrderInput
		field     string
		index     int
		malformed bool
	}{
		{name: "no legs"},
		{name: "missing price", order: models.OrderInput{Legs: []models.OrderLeg{leg("1", nil)}}},
		{name: "uint64 maximum", order: models.OrderInput{Legs: []models.OrderLeg{leg("18446744073709551615", &price)}}},
		{name: "uint64 overflow", order: models.OrderInput{Legs: []models.OrderLeg{leg("18446744073709551616", &price)}}, malformed: true},
		{name: "negative", order: models.OrderInput{Legs: []models.OrderLeg{leg("-1", &price)}}, malformed: true},
		{name: "zero size", order: models.OrderInput{Legs: []models.OrderLeg{leg("0", &price)}}, field: "size"},
		{name: "first failing leg", order: models.OrderInput{Legs: []models.OrderLeg{leg("1", &price), leg("0", &price), leg("-1", &price)}}, field: "size", index: 1},
		{name: "nil nested metadata", order: models.OrderInput{Metadata: &models.OrderMetadataInput{}, Legs: []models.OrderLeg{leg("1", &price)}}},
		{name: "reduce only skips minimum", order: models.OrderInput{ReduceOnly: Ptr(true), Legs: []models.OrderLeg{leg("0", &price)}}},
		{name: "skip does not allow negative", order: models.OrderInput{ReduceOnly: Ptr(true), IsMarket: Ptr(true), Legs: []models.OrderLeg{leg("-1", nil)}}, malformed: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkOrderParity(t, tc.order, meta)
			if tc.malformed {
				var malformed *SigningError
				if !errors.As(err, &malformed) {
					t.Fatalf("want malformed value, got %v", err)
				}
				return
			}
			if tc.field != "" {
				var failure *OrderOutOfBoundsError
				if !errors.As(err, &failure) || failure.Field != tc.field || failure.Leg != tc.index {
					t.Fatalf("wrong first failure: %#v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Multiple malformed fields must produce the same complete error on every call,
// including the JSON reference path whose map insertion order can differ.
func TestTypedOrderChecksMultipleMalformedFields(t *testing.T) {
	meta := &Meta{Instruments: map[string]InstrumentMeta{"BTC": {BaseDecimals: 0}}}
	for _, value := range []string{"-1", "18446744073709551616"} {
		price := models.MustParseDecimal(value)
		order := models.OrderInput{Legs: []models.OrderLeg{{Instrument: "BTC", Size: price, LimitPrice: &price}}}
		want := "OrderLeg.limit_price: " + value + " is out of range"
		for range 1000 {
			err := checkOrderParity(t, order, meta)
			var signing *SigningError
			if !errors.As(err, &signing) || signing.Message != want {
				t.Fatalf("want %q, got %#v", want, err)
			}
		}
	}
}

func BenchmarkCheckOrder(b *testing.B) {
	meta := &Meta{Instruments: map[string]InstrumentMeta{"BTC": {BaseDecimals: 0, Limits: map[string]string{"min_size": "1", "tick_size": "0.1", "min_notional": "1"}}}}
	price := models.MustParseDecimal("1")
	order := models.OrderInput{Legs: []models.OrderLeg{{Instrument: "BTC", Size: price, LimitPrice: &price}}}
	for _, implementation := range []struct {
		name  string
		check func(models.OrderInput, *Meta) error
	}{
		{"typed", func(order models.OrderInput, meta *Meta) error { return CheckOrder(&orderChecks, order, meta) }},
		{"JSON", referenceOrderChecks},
	} {
		b.Run(implementation.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if err := implementation.check(order, meta); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
