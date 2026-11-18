package models

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// Differential test: the one-pass decoders against the encoding/json decoders they replaced
// (legacy<T> in legacy_gen_test.go), on random valid and invalid documents derived from the contract
// fields of every model. Both must accept and reject the same documents and decode the same values,
// through json.Unmarshal (UnmarshalJSON) and through the runtime's direct path (unmarshal<T>),
// including when decoding into a value that already holds an earlier document.

func TestDecodersMatchEncodingJSON(t *testing.T) {
	samples := 300
	if testing.Short() {
		samples = 40
	}
	byName := map[string]*legacyModel{}
	for i := range legacyTable {
		byName[legacyTable[i].name] = &legacyTable[i]
	}
	var accepted, rejected int
	for i := range legacyTable {
		m := &legacyTable[i]
		rng := rand.New(rand.NewPCG(uint64(i), 7)) //nolint:gosec // reproducible test data
		g := &sampler{rng: rng, models: byName}
		docs := []string{"null", "{}", "[]", `"x"`, "1", "true", "", " ", "{", `{"a":1,}`, "{} {}", "nul"}
		for k := 0; k < samples; k++ {
			docs = append(docs, g.document(m, k))
		}
		var prev string
		for _, doc := range docs {
			if compareDecoders(t, m, doc) {
				accepted++
			} else {
				rejected++
			}
			compareReuse(t, m, prev, doc)
			prev = doc
		}
	}
	t.Logf("%d models, %d documents accepted, %d rejected", len(legacyTable), accepted, rejected)
	if accepted < len(legacyTable)*samples/10 {
		t.Fatalf("only %d documents were accepted: the sampler is too hostile to prove values match", accepted)
	}
}

// compareDecoders decodes doc with every path and reports whether it was accepted.
func compareDecoders(t *testing.T, m *legacyModel, doc string) bool {
	t.Helper()
	cur, old := m.fresh()
	errOld := json.Unmarshal([]byte(doc), old)
	errCur := json.Unmarshal([]byte(doc), cur)
	fast, _ := m.fresh()
	errFast := m.fast([]byte(doc), fast)
	if (errOld == nil) != (errCur == nil) || (errOld == nil) != (errFast == nil) {
		t.Fatalf("%s: accept/reject differs on %q\nencoding/json: %v\njson.Unmarshal now: %v\ndirect: %v", m.name, doc, errOld, errCur, errFast)
	}
	if errOld != nil {
		checkErrorKind(t, m.name, doc, errOld, errCur, "json.Unmarshal")
		checkErrorKind(t, m.name, doc, errOld, errFast, "direct")
		return false
	}
	if diff := diffValues(reflect.ValueOf(old).Elem(), reflect.ValueOf(cur).Elem(), m.name); diff != "" {
		t.Fatalf("%s: json.Unmarshal value differs on %q: %s", m.name, doc, diff)
	}
	if diff := diffValues(reflect.ValueOf(old).Elem(), reflect.ValueOf(fast).Elem(), m.name); diff != "" {
		t.Fatalf("%s: direct decode value differs on %q: %s", m.name, doc, diff)
	}
	return true
}

// compareReuse decodes a then b into the same value.
func compareReuse(t *testing.T, m *legacyModel, a, b string) {
	t.Helper()
	cur, old := m.fresh()
	fast, _ := m.fresh()
	_ = json.Unmarshal([]byte(a), old)
	_ = json.Unmarshal([]byte(a), cur)
	firstOK := m.fast([]byte(a), fast) == nil
	errOld := json.Unmarshal([]byte(b), old)
	errCur := json.Unmarshal([]byte(b), cur)
	errFast := m.fast([]byte(b), fast)
	if (errOld == nil) != (errCur == nil) || (errOld == nil) != (errFast == nil) {
		t.Fatalf("%s: accept/reject differs on %q after %q\nencoding/json: %v\njson.Unmarshal now: %v\ndirect: %v", m.name, b, a, errOld, errCur, errFast)
	}
	if errOld != nil {
		// A model with UnmarshalJSON changes its target only on success, on every path.
		if _, custom := fast.(json.Unmarshaler); custom && firstOK {
			want, _ := m.fresh()
			_ = m.fast([]byte(a), want)
			if diff := diffValues(reflect.ValueOf(want).Elem(), reflect.ValueOf(fast).Elem(), m.name); diff != "" {
				t.Fatalf("%s: failed direct decode of %q changed the target: %s", m.name, b, diff)
			}
		}
		return
	}
	if diff := diffValues(reflect.ValueOf(old).Elem(), reflect.ValueOf(cur).Elem(), m.name); diff != "" {
		t.Fatalf("%s: json.Unmarshal value differs on %q after %q: %s", m.name, b, a, diff)
	}
	// The runtime decodes only into new values and drops them on error, so after a rejected document
	// the direct path's leftovers are not compared.
	if !firstOK {
		return
	}
	if diff := diffValues(reflect.ValueOf(old).Elem(), reflect.ValueOf(fast).Elem(), m.name); diff != "" {
		t.Fatalf("%s: direct decode value differs on %q after %q: %s", m.name, b, a, diff)
	}
}

// diffValues compares a legacy value with a current one field by field (the types differ only in
// the names of nested model types). Empty means equal.
func diffValues(a, b reflect.Value, path string) string {
	if a.Type() == b.Type() {
		if !reflect.DeepEqual(a.Interface(), b.Interface()) {
			return fmt.Sprintf("%s: %#v vs %#v", path, a.Interface(), b.Interface())
		}
		return ""
	}
	if a.Kind() != b.Kind() {
		return fmt.Sprintf("%s: kind %s vs %s", path, a.Kind(), b.Kind())
	}
	switch a.Kind() {
	case reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			if a.IsNil() != b.IsNil() {
				return path + ": nil vs non-nil"
			}
			return ""
		}
		return diffValues(a.Elem(), b.Elem(), path)
	case reflect.Slice:
		if a.IsNil() != b.IsNil() || a.Len() != b.Len() {
			return fmt.Sprintf("%s: list nil %t len %d vs nil %t len %d", path, a.IsNil(), a.Len(), b.IsNil(), b.Len())
		}
		for i := 0; i < a.Len(); i++ {
			if d := diffValues(a.Index(i), b.Index(i), fmt.Sprintf("%s[%d]", path, i)); d != "" {
				return d
			}
		}
		return ""
	case reflect.Struct:
		for i := 0; i < a.NumField(); i++ {
			if d := diffValues(a.Field(i), b.Field(i), path+"."+a.Type().Field(i).Name); d != "" {
				return d
			}
		}
		return ""
	}
	return fmt.Sprintf("%s: cannot compare %s with %s", path, a.Type(), b.Type())
}

// -- sampler -------------------------------------------------------------------------------------

type sampler struct {
	rng    *rand.Rand
	models map[string]*legacyModel
}

func (g *sampler) pick(xs ...string) string { return xs[g.rng.IntN(len(xs))] }
func (g *sampler) chance(p float64) bool    { return g.rng.Float64() < p }

// document is one sample for m: usually a valid object, sometimes broken on purpose.
func (g *sampler) document(m *legacyModel, k int) string {
	doc := g.object(m, 0, k%3 == 0)
	switch g.rng.IntN(12) {
	case 0: // truncated
		if len(doc) > 1 {
			doc = doc[:g.rng.IntN(len(doc))]
		}
	case 1: // trailing data
		doc += g.pick(" ", "\n", " x", "}", ",1", " null")
	case 2:
		doc = g.pick(" ", "\t\n", "\r") + doc + g.pick("", " ", "\n")
	}
	return doc
}

// object writes an object for m. hostile adds edge cases: odd key spellings, repeats, nulls, unknown
// keys and out-of-range values.
func (g *sampler) object(m *legacyModel, depth int, hostile bool) string {
	var parts []string
	for _, f := range m.fields {
		if f.optional && g.chance(0.5) {
			continue
		}
		if !f.optional && hostile && g.chance(0.04) {
			continue // a missing required key
		}
		n := 1
		if hostile && g.chance(0.15) {
			n = 2 + g.rng.IntN(2) // a repeated key
		}
		for i := 0; i < n; i++ {
			parts = append(parts, g.key(f.name, hostile)+g.pick(":", " : ")+g.field(f, depth, hostile))
		}
	}
	if g.chance(0.3) {
		parts = append(parts, g.key(g.pick("unknown", "x", "Ünknown", ""), false)+":"+g.anyValue(0))
	}
	g.rng.Shuffle(len(parts), func(i, j int) { parts[i], parts[j] = parts[j], parts[i] })
	return "{" + strings.Join(parts, g.pick(",", ", ", ",\n")) + "}"
}

func (g *sampler) key(name string, hostile bool) string {
	if !hostile || !g.chance(0.2) {
		return `"` + name + `"`
	}
	switch g.rng.IntN(5) {
	case 0:
		return `"` + strings.ToUpper(name) + `"`
	case 1:
		return `"` + strings.ToUpper(name[:1]) + name[1:] + `"`
	case 2: // the long s and the Kelvin sign fold to s and k
		return `"` + strings.NewReplacer("s", "ſ", "k", "K").Replace(name) + `"`
	case 3: // an escaped first letter
		return fmt.Sprintf(`"\u%04x%s"`, name[0], name[1:])
	default:
		return `"` + name + `x"`
	}
}

func (g *sampler) field(f legacyField, depth int, hostile bool) string {
	if hostile && g.chance(0.05) {
		return "null"
	}
	if f.list {
		if g.chance(0.1) {
			return g.pick("null", "[]", "{}", `"x"`)
		}
		n := g.rng.IntN(4)
		items := make([]string, n)
		for i := range items {
			items[i] = g.scalar(f, depth, hostile)
		}
		return "[" + strings.Join(items, ",") + "]"
	}
	return g.scalar(f, depth, hostile)
}

func (g *sampler) scalar(f legacyField, depth int, hostile bool) string {
	odd := hostile && g.chance(0.15)
	switch f.kind {
	case "decimal":
		if odd {
			return g.pick(`"-0"`, `"01"`, `"1e3"`, `"1."`, `".5"`, `"\u0031.5"`, `1.5`, `null`, `""`, `" 1"`, `"-"`, `{}`)
		}
		return g.pick(`"0"`, `"1.5"`, `"-12.000001"`, `"65000.5"`, `"123456789012345678901234567890.1"`)
	case "struct":
		if depth > 3 || odd {
			return g.pick("null", "{}", `"x"`, "[]", "1")
		}
		return g.object(g.models[f.typ], depth+1, hostile)
	case "enum":
		if odd {
			return g.pick(`"UNKNOWN_VALUE"`, `""`, `1`, `null`, `"\u0041"`, `"\u0041BC"`)
		}
		vs := legacyEnums[f.typ]
		return `"` + vs[g.rng.IntN(len(vs))] + `"`
	case "timestamp":
		if odd {
			return g.pick(`"+5"`, `"-1"`, `"007"`, `" 5"`, `5`, `null`, `""`, `"9223372036854775808"`, `"-9223372036854775808"`, `"1e3"`)
		}
		return g.pick(`"1697788800000000000"`, `"0"`, `"1"`)
	case "any":
		return g.anyValue(0)
	}
	switch f.typ {
	case "bool":
		if odd {
			return g.pick(`"true"`, `1`, `null`, `tru`)
		}
		return g.pick("true", "false")
	case "uint8", "uint16", "uint32", "int8", "int16", "int32":
		if odd {
			return g.pick("-1", "-0", "1.0", "1e2", "256", "65536", "4294967296", "-129", "-2147483649", `"1"`, "null", "01", "-")
		}
		return g.pick("0", "1", "127", "255")
	case "uint64", "int64":
		if odd {
			return g.pick(`" 5 "`, `"null"`, `" null"`, `"-0"`, `"007"`, `"1e3"`, `"1.0"`, `"+5"`, `""`, `"\u0035"`, `5`, `null`,
				`"18446744073709551615"`, `"18446744073709551616"`, `"-9223372036854775808"`, `"9223372036854775808"`, `"5 5"`, `"\"5\""`, `"-"`, `"true"`, `"nul"`)
		}
		return g.pick(`"0"`, `"1"`, `"2398479823477"`, `"9223372036854775807"`)
	}
	// strings
	if odd {
		return g.pick(`1`, `null`, `true`, `"\ud800x"`, `"\ud83d\ude00"`, "\"\xff\xfe\"", `"a\u0000b"`, `"\n"`, "\"\t\"", `"\x"`, `"é"`)
	}
	return g.pick(`""`, `"abc"`, `"BTC_USDT_Perp"`, `"0x12ab"`, `"a\"b"`, `"h\u00e9llo"`, `"héllo"`, `"日本"`)
}

func (g *sampler) anyValue(depth int) string {
	if depth > 2 {
		return g.pick("1", "null", `"s"`)
	}
	switch g.rng.IntN(8) {
	case 0:
		return "{}"
	case 1:
		return `{"a":` + g.anyValue(depth+1) + `, "b" : [` + g.anyValue(depth+1) + `]}`
	case 2:
		return "[" + g.anyValue(depth+1) + "," + g.anyValue(depth+1) + "]"
	case 3:
		return g.pick("-1.5e+3", "0", "1E2", "-0.0")
	case 4:
		return g.pick("true", "false", "null")
	case 5:
		return g.pick(`"x"`, `"\u00e9\n"`, `""`)
	case 6:
		return g.pick("[1,]", `{"a"}`, "01", "tru", `"\q"`, "[") // invalid
	}
	return "[ ]"
}

// checkErrorKind: invalid JSON is a *json.SyntaxError on every path, as encoding/json reported it.
func checkErrorKind(t *testing.T, model, doc string, old, cur error, path string) {
	t.Helper()
	var se *json.SyntaxError
	if !json.Valid([]byte(doc)) && (!errors.As(old, &se) || !errors.As(cur, &se)) {
		t.Fatalf("%s %s: invalid JSON %q: encoding/json %T %v, now %T %v", model, path, doc, old, old, cur, cur)
	}
}

// Fixed edge cases the random documents may miss: nesting at encoding/json's limit (beyond the
// 64-level fast skip), long and surrogate-pair keys, case-folded and repeated keys (including the
// dotless i, which does not fold to i), integer and timestamp texts encoding/json treats specially.
func TestDecodersMatchOnEdgeCases(t *testing.T) {
	var order *legacyModel
	for i := range legacyTable {
		if legacyTable[i].name == "Order" {
			order = &legacyTable[i]
		}
	}
	leg := `"legs":[{"instrument":"BTC","size":"1","is_buying_asset":true}]`
	base := `{"sub_account_id":"1","time_in_force":"GOOD_TILL_TIME",` + leg +
		`,"signature":{"signer":"0x1","r":"0x1","s":"0x1","v":1,"expiration":"1","nonce":1,"chain_id":"1"},"metadata":{"client_order_id":"1"}`
	with := func(from, to string) string {
		if !strings.Contains(base, from) {
			t.Fatalf("base has no %q", from)
		}
		return strings.Replace(base, from, to, 1) + "}"
	}
	nested := func(opening, closing string, n int) string {
		return base + `,"x":` + strings.Repeat(opening, n) + "1" + strings.Repeat(closing, n) + "}"
	}
	docs := []string{
		base + "}",
		base + "}\x00", base + "} \x00", base + "}\x00 ", // a NUL byte after the value is trailing data
		nested("[", "]", 9997), nested("[", "]", 9998), nested("[", "]", 9999), nested("[", "]", 10000),
		nested(`{"a":`, "}", 9998), nested(`{"a":`, "}", 9999), nested(`{"a":`, "}", 10000),
		nested(`[{"a":`, "}]", 4999), nested(`[{"a":`, "}]", 5000),
		nested("[", "]", 64), nested("[", "]", 65),
		base + `,"\ud83d\ude00":1}`,
		base + `,"\ud83d":1}`,
		base + `,"` + strings.Repeat("k", 200) + `":1}`,
		base + `,"order_id` + strings.Repeat("_", 70) + `":1}`,
		base + `,"ORDER_ID":"a","order_id":"b","Order_Id":null}`,
		base + `,"is_market":null,"is_market":true,"IS_MARKET":false}`,
		base + `,"builder_fee":"1e3"}`,
		base + `,"builder_fee":null}`,
		with(`"size":"1"`, `"size":"1",  "SIZE":"2"`),
		with(`"size":"1"`, `"sıze":"2"`),
		with(`"size":"1"`, `"ſize":"2"`),
		with(`"instrument":"BTC"`, `"İnstrument":"BTC"`),
		with(`"sub_account_id":"1"`, `"sub_account_id":" 1 "`),
		with(`"sub_account_id":"1"`, `"sub_account_id":"1\u0000"`),
		with(`"sub_account_id":"1"`, `"sub_account_id":"0x1"`),
		with(`"sub_account_id":"1"`, `"sub_account_id":"+1"`),
		with(`"sub_account_id":"1"`, `"sub_account_id":"1.0"`),
		with(`"sub_account_id":"1"`, `"sub_account_id":"1e0"`),
		with(`"sub_account_id":"1"`, `"sub_account_id":"-0"`),
		with(`"nonce":1`, `"nonce":1.0`),
		with(`"nonce":1`, `"nonce":-0`),
		with(`"nonce":1`, `"nonce":1e0`),
		with(`"nonce":1`, `"nonce":"1"`),
		with(`"chain_id":"1"`, `"chain_id":"-0"`),
		with(`"expiration":"1"`, `"expiration":"+1"`),
		with(`"expiration":"1"`, `"expiration":"\u002b1"`),
		with(`"expiration":"1"`, `"expiration":"01"`),
		with(`"expiration":"1"`, `"expiration":" 1"`),
		with(`"client_order_id":"1"`, `"client_order_id":"\" 1\""`),
		with(`"client_order_id":"1"`, `"client_order_id":"0x1"`),
		with(`"client_order_id":"1"`, `"client_order_id":"\t1"`),
		with(`"client_order_id":"1"`, `"client_order_id":"1 "`),
		with(`"client_order_id":"1"`, `"client_order_id":"null "`),
		with(`"client_order_id":"1"`, `"client_order_id":"-1"`),
		with(`"client_order_id":"1"`, `"client_order_id":"+1"`),
		with(`"metadata":{"client_order_id":"1"}`, `"metadata":{"client_order_id":"1","create_time":"+0"}`),
		with(`"metadata":{"client_order_id":"1"}`, `"metadata":{"client_order_id":"1","slippage_bps":"1"}`),
		with(`"metadata":{"client_order_id":"1"}`, `"metadata":{"client_order_id":"1","slippage_bps":1.5e0}`),
		with(`"metadata":{"client_order_id":"1"}`, `"metadata":{"client_order_id":"1","slippage_bps":1e0}`),
	}
	var prev string
	for _, doc := range docs {
		compareDecoders(t, order, doc)
		compareReuse(t, order, base+"}", doc)
		compareReuse(t, order, prev, doc)
		prev = doc
	}
}

// Errors keep encoding/json's types where they map: callers can still errors.As them.
func TestDecodeErrorTypes(t *testing.T) {
	var se *json.SyntaxError
	var te *json.UnmarshalTypeError
	var ne *strconv.NumError
	var level OrderbookLevel
	if err := unmarshalOrderbookLevel([]byte(`{"price":"1","size":"1","num_orders":1,}`), &level); !errors.As(err, &se) {
		t.Errorf("syntax: got %T %v", err, err)
	}
	err := unmarshalOrderbookLevel([]byte(`{"price":"1","size":"1","num_orders":"x"}`), &level)
	if !errors.As(err, &te) || te.Field != "num_orders" || te.Struct != "OrderbookLevel" || te.Value != "string" {
		t.Errorf("type: got %T %v", err, err)
	}
	var levels OrderbookLevels
	err = unmarshalOrderbookLevels([]byte(`{"event_time":"1","instrument":"X","bids":[{"price":"1","size":"1","num_orders":4294967296}],"asks":[]}`), &levels)
	if !errors.As(err, &te) || te.Field != "bids.num_orders" || te.Struct != "OrderbookLevels" || te.Value != "number 4294967296" {
		t.Errorf("nested range: got %T %v", err, err)
	}
	err = unmarshalOrderbookLevels([]byte(`{"event_time":"x","instrument":"X","bids":[],"asks":[]}`), &levels)
	if !errors.As(err, &ne) {
		t.Errorf("timestamp: got %T %v", err, err)
	}
	if err := json.Unmarshal([]byte(`{"price":"1","size":"1","num_orders":true}`), &level); !errors.As(err, &te) {
		t.Errorf("json.Unmarshal type: got %T %v", err, err)
	}
}
