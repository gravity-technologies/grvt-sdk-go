package models

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/gravity-technologies/grvt-sdk-go/internal/wirejson"
)

// Lite codecs against a reference renamer written from the contract fields alone (oracleToLite):
// for every model with a lite form, on the same random documents as the differential test,
//
//   - decoding the lite form accepts the same documents and gives the same value as decoding the
//     full form;
//   - liteFrom<T> turns the full text encoding/json writes for a value into the text the reference
//     renamer gives, uses only lite names of the right type at every level, and decodes back to the
//     same value.

func TestLiteMatchesFull(t *testing.T) {
	samples := 200
	if testing.Short() {
		samples = 30
	}
	byName := map[string]*legacyModel{}
	for i := range legacyTable {
		byName[legacyTable[i].name] = &legacyTable[i]
	}
	var models, compared, encoded int
	for i := range legacyTable {
		m := &legacyTable[i]
		if m.lite == nil {
			continue
		}
		models++
		g := &sampler{rng: rand.New(rand.NewPCG(uint64(i), 11)), models: byName} //nolint:gosec // reproducible test data
		for k := 0; k < samples; k++ {
			doc := g.document(m, k)
			lite, err := oracleToLite(byName, m.name, []byte(doc))
			if err != nil {
				continue // not JSON: nothing to rename
			}
			full, _ := m.fresh()
			errFull := m.fast([]byte(doc), full)
			got, _ := m.fresh()
			errLite := m.lite(lite, got)
			if (errFull == nil) != (errLite == nil) {
				t.Fatalf("%s: full and lite disagree\nfull %s: %v\nlite %s: %v", m.name, doc, errFull, lite, errLite)
			}
			if errFull != nil {
				continue
			}
			compared++
			if !reflect.DeepEqual(full, got) {
				t.Fatalf("%s: lite value differs\nfull %s\nlite %s\n%s", m.name, doc, lite, diffValues(reflect.ValueOf(full).Elem(), reflect.ValueOf(got).Elem(), m.name))
			}
			if checkLiteEncode(t, byName, m, full) {
				encoded++
			}
		}
	}
	t.Logf("%d lite models, %d documents compared, %d values encoded", models, compared, encoded)
	if models < 200 || compared < models*samples/10 {
		t.Fatalf("too little coverage: %d models, %d documents", models, compared)
	}
}

// checkLiteEncode encodes v (decoded from a full document) as lite and checks it; false when
// encoding/json cannot encode v (e.g. a timestamp outside int64 nanoseconds).
func checkLiteEncode(t *testing.T, byName map[string]*legacyModel, m *legacyModel, v any) bool {
	t.Helper()
	full, err := json.Marshal(v)
	if err != nil {
		return false
	}
	lite, err := m.toLite(full)
	if err != nil {
		t.Fatalf("%s: liteFrom on %s: %v", m.name, full, err)
	}
	want, err := oracleToLite(byName, m.name, full)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(lite, want) {
		t.Fatalf("%s: lite encoding\n got %s\nwant %s\nfrom %s", m.name, lite, want, full)
	}
	if err := onlyLiteKeys(byName, m.name, lite); err != nil {
		t.Fatalf("%s: %v in %s", m.name, err, lite)
	}
	// encoding/json drops empty lists (omitempty), so compare with the full text decoded, not with v.
	want2, _ := m.fresh()
	if err := m.fast(full, want2); err != nil {
		return false
	}
	back, _ := m.fresh()
	if err := m.lite(lite, back); err != nil {
		t.Fatalf("%s: decoding own lite %s: %v", m.name, lite, err)
	}
	if !reflect.DeepEqual(back, want2) {
		t.Fatalf("%s: lite round trip differs: %s", m.name, diffValues(reflect.ValueOf(want2).Elem(), reflect.ValueOf(back).Elem(), m.name))
	}
	return true
}

// Names repeat across types ("s" is a stream, a size and a status), so a renamer that used one global
// table would get these wrong. Each case nests types that reuse a lite name for different fields.
func TestLiteNamesArePerType(t *testing.T) {
	frame := `{"stream":"v1.book.s","selector":"BTC_USDT_Perp@500-10","sequence_number":"7","prev_sequence_number":"6",` +
		`"feed":{"event_time":"1","instrument":"BTC_USDT_Perp","bids":[{"price":"1.5","size":"2","num_orders":3}],"asks":[]}}`
	lite, err := wirejson.ToLite[WSOrderbookLevelsFeedDataV1]([]byte(frame))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"s":"v1.book.s","s1":"BTC_USDT_Perp@500-10","sn":"7","ps":"6",` +
		`"f":{"et":"1","i":"BTC_USDT_Perp","b":[{"p":"1.5","s":"2","no":3}],"a":[]}}`
	if string(lite) != want {
		t.Fatalf("got  %s\nwant %s", lite, want)
	}
	var full, back WSOrderbookLevelsFeedDataV1
	if err := json.Unmarshal([]byte(frame), &full); err != nil {
		t.Fatal(err)
	}
	if err := wirejson.UnmarshalLite(lite, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(full, back) {
		t.Fatalf("got %+v, want %+v", back, full)
	}
	if back.Stream != "v1.book.s" || back.Feed.Bids[0].Size.String() != "2" {
		t.Fatalf("s decoded into the wrong field: %+v", back)
	}
}

// Lite decoding skips keys it does not know (the server sends some the contract does not list, such
// as "fi" in ticker frames), and full names in a lite text are unknown keys, not fields.
func TestLiteDecodeSkipsUnknownKeys(t *testing.T) {
	var lvl OrderbookLevel
	if err := wirejson.UnmarshalLite([]byte(`{"p":"1","s":"2","no":3,"fi":{"x":[1]},"zz":null}`), &lvl); err != nil {
		t.Fatal(err)
	}
	if lvl.Price.String() != "1" || lvl.Size.String() != "2" || lvl.NumOrders != 3 {
		t.Fatalf("%+v", lvl)
	}
	err := wirejson.UnmarshalLite([]byte(`{"price":"1","size":"2","num_orders":3}`), &lvl)
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("full names in lite text: want a missing-field error, got %v", err)
	}
	// Lite has no case folding: "P" is not "p".
	if err := wirejson.UnmarshalLite([]byte(`{"P":"1","s":"2","no":3}`), &lvl); err == nil {
		t.Fatal(`"P" matched the lite name "p"`)
	}
}

// A full text with a key the type does not have cannot be made lite: the lite gateway would ignore
// the key, so the call would silently lose it.
func TestLiteEncodeRejectsUnknownKeys(t *testing.T) {
	for _, doc := range []string{
		`{"price":"1","size":"2","num_orders":3,"extra":1}`,
		`{"p":"1"}`, // already lite
	} {
		if _, err := wirejson.ToLite[OrderbookLevel]([]byte(doc)); err == nil {
			t.Errorf("%s: want an error", doc)
		}
	}
	if _, err := wirejson.ToLite[OrderbookLevels]([]byte(`{"bids":[{"price":"1","sizes":"2"}]}`)); err == nil {
		t.Error("an unknown key in a nested list item must fail")
	}
	// Edge types have no lite form.
	if _, err := wirejson.ToLite[EdgeLoginResponse]([]byte(`{}`)); err == nil {
		t.Error("edge types must not have a lite form")
	}
}

// -- reference renamer -----------------------------------------------------------------------------

// oracleToLite renames the full JSON text of the model typ to lite, keeping every value's text. A key
// matches a field as the full decoder matches it (exactly, then folded); an unknown key keeps its
// name behind a "~", which no lite name has, so it stays unknown.
func oracleToLite(byName map[string]*legacyModel, typ string, raw []byte) ([]byte, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		if !json.Valid(raw) {
			return nil, fmt.Errorf("invalid JSON")
		}
		return raw, nil
	}
	m := byName[typ]
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteByte('{')
	for first := true; dec.More(); first = false {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := tok.(string)
		var val json.RawMessage
		if err = dec.Decode(&val); err != nil {
			return nil, err
		}
		f := matchField(m, key)
		name := "~" + key
		if f != nil {
			name = f.lite
			if f.kind == "struct" {
				if val, err = oracleStruct(byName, f, val); err != nil {
					return nil, err
				}
			}
		}
		if !first {
			out.WriteByte(',')
		}
		k, err := json.Marshal(name)
		if err != nil {
			return nil, err
		}
		out.Write(k)
		out.WriteByte(':')
		out.Write(val)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	out.WriteByte('}')
	if _, err := dec.Token(); err == nil || dec.InputOffset() != int64(len(raw)) {
		return nil, fmt.Errorf("trailing data")
	}
	return out.Bytes(), nil
}

func oracleStruct(byName map[string]*legacyModel, f *legacyField, val []byte) ([]byte, error) {
	if !f.list || len(val) == 0 || val[0] != '[' {
		return oracleToLite(byName, f.typ, val)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(val, &items); err != nil {
		return nil, err
	}
	parts := make([]string, len(items))
	for i, it := range items {
		b, err := oracleToLite(byName, f.typ, it)
		if err != nil {
			return nil, err
		}
		parts[i] = string(b)
	}
	return []byte("[" + strings.Join(parts, ",") + "]"), nil
}

func matchField(m *legacyModel, key string) *legacyField {
	for i := range m.fields {
		if m.fields[i].name == key {
			return &m.fields[i]
		}
	}
	folded := string(wirejson.Fold(nil, []byte(key)))
	for i := range m.fields {
		if strings.ToUpper(m.fields[i].name) == folded {
			return &m.fields[i]
		}
	}
	return nil
}

// onlyLiteKeys fails on any key that is not a lite name of its type, at any depth.
func onlyLiteKeys(byName map[string]*legacyModel, typ string, raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return err
	}
	m := byName[typ]
next:
	for k, v := range obj {
		for i := range m.fields {
			f := &m.fields[i]
			if f.lite != k {
				continue
			}
			if f.kind != "struct" {
				continue next
			}
			if f.list && len(v) > 0 && v[0] == '[' {
				var items []json.RawMessage
				if err := json.Unmarshal(v, &items); err != nil {
					return err
				}
				for _, it := range items {
					if err := onlyLiteKeys(byName, f.typ, it); err != nil {
						return err
					}
				}
				continue next
			}
			if err := onlyLiteKeys(byName, f.typ, v); err != nil {
				return err
			}
			continue next
		}
		return fmt.Errorf("%s has no lite field %q", typ, k)
	}
	return nil
}
