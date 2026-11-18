package wirejson

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"time"
	"unsafe"
)

// Value decoders for the generated code. Each one decodes the next value into *dst in place, with
// the rules of the encoding/json path it replaces (noted per function).

// DecodeString: a string; null keeps the old value.
func DecodeString[S ~string](r *Reader, dst *S) error {
	switch r.Next() {
	case '"':
		b, err := r.readString()
		if err != nil {
			return err
		}
		*dst = S(b)
		return nil
	case 'n':
		return r.literal("null")
	}
	return Unexpected[S](r)
}

// DecodeBool: true or false; null keeps the old value.
func DecodeBool(r *Reader, dst *bool) error {
	switch r.Next() {
	case 't':
		*dst = true
		return r.literal("true")
	case 'f':
		*dst = false
		return r.literal("false")
	case 'n':
		return r.literal("null")
	}
	return Unexpected[bool](r)
}

// DecodeBoolPtr: null sets nil.
func DecodeBoolPtr(r *Reader, dst **bool) error {
	if r.Next() == 'n' {
		*dst = nil
		return r.literal("null")
	}
	if *dst == nil {
		*dst = new(bool)
	}
	return DecodeBool(r, *dst)
}

type unsigned interface {
	~uint8 | ~uint16 | ~uint32 | ~uint64
}

type signed interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64
}

// parseUint is strconv.ParseUint(s, 10, bits) for the common all-digits case.
func parseUint(s []byte, bits int) (uint64, bool) {
	if len(s) > 0 && len(s) < 20 {
		var n uint64
		for _, c := range s {
			if c < '0' || c > '9' {
				goto slow
			}
			n = n*10 + uint64(c-'0')
		}
		return n, bits == 64 || n>>uint(bits) == 0
	}
slow:
	n, err := strconv.ParseUint(string(s), 10, bits)
	return n, err == nil
}

func parseInt(s []byte, bits int) (int64, bool) {
	n, err := strconv.ParseInt(string(s), 10, bits)
	return n, err == nil
}

// DecodeUint: a JSON number that is an integer in range (strconv.ParseUint of its text); null keeps
// the old value.
func DecodeUint[T unsigned](r *Reader, dst *T) error {
	switch c := r.Next(); {
	case c == 'n':
		return r.literal("null")
	case c == '-' || '0' <= c && c <= '9':
		lit, err := r.number()
		if err != nil {
			return err
		}
		n, ok := parseUint(lit, int(unsafe.Sizeof(*dst))*8)
		if !ok {
			return r.outOfRange(lit, reflect.TypeFor[T]())
		}
		*dst = T(n)
		return nil
	}
	return Unexpected[T](r)
}

// DecodeUintPtr: null sets nil.
func DecodeUintPtr[T unsigned](r *Reader, dst **T) error {
	if r.Next() == 'n' {
		*dst = nil
		return r.literal("null")
	}
	if *dst == nil {
		*dst = new(T)
	}
	return DecodeUint(r, *dst)
}

// DecodeInt is DecodeUint for signed integers.
func DecodeInt[T signed](r *Reader, dst *T) error {
	switch c := r.Next(); {
	case c == 'n':
		return r.literal("null")
	case c == '-' || '0' <= c && c <= '9':
		lit, err := r.number()
		if err != nil {
			return err
		}
		n, ok := parseInt(lit, int(unsafe.Sizeof(*dst))*8)
		if !ok {
			return r.outOfRange(lit, reflect.TypeFor[T]())
		}
		*dst = T(n)
		return nil
	}
	return Unexpected[T](r)
}

// DecodeIntPtr: null sets nil.
func DecodeIntPtr[T signed](r *Reader, dst **T) error {
	if r.Next() == 'n' {
		*dst = nil
		return r.literal("null")
	}
	if *dst == nil {
		*dst = new(T)
	}
	return DecodeInt(r, *dst)
}

// trimSpace trims JSON whitespace.
func trimSpace(s []byte) []byte {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 {
		c := s[len(s)-1]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

// quotedText reads the string of a required 64-bit integer: the string's text must itself be one JSON
// value. null is nil, text nil (the old value stays). On a failure the error is the one json.Unmarshal
// gives for the text, as before.
func quotedText[T ~uint64 | ~int64](r *Reader) (text []byte, err error) {
	if r.Next() != '"' {
		return nil, Unexpected[string](r)
	}
	b, err := r.readString()
	if err != nil {
		return nil, err
	}
	t := trimSpace(b)
	if string(t) == "null" {
		return nil, nil
	}
	if !ValidNumber(t) {
		return nil, quotedError[T](b)
	}
	return t, nil
}

func quotedError[T any](text []byte) error {
	var v T
	if err := json.Unmarshal(text, &v); err != nil {
		return err
	}
	return fmt.Errorf("invalid integer %q", text)
}

// DecodeQuotedUint is a required uint64: a string holding a JSON integer (whitespace around it is
// allowed); "null" in the string keeps the old value.
func DecodeQuotedUint[T ~uint64](r *Reader, dst *T) error {
	t, err := quotedText[T](r)
	if err != nil || t == nil {
		return err
	}
	n, ok := parseUint(t, 64)
	if !ok {
		return quotedError[T](t)
	}
	*dst = T(n)
	return nil
}

// DecodeQuotedInt is DecodeQuotedUint for int64.
func DecodeQuotedInt[T ~int64](r *Reader, dst *T) error {
	t, err := quotedText[T](r)
	if err != nil || t == nil {
		return err
	}
	n, ok := parseInt(t, 64)
	if !ok {
		return quotedError[T](t)
	}
	*dst = T(n)
	return nil
}

// stringTagText reads an optional 64-bit integer (encoding/json's ",string" option): null or "null"
// set nil (ok false); otherwise the string must start with '-' or a digit. A bad text is a
// *json.UnmarshalTypeError, as Go 1.27's encoding/json reports it.
func (r *Reader) stringTagText(t reflect.Type) (text []byte, ok bool, err error) {
	switch r.Next() {
	case 'n':
		return nil, false, r.literal("null")
	case '"':
		b, err := r.readString()
		if err != nil {
			return nil, false, err
		}
		if string(b) == "null" {
			return nil, false, nil
		}
		if len(b) == 0 || b[0] != '-' && (b[0] < '0' || b[0] > '9') {
			return nil, false, r.outOfRange(b, t)
		}
		return b, true, nil
	}
	if err := r.Skip(); err != nil {
		return nil, false, err
	}
	return nil, false, fmt.Errorf("json: invalid use of ,string struct tag, trying to unmarshal unquoted value into %v", t)
}

// DecodeStringUintPtr is an optional uint64 sent as a string.
func DecodeStringUintPtr[T ~uint64](r *Reader, dst **T) error {
	t, ok, err := r.stringTagText(reflect.TypeFor[T]())
	if err != nil {
		return err
	}
	if !ok {
		*dst = nil
		return nil
	}
	n, fits := parseUint(t, 64)
	if !fits {
		return r.outOfRange(t, reflect.TypeFor[T]())
	}
	if *dst == nil {
		*dst = new(T)
	}
	**dst = T(n)
	return nil
}

// DecodeStringIntPtr is DecodeStringUintPtr for int64.
func DecodeStringIntPtr[T ~int64](r *Reader, dst **T) error {
	t, ok, err := r.stringTagText(reflect.TypeFor[T]())
	if err != nil {
		return err
	}
	if !ok {
		*dst = nil
		return nil
	}
	n, fits := parseInt(t, 64)
	if !fits {
		return r.outOfRange(t, reflect.TypeFor[T]())
	}
	if *dst == nil {
		*dst = new(T)
	}
	**dst = T(n)
	return nil
}

// DecodeTime is a timestamp: a string of integer nanoseconds (strconv.ParseInt, so a leading '+' is
// allowed), as UTC. A bad text gives strconv.ParseInt's error, as before.
func DecodeTime(r *Reader, dst *time.Time) error {
	var b []byte
	switch r.Next() {
	case '"':
		var err error
		if b, err = r.readString(); err != nil {
			return err
		}
	case 'n':
		if err := r.literal("null"); err != nil {
			return err
		}
	default:
		return Unexpected[string](r)
	}
	ns, ok := parseInt(b, 64)
	if !ok {
		_, err := strconv.ParseInt(string(b), 10, 64)
		return err
	}
	*dst = time.Unix(0, ns).UTC()
	return nil
}

// DecodeTimePtr: null sets nil.
func DecodeTimePtr(r *Reader, dst **time.Time) error {
	if r.Next() == 'n' {
		*dst = nil
		return r.literal("null")
	}
	if *dst == nil {
		*dst = new(time.Time)
	}
	return DecodeTime(r, *dst)
}

// DecodeRaw copies any value's text (null is kept as "null").
func DecodeRaw(r *Reader, dst *json.RawMessage) error {
	raw, err := r.Raw()
	if err != nil {
		return err
	}
	*dst = append((*dst)[0:0], raw...)
	return nil
}

// DecodeUint64s is a list of 64-bit integers, each a string or a number holding only digits (the
// string is not unescaped). null is an empty list. The list is always replaced.
func DecodeUint64s[S ~[]uint64](r *Reader, dst *S) error {
	switch r.Next() {
	case 'n':
		*dst = make(S, 0)
		return r.literal("null")
	case '[':
	default:
		return Unexpected[S](r)
	}
	if err := r.OpenArray(); err != nil {
		return err
	}
	out := make(S, 0)
	for first := true; ; first = false {
		more, err := r.Elem(first)
		if err != nil {
			return err
		}
		if !more {
			break
		}
		raw, err := r.Raw()
		if err != nil {
			return err
		}
		s := raw
		if s[0] == '"' {
			s = s[1 : len(s)-1]
		}
		n, ok := parseUint(s, 64)
		if !ok {
			_, err := strconv.ParseUint(string(s), 10, 64)
			return err
		}
		out = append(out, n)
	}
	*dst = out
	return nil
}

// Grow makes s[i] addressable for decoding list item i, as encoding/json does: an item already there
// is decoded into again.
func Grow[E any](s []E, i int) []E {
	if i >= cap(s) { // here len(s) == cap(s) == i
		var zero E
		if cap(s) == 0 && unsafe.Sizeof(zero) <= 64 {
			return make([]E, 1, 4) // small items: skip the 1 and 2 steps
		}
		return append(s, zero)
	}
	if i >= len(s) {
		return s[:i+1]
	}
	return s
}

// Trim ends a list after n items; an empty list is a new empty slice.
func Trim[E any](s []E, n int) []E {
	if n == 0 {
		return make([]E, 0)
	}
	return s[:n]
}

// -- registry ------------------------------------------------------------------------------------

var (
	registry     sync.Map // reflect.Type -> func([]byte, *T) error
	liteRegistry sync.Map // reflect.Type -> liteEntry[T]
)

// Register records the generated decoder of a model type.
func Register[T any](unmarshal func([]byte, *T) error) {
	registry.Store(reflect.TypeFor[T](), unmarshal)
}

// Lookup returns the generated decoder of T, or nil.
func Lookup[T any]() func([]byte, *T) error {
	f, _ := registry.Load(reflect.TypeFor[T]())
	fn, _ := f.(func([]byte, *T) error)
	return fn
}

// Unmarshal decodes data into v with T's generated decoder, or with encoding/json when T has none.
func Unmarshal[T any](data []byte, v *T) error {
	if f := Lookup[T](); f != nil {
		return f(data, v)
	}
	return json.Unmarshal(data, v)
}

// LiteEncoder appends the lite form of the full-encoding value at r to out (generated liteFrom<T>).
type LiteEncoder func(r *Reader, out []byte) ([]byte, error)

type liteEntry[T any] struct {
	unmarshal func([]byte, *T) error
	toLite    func([]byte) ([]byte, error)
}

func lookupLite[T any]() (liteEntry[T], error) {
	e, ok := liteRegistry.Load(reflect.TypeFor[T]())
	if !ok {
		return liteEntry[T]{}, fmt.Errorf("%v: %w", reflect.TypeFor[T](), ErrNoLite)
	}
	entry, ok := e.(liteEntry[T])
	if !ok {
		return liteEntry[T]{}, fmt.Errorf("%v: %w", reflect.TypeFor[T](), ErrNoLite)
	}
	return entry, nil
}

// RegisterLite records the generated lite codec of a model type: its lite decoder and its full-to-lite
// renamer.
func RegisterLite[T any](unmarshal func([]byte, *T) error, toLite func([]byte) ([]byte, error)) {
	liteRegistry.Store(reflect.TypeFor[T](), liteEntry[T]{unmarshal: unmarshal, toLite: toLite})
}

// ErrNoLite is the error for a type with no lite form (edge types, or any type that is not a model).
var ErrNoLite = errors.New("type has no lite encoding")

// UnmarshalLite decodes lite-encoding data into v with T's generated decoder. There is no
// encoding/json fallback: a type without one is ErrNoLite.
func UnmarshalLite[T any](data []byte, v *T) error {
	e, err := lookupLite[T]()
	if err != nil {
		return err
	}
	return e.unmarshal(data, v)
}

// ToLite rewrites full, the full-encoding text of a T (as encoding/json writes it), into the lite
// encoding: same values, each key renamed by its type. A key T has no lite name for is an error.
func ToLite[T any](full []byte) ([]byte, error) {
	e, err := lookupLite[T]()
	if err != nil {
		return nil, err
	}
	return e.toLite(full)
}

// FinishLite runs a generated lite encoder over the whole text r reads (full is that text).
func FinishLite(r *Reader, enc LiteEncoder, full []byte) ([]byte, error) {
	out, err := enc(r, make([]byte, 0, len(full)))
	if err != nil {
		return nil, r.Fail(err)
	}
	if err := r.End(); err != nil {
		return nil, err
	}
	return out, nil
}

// LiteList is a lite encoder for a list of items that each have encoder item (null is copied).
func LiteList(r *Reader, out []byte, item LiteEncoder) ([]byte, error) {
	if r.Next() != '[' {
		return r.AppendRaw(out)
	}
	if err := r.OpenArray(); err != nil {
		return nil, err
	}
	out = append(out, '[')
	for first := true; ; first = false {
		more, err := r.Elem(first)
		if err != nil {
			return nil, err
		}
		if !more {
			return append(out, ']'), nil
		}
		if !first {
			out = append(out, ',')
		}
		if out, err = item(r, out); err != nil {
			return nil, err
		}
	}
}

// NoLiteName is the error for a key the type typeName has no lite name for.
//
//go:noinline
func NoLiteName(typeName string, key []byte) error {
	return fmt.Errorf("%s has no lite name for key %q", typeName, key)
}

// DecodeStringList is a list of strings (null items keep the item already there); null is nil.
func DecodeStringList[S ~string](r *Reader, dst *[]S) error {
	switch r.Next() {
	case 'n':
		*dst = nil
		return r.literal("null")
	case '[':
	default:
		return Unexpected[[]S](r)
	}
	if err := r.OpenArray(); err != nil {
		return err
	}
	s, n := *dst, 0
	for ; ; n++ {
		more, err := r.Elem(n == 0)
		if err != nil {
			return err
		}
		if !more {
			break
		}
		s = Grow(s, n)
		if err := DecodeString(r, &s[n]); err != nil {
			return err
		}
	}
	*dst = Trim(s, n)
	return nil
}

// DecodeUintList is DecodeStringList for small unsigned integers.
func DecodeUintList[T unsigned](r *Reader, dst *[]T) error {
	switch r.Next() {
	case 'n':
		*dst = nil
		return r.literal("null")
	case '[':
	default:
		return Unexpected[[]T](r)
	}
	if err := r.OpenArray(); err != nil {
		return err
	}
	s, n := *dst, 0
	for ; ; n++ {
		more, err := r.Elem(n == 0)
		if err != nil {
			return err
		}
		if !more {
			break
		}
		s = Grow(s, n)
		if err := DecodeUint(r, &s[n]); err != nil {
			return err
		}
	}
	*dst = Trim(s, n)
	return nil
}

// DecodeAny decodes the next value as json.Unmarshal into an `any` does: map[string]any (a repeated
// key keeps its last value), []any, string, float64, bool or nil.
func DecodeAny(r *Reader) (any, error) {
	switch c := r.Next(); c {
	case '{':
		if err := r.OpenObject(); err != nil {
			return nil, err
		}
		m := map[string]any{}
		for first := true; ; first = false {
			key, ok, err := r.Member(first)
			if err != nil {
				return nil, err
			}
			if !ok {
				return m, nil
			}
			k := string(key)
			if m[k], err = DecodeAny(r); err != nil {
				return nil, err
			}
		}
	case '[':
		if err := r.OpenArray(); err != nil {
			return nil, err
		}
		list := []any{}
		for first := true; ; first = false {
			more, err := r.Elem(first)
			if err != nil {
				return nil, err
			}
			if !more {
				return list, nil
			}
			v, err := DecodeAny(r)
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
	case '"':
		b, err := r.readString()
		return string(b), err
	case 't', 'f':
		return r.Bool()
	case 'n':
		_, err := r.Null()
		return nil, err
	default:
		lit, err := r.Number()
		if err != nil {
			return nil, err
		}
		f, err := strconv.ParseFloat(string(lit), 64)
		if err != nil {
			return nil, r.outOfRange(lit, reflect.TypeFor[float64]())
		}
		return f, nil
	}
}
