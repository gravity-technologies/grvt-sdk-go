package grvt

import (
	"encoding/json"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
)

// legacyEnvelope is the frame envelope the read loop decoded with encoding/json before decodeEnvelope.
type legacyEnvelope struct {
	ID       uint32          `json:"id"`
	Stream   string          `json:"stream"`
	Selector string          `json:"selector"`
	Seq      json.RawMessage `json:"sequence_number"`
	Prev     json.RawMessage `json:"prev_sequence_number"`
	Result   json.RawMessage `json:"result"`
	Error    json.RawMessage `json:"error"`
	Code     int             `json:"code"`
	Message  string          `json:"message"`
}

// The one-pass router accepts, rejects and reads frames exactly as encoding/json did.
func TestEnvelopeMatchesEncodingJSON(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // reproducible test data
	pick := func(xs ...string) string { return xs[rng.IntN(len(xs))] }
	value := func(key string) string {
		switch key {
		case "id", "code", "ID", "Code":
			return pick("0", "7", "4294967295", "4294967296", "-1", "1.5", `"7"`, "null", "1e2", "-9223372036854775808", "9223372036854775808")
		case "stream", "selector", "message", "Stream", "SELECTOR", "meſſage":
			return pick(`"v1.book.d"`, `"BTC_USDT_Perp@500"`, `""`, `"\u0041b"`, "null", "1", "\"\xff\"", `"\ud800"`, `"\u0041b"`)
		case "error", "Error":
			return pick("null", `{"code":1006,"message":"rate"}`, `{"CODE":"x"}`, `{"code":5,"code":null}`, "[]", `{"message":1}`, `{"x":[1,{"y":2}]}`, `"x"`)
		}
		return pick("null", `"123"`, "123", `{"a":[1,2,{"b":null}]}`, "[]", `"x"`, "true", "-0.5e3", "[1,]", `{"a"}`, "01")
	}
	keys := []string{"id", "stream", "selector", "sequence_number", "prev_sequence_number", "result", "error", "code", "message",
		"ID", "Stream", "SELECTOR", "Code", "Error", "meſſage", "feed", "unknown"}
	frames := []string{bookString(), "null", "{}", "[]", `"x"`, "", "{", "{} x", `{"stream":"a",}`}
	for i := 0; i < 20000; i++ {
		var parts []string
		for n := rng.IntN(6); n >= 0; n-- {
			k := keys[rng.IntN(len(keys))]
			parts = append(parts, `"`+k+`":`+value(k))
		}
		frame := "{" + strings.Join(parts, ",") + "}"
		if rng.IntN(10) == 0 {
			frame = frame[:rng.IntN(len(frame))]
		}
		frames = append(frames, frame)
	}
	for _, frame := range frames {
		var old legacyEnvelope
		errOld := json.Unmarshal([]byte(frame), &old)
		var cur envelope
		errCur := decodeEnvelope([]byte(frame), &cur)
		if (errOld == nil) != (errCur == nil) {
			t.Fatalf("accept/reject differs on %q: encoding/json %v, router %v", frame, errOld, errCur)
		}
		if errOld != nil {
			continue
		}
		got := legacyEnvelope{ID: cur.ID, Stream: string(cur.Stream), Selector: string(cur.Selector), Seq: cur.Seq, Prev: cur.Prev,
			Result: cur.Result, Code: cur.Code, Message: cur.Message}
		if cur.Error != nil {
			got.Error = cur.Error.Body
		}
		if string(old.Error) == "null" {
			old.Error = nil
		}

		if !reflect.DeepEqual(old, got) {
			t.Fatalf("frame %q:\nencoding/json %+v\nrouter        %+v", frame, old, got)
		}
	}
}

func bookString() string { return string(bookFrame(3)) }
