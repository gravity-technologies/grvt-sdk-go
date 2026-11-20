package grvt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/gravity-technologies/grvt-sdk-go/models"
)

// The range itself is a shared scenario; this checks what the data can't: the counter's random start uses all
// 31 bits, and the counter wraps without leaving the range.
func TestClientOrderIDCounterStartsAnywhereAndWraps(t *testing.T) {
	ids := TimeCounterClientOrderID{random: func() uint32 { return 1<<32 - 1 }}
	a, b := ids.NextClientOrderID(), ids.NextClientOrderID()
	if a < 1<<63 || b < 1<<63 || a&(1<<31-1) != 1<<31-1 || b&(1<<31-1) != 0 {
		t.Fatalf("ids %x %x", a, b)
	}
}

func TestRequestIDsSkipZeroAndInUse(t *testing.T) {
	tb := newRequestTable[int](1<<32 - 1)
	a, _ := tb.add(&pending[int]{method: "a"})
	b, _ := tb.add(&pending[int]{method: "b", write: true})
	if a != 1<<32-1 || b != 1 {
		t.Fatalf("ids %d %d", a, b)
	}
	if tb.writesInFlight() != 1 || tb.pop(a).method != "a" || tb.pop(a) != nil {
		t.Fatal("pop")
	}
	tb.next = 1 // wrap onto the pending id 1
	if c, _ := tb.add(&pending[int]{}); c != 2 {
		t.Fatalf("id %d, want 2 (1 is in use)", c)
	}
	if len(tb.drain()) != 2 || len(tb.pending) != 0 {
		t.Fatal("drain")
	}
}

func TestOnlySnapshotStreamsMayDrop(t *testing.T) {
	for _, s := range Streams {
		if s.Conflate != s.SnapshotFeed {
			t.Errorf("%s: Conflate=%v SnapshotFeed=%v", s.Stream, s.Conflate, s.SnapshotFeed)
		}
	}
}

func TestBackoffIsFullJitterAndCapped(t *testing.T) {
	b := newBackoff(100*time.Millisecond, time.Second)
	b.rand = func() float64 { return 0.999 }
	for attempt := 0; attempt < 10; attempt++ {
		ceiling := min(100*time.Millisecond<<attempt, time.Second)
		if d := b.Next(); d > ceiling || d < ceiling*99/100 {
			t.Fatalf("attempt %d: %s, ceiling %s", attempt, d, ceiling)
		}
	}
	b.Reset()
	if b.Attempt != 0 {
		t.Fatal("reset")
	}
}

func TestFrameAndSelector(t *testing.T) {
	if got := string(jsonrpcFrame("v1/instrument", []byte(`{"instrument":"X"}`), 7)); got != `{"jsonrpc":"2.0","method":"v1/instrument","params":{"instrument":"X"},"id":7}` {
		t.Fatal(got)
	}
	if got := Selector([]string{"BTC", ""}, []string{"500", "10"}); got != "BTC@500-10" {
		t.Fatal(got)
	}
	if got := Selector([]string{"BTC"}, []string{SelectorInt[uint32](nil)}); got != "BTC" {
		t.Fatal(got)
	}
}

func TestScaleDecimalRejectsOverPrecision(t *testing.T) {
	for _, c := range []struct {
		in   string
		dec  uint32
		want string
	}{{"65000.1", 9, "65000100000000"}, {"1.500", 1, "15"}, {"0", 9, "0"}, {"-2.5", 1, "-25"}} {
		got, err := ScaleDecimal(c.in, c.dec)
		if err != nil || got.String() != c.want {
			t.Fatalf("%s: %v %v", c.in, got, err)
		}
	}
	if _, err := ScaleDecimal("1.1234567891", 9); err == nil || !strings.Contains(err.Error(), "decimal places") {
		t.Fatal(err)
	}
	if _, err := ScaleDecimal("abc", 9); err == nil {
		t.Fatal("abc")
	}
}

func TestTransforms(t *testing.T) {
	b := &MessageBuilder{}
	n := uint64(8289849667772468)
	if v := b.Copy(&n, "uint64", "x", nil); v.(*big.Int).Uint64() != n {
		t.Fatal(v)
	}
	if v := b.Copy((*bool)(nil), "bool", "x", false); v != false {
		t.Fatal(v)
	}
	if b.Err != nil {
		t.Fatal(b.Err)
	}
	b.Copy((*uint64)(nil), "uint64", "x", nil)
	if b.Err == nil {
		t.Fatal("missing required field must fail")
	}
	if AddressSet("0x0000000000000000000000000000000000000000") || !AddressSet("0x00000000000000000000000000000000000000a1") {
		t.Fatal("AddressSet")
	}
	b = &MessageBuilder{}
	b.EnumCode(models.TimeInForce("FROM_THE_FUTURE"), "tif")
	if b.Err == nil {
		t.Fatal("unknown enum values cannot be signed")
	}
}

// The EIP-712 spec's "Mail" example, through the reference encoder (the generated encoders cover
// only the SDK's own types; reference_test.go checks them against it).
func TestEIP712SpecExample(t *testing.T) {
	st, err := newStructType("Mail", []TypeDef{
		{Name: "Mail", Fields: []FieldDef{{"from", "Person"}, {"to", "Person"}, {"contents", "string"}}},
		{Name: "Person", Fields: []FieldDef{{"name", "string"}, {"wallet", "address"}}},
	}, "0xa0cedeb2dc280ba39b857546d74f5549c3a1d7bdc2dd96bf881f76108e23dac2", nil)
	if err != nil {
		t.Fatal(err)
	}
	addr := func(s string) Address { a, _ := toAddress(s); return a }
	msg := Struct{
		Struct{"Cow", addr("0xCD2a3d9F938E13CD947Ec05AbC7FE734Df8DD826")},
		Struct{"Bob", addr("0xbBbBBBBbbBBBbbbBbbBbbbbBBbBbbbbBbBbbBBbB")},
		"Hello, Bob!",
	}
	vc := addr("0xCcCCccccCCCCcCCCCCCcCcCccCcCCCcCcccccccC")
	d := NewDomain("Ether Mail", "1", 1, &vc)
	sep := d.Separator()
	ref, err := newRefType(st)
	if err != nil {
		t.Fatal(err)
	}
	h, _ := ref.HashStruct(msg)
	dg, _ := ref.Digest(d, msg)
	for _, c := range [][2]string{
		{Hex0x(sep[:]), "0xf2cee375fa42b42143804025fc449deafd50cc031ca257e0b194a650a912090f"},
		{Hex0x(h[:]), "0xc52c0ee5d84264471806290a3f2c4cecfc5490626bf912d01f240d7a274b371e"},
		{Hex0x(dg[:]), "0xbe609aee343fb3c4b28e1df9e632fca64fcfaede20f02e86244efddf30957bd2"},
	} {
		if c[0] != c[1] {
			t.Fatalf("%s != %s", c[0], c[1])
		}
	}
	if ChecksumAddress(vc) != "0xCcCCccccCCCCcCCCCCCcCcCccCcCCCcCcccccccC" {
		t.Fatal(ChecksumAddress(vc))
	}
}

func TestRangeChecks(t *testing.T) {
	h := keccak256([]byte("T(uint8 a,int8 b)"))
	st, err := newStructType("T", []TypeDef{{Name: "T", Fields: []FieldDef{{"a", "uint8"}, {"b", "int8"}}}}, Hex0x(h[:]), nil)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := newRefType(st)
	if err != nil {
		t.Fatal(err)
	}
	gen := &StructType{name: "T", types: st.types, hash: func(v Value) ([32]byte, error) {
		s, err := structFields(v, "T", 2)
		if err != nil {
			return [32]byte{}, err
		}
		var w [32]byte
		if err := putUint(w[:], s[0], 8); err != nil {
			return w, err
		}
		return w, putInt(w[:], s[1], 8)
	}}
	ok := func(a, b int64) bool {
		_, refErr := ref.HashStruct(Struct{big.NewInt(a), big.NewInt(b)})
		_, genErr := gen.HashStruct(Struct{big.NewInt(a), big.NewInt(b)})
		if (refErr == nil) != (genErr == nil) {
			t.Fatalf("(%d, %d): reference %v, generated %v", a, b, refErr, genErr)
		}
		return refErr == nil
	}
	if !ok(255, -128) || ok(256, 0) || ok(-1, 0) || ok(0, 128) {
		t.Fatal("range checks")
	}
	if _, err := newStructType("T", st.types, "0x00", nil); err == nil {
		t.Fatal("type hash mismatch must fail")
	}
}

func TestRedaction(t *testing.T) {
	key := "0x" + strings.Repeat("ab", 32)
	line := `{"api_key": "k-123", "r":"0x1","order":{"s" : "0x2","size":"1.5"},"pk":"` + key + `"} gravity=abc; Path=/`
	out := Redact([]byte(line), 10_000)
	if strings.Contains(out, "k-123") || strings.Contains(out, "abc;") || strings.Contains(out, key) || strings.Contains(out, `"0x1"`) {
		t.Fatal(out)
	}
	if !strings.Contains(out, `"size":"1.5"`) || !strings.Contains(out, "gravity=***;") {
		t.Fatal(out)
	}
	if redactHeader("Cookie", "gravity=x") != "***" {
		t.Fatal("cookie header")
	}
}

func TestSessionStateNeverPrintsTheCookie(t *testing.T) {
	st := SessionState{Cookie: "cookie-secret-123", AccountID: "acc-1", Generation: 2}
	var logged bytes.Buffer
	slog.New(slog.NewTextHandler(&logged, nil)).Info("session", "state", st)
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("session", "state", st)
	for _, out := range []string{fmt.Sprint(st), fmt.Sprintf("%+v", st), fmt.Sprintf("%#v", st), fmt.Sprintf("%v", &st), logged.String()} {
		if strings.Contains(out, "cookie-secret-123") || !strings.Contains(out, "<redacted>") || !strings.Contains(out, "acc-1") {
			t.Fatal(out)
		}
	}
}

func TestConfigNeverPrintsTheAPIKey(t *testing.T) {
	cfg := Config{Env: Testnet(), APIKey: "key-secret-123"}
	var logged bytes.Buffer
	slog.New(slog.NewTextHandler(&logged, nil)).Info("config", "cfg", cfg)
	for _, out := range []string{fmt.Sprint(cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg), fmt.Sprintf("%v", &cfg), logged.String()} {
		if strings.Contains(out, "key-secret-123") || !strings.Contains(out, "<redacted>") {
			t.Fatal(out)
		}
	}
}

func TestModelsDecodePartialFramesAndUnknownEnums(t *testing.T) {
	var f models.PartialWSTickerFeedDataV1
	err := json.Unmarshal([]byte(`{"stream":"v1.ticker.d","selector":"X@500","sequence_number":"7","prev_sequence_number":"6","feed":{"mark_price":"1"}}`), &f)
	if err != nil {
		t.Fatal(err)
	}
	if f.SequenceNumber != 7 || f.Feed.MarkPrice == nil {
		t.Fatalf("%+v", f)
	}
	var order models.OrderInput
	if err = json.Unmarshal([]byte(`{"sub_account_id":"1","time_in_force":"SOMETHING_NEW","legs":[]}`), &order); err != nil {
		t.Fatal(err)
	}
	if _, known := order.TimeInForce.Code(); known {
		t.Fatal("unknown enum reported as known")
	}
	order.SubAccountID = Ptr[uint64](1793172225200080949)
	order.PostOnly = Ptr(false)
	raw, err := json.Marshal(order)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["sub_account_id"] != "1793172225200080949" || wire["post_only"] != false {
		t.Fatal(string(raw))
	}
	var ids models.Uint64s
	if err := json.Unmarshal([]byte(`["1",2]`), &ids); err != nil || len(ids) != 2 || ids[1] != 2 {
		t.Fatal(ids, err)
	}
}
