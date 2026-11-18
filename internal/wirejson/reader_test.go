package wirejson

import (
	"bytes"
	"encoding/json"
	"math/rand/v2"
	"strings"
	"testing"
)

// Skip accepts exactly the texts encoding/json accepts, including the nesting limit.
func TestSkipMatchesValid(t *testing.T) {
	docs := []string{
		strings.Repeat("[", 10000) + strings.Repeat("]", 10000),
		strings.Repeat("[", 10001) + strings.Repeat("]", 10001),
		strings.Repeat(`{"a":`, 70) + "1" + strings.Repeat("}", 70),
		strings.Repeat(`[{"a":`, 5000) + "1" + strings.Repeat("}]", 5000),
		strings.Repeat(`[{"a":`, 5001) + "1" + strings.Repeat("}]", 5001),
	}
	pieces := []string{"{", "}", "[", "]", ",", ":", `"a"`, `"é"`, `"\u00e9"`, `"\q"`, "\"\x01\"", "\"\xff\"", "1", "-0.5e+3", "01", "-", "1.",
		"true", "tru", "null", " ", "\n", `""`, `"\ud800"`, "1e", "0x1"}
	rng := rand.New(rand.NewPCG(3, 4)) //nolint:gosec // reproducible test data
	for i := 0; i < 200000; i++ {
		var b strings.Builder
		for n := rng.IntN(12); n >= 0; n-- {
			b.WriteString(pieces[rng.IntN(len(pieces))])
		}
		docs = append(docs, b.String())
	}
	for _, doc := range docs {
		r := NewReader([]byte(doc))
		err := r.Skip()
		if err == nil {
			err = r.End()
		}
		if want := json.Valid([]byte(doc)); want != (err == nil) {
			if len(doc) > 200 {
				doc = doc[:200] + "..."
			}
			t.Fatalf("%q: encoding/json valid %t, Skip %v", doc, want, err)
		}
	}
}

// Strings unescape like encoding/json, including invalid UTF-8 and lone surrogates.
func TestStringMatchesEncodingJSON(t *testing.T) {
	pieces := []string{"a", "é", "\xff", "\xed\xa0\x80", `\n`, `\"`, `\\`, `\/`, `\u0041`, `\ud83d\ude00`, `\ud83d`, `\ude00`, `\ud83dx`,
		`\ud83d\u0041`, "\U0001F600", "�", "abcdefghij"}
	rng := rand.New(rand.NewPCG(5, 6)) //nolint:gosec // reproducible test data
	for i := 0; i < 50000; i++ {
		var b strings.Builder
		b.WriteByte('"')
		for n := rng.IntN(8); n >= 0; n-- {
			b.WriteString(pieces[rng.IntN(len(pieces))])
		}
		b.WriteByte('"')
		doc := b.String()
		var want string
		if err := json.Unmarshal([]byte(doc), &want); err != nil {
			t.Fatalf("%q: %v", doc, err)
		}
		r := NewReader([]byte(doc))
		got, err := r.String()
		if err != nil || !bytes.Equal(got, []byte(want)) {
			t.Fatalf("%q: got %q (%v), want %q", doc, got, err, want)
		}
	}
}

// Fold matches keys to field names as encoding/json does.
func TestFold(t *testing.T) {
	type v struct {
		Size int `json:"size"`
		Kind int `json:"kind"`
	}
	for _, key := range []string{"size", "SIZE", "Size", "ſize", "ſIZE", "kind", "Kind", "Kind", "sizé", "siz", "kinds"} {
		var want v
		_ = json.Unmarshal([]byte(`{"`+key+`":1}`), &want)
		var buf [64]byte
		f := string(Fold(buf[:0], []byte(key)))
		got := v{}
		if f == "SIZE" {
			got.Size = 1
		}
		if f == "KIND" {
			got.Kind = 1
		}
		if got != want {
			t.Errorf("%q folds to %q: got %+v, encoding/json %+v", key, f, got, want)
		}
	}
}
