package models

import (
	"encoding/json"
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"
)

func TestDecimalExact(t *testing.T) {
	text := "12345678901234567890.1234567890"
	value, err := ParseDecimal(text)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Decimal
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.String() != text || decoded.Rat().Cmp(value.Rat()) != 0 {
		t.Fatalf("lost precision: %s", raw)
	}
	if (Decimal{}).String() != "0" {
		t.Fatal("invalid zero value")
	}
}
func TestDecimalRejectInvalid(t *testing.T) {
	for _, text := range []string{"", "NaN", "Infinity", "1e3", "0x10", " 1", "01", ".5", "1."} {
		if _, err := ParseDecimal(text); err == nil {
			t.Errorf("accepted %q", text)
		}
	}
	for _, text := range []string{"null", "1", `"NaN"`} {
		var value Decimal
		if err := json.Unmarshal([]byte(text), &value); err == nil {
			t.Errorf("accepted wire %q", text)
		}
	}
}

// validDecimal accepts exactly what the regular expression it replaced accepted.
func TestValidDecimalMatchesPattern(t *testing.T) {
	pattern := regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?$`)
	alphabet := []string{"0", "1", "9", "-", ".", "e", "+", " ", "\n", "a"}
	rng := rand.New(rand.NewPCG(8, 9)) //nolint:gosec // reproducible test data
	for i := 0; i < 200000; i++ {
		var b strings.Builder
		for n := rng.IntN(7); n > 0; n-- {
			b.WriteString(alphabet[rng.IntN(len(alphabet))])
		}
		s := b.String()
		if got, want := validDecimal([]byte(s)), pattern.MatchString(s); got != want {
			t.Fatalf("%q: validDecimal %t, pattern %t", s, got, want)
		}
	}
}
