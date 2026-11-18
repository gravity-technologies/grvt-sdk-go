package models

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// The decoders the models had before the one-pass decoders, kept as the reference for the differential
// test (decode_diff_test.go). legacy_gen_test.go holds one legacy<T> per model built on these.

// requiredField distinguishes an omitted key from an explicit nullable value.
type requiredField[T any] struct {
	value    T
	present  bool
	nullable bool
}

func (f *requiredField[T]) UnmarshalJSON(data []byte) error {
	f.present = true
	if !f.nullable && bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return fmt.Errorf("required field cannot be null")
	}
	return json.Unmarshal(data, &f.value)
}

// Wide integers are JSON strings; the outer presence wrapper is not a primitive,
// so encoding/json cannot apply its usual ,string conversion to it.
type quotedInteger[T ~uint64 | ~int64] struct{ value T }

func (n *quotedInteger[T]) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	return json.Unmarshal([]byte(text), &n.value)
}

// legacyModel is one model in the generated table: its contract fields, a new zero value of the model
// and of its legacy twin, and the model's one-pass decoder.
type legacyModel struct {
	name   string
	fields []legacyField
	fresh  func() (current, legacy any)
	fast   func([]byte, any) error
	// lite decodes lite text and toLite turns full text into lite (nil for edge types).
	lite   func([]byte, any) error
	toLite func([]byte) ([]byte, error)
}

type legacyField struct {
	name, lite, typ, kind    string
	list, optional, nullable bool
}
