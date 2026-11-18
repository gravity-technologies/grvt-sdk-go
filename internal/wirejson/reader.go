// Package wirejson is the JSON reader behind the generated model decoders (models/models_gen.go) and
// the WebSocket frame router.
//
// A decoder reads the text once, front to back, and validates every byte it passes, including values
// it skips. It accepts and rejects exactly what encoding/json accepted with the SDK's earlier
// decoders, and produces the same values:
//
//   - keys match field names exactly, then without case (encoding/json's folding);
//   - unknown keys are skipped but must still be valid JSON;
//   - a repeated key decodes again into the value already there (objects merge, lists reuse their
//     elements), as encoding/json does;
//   - strings are unescaped like encoding/json: invalid UTF-8 and lone surrogates become U+FFFD;
//   - nesting deeper than 10000 is an error.
package wirejson

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// maxDepth is encoding/json's nesting limit.
const maxDepth = 10000

// Reader reads one JSON text. The zero value is not usable; use NewReader.
type Reader struct {
	data  []byte
	pos   int
	depth int
	lite  bool
}

// NewReader reads data in the full encoding.
func NewReader(data []byte) Reader { return Reader{data: data} }

// NewLiteReader reads data in the lite encoding: the generated decoders match each type's lite keys
// (exactly, without case folding) instead of its full ones. Everything else is the same.
func NewLiteReader(data []byte) Reader { return Reader{data: data, lite: true} }

// Lite reports whether keys are lite names.
func (r *Reader) Lite() bool { return r.lite }

// Errors are the ones encoding/json gave for the same input, so callers can keep using errors.As:
//
//   - invalid JSON is the *json.SyntaxError encoding/json reports for the text;
//   - a value of the wrong kind, or a number out of range, is a *json.UnmarshalTypeError (Struct is
//     the outermost model and Field the path of JSON names from it);
//   - other errors (a missing or null required key, a bad decimal, timestamp or integer text) keep
//     their earlier error and gain the model field as context: "Order.legs: OrderLeg.size is
//     required". errors.As and errors.Is reach the original error.

// ErrNull is the error for null in a required field that is not nullable.
var ErrNull = errors.New("required field cannot be null")

// fieldError puts a model field in front of an error that has no place for one.
type fieldError struct {
	field string
	err   error
}

func (e *fieldError) Error() string { return e.field + ": " + e.err.Error() }
func (e *fieldError) Unwrap() error { return e.err }

// At records that err happened in field (a JSON name) of the model typeName. A type error gets the
// field in its path; any other error, except a syntax error, is wrapped once, at the innermost field.
// Error paths stay out of line so the generated decoders stay small.
//
//go:noinline
func At(err error, typeName, field string) error {
	switch e := err.(type) { //nolint:errorlint // only errors made here, never wrapped yet
	case *json.UnmarshalTypeError:
		if e.Field == "" {
			e.Field = field
		} else {
			e.Field = field + "." + e.Field
		}
		e.Struct = typeName
		return e
	case *json.SyntaxError, *fieldError:
		return err
	}
	return &fieldError{field: typeName + "." + field, err: err}
}

// syntax returns encoding/json's error for the text: the reader found it invalid, so encoding/json
// does too, and its error is the one callers know.
func (r *Reader) syntax() error {
	var v any
	err := json.Unmarshal(r.data, &v)
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return se
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("invalid JSON at byte %d", r.pos)
}

// Fail is the error a whole-text decode returns for err: encoding/json checks the whole text before
// decoding, so invalid JSON anywhere wins over an error found earlier in the text.
func (r *Reader) Fail(err error) error {
	var se *json.SyntaxError
	if !errors.As(err, &se) && !json.Valid(r.data) {
		return r.syntax()
	}
	return err
}

// Unexpected is the error for a value of the wrong kind for t: a *json.SyntaxError when the value is
// not valid JSON, otherwise a *json.UnmarshalTypeError.
func (r *Reader) Unexpected(t reflect.Type) error {
	r.skipWS()
	start := r.pos
	if err := r.Skip(); err != nil {
		return err
	}
	got := "number"
	switch r.data[start] {
	case '"':
		got = "string"
	case '{':
		got = "object"
	case '[':
		got = "array"
	case 't', 'f':
		got = "bool"
	case 'n':
		got = "null"
	}
	return &json.UnmarshalTypeError{Value: got, Type: t, Offset: int64(r.pos)}
}

// Unexpected is Reader.Unexpected for T.
//
//go:noinline
func Unexpected[T any](r *Reader) error { return r.Unexpected(reflect.TypeFor[T]()) }

// outOfRange is the error for a number that does not fit t.
func (r *Reader) outOfRange(lit []byte, t reflect.Type) error {
	return &json.UnmarshalTypeError{Value: "number " + string(lit), Type: t, Offset: int64(r.pos)}
}

func (r *Reader) skipWS() {
	for r.pos < len(r.data) {
		switch r.data[r.pos] {
		case ' ', '\t', '\n', '\r':
			r.pos++
		default:
			return
		}
	}
}

// Next returns the first byte of the next value (after whitespace), or 0 at the end of the input.
func (r *Reader) Next() byte {
	r.skipWS()
	if r.pos < len(r.data) {
		return r.data[r.pos]
	}
	return 0
}

// End checks that only whitespace is left.
func (r *Reader) End() error {
	r.skipWS()
	if r.pos < len(r.data) { // not Next() != 0: a NUL byte is trailing data too
		return r.syntax()
	}
	return nil
}

func (r *Reader) literal(word string) error {
	if len(r.data)-r.pos >= len(word) && string(r.data[r.pos:r.pos+len(word)]) == word {
		r.pos += len(word)
		return nil
	}
	for i := 0; i < len(word); i++ {
		if r.pos >= len(r.data) || r.data[r.pos] != word[i] {
			return r.syntax()
		}
		r.pos++
	}
	return nil
}

// Null consumes a null and reports true, or reports false and consumes nothing.
func (r *Reader) Null() (bool, error) {
	if r.Next() != 'n' {
		return false, nil
	}
	return true, r.literal("null")
}

// Bool reads true or false.
func (r *Reader) Bool() (bool, error) {
	switch r.Next() {
	case 't':
		return true, r.literal("true")
	case 'f':
		return false, r.literal("false")
	}
	return false, Unexpected[bool](r)
}

// -- containers ----------------------------------------------------------------------------------

func (r *Reader) open(c byte) error {
	if r.Next() != c {
		return r.syntax()
	}
	r.pos++
	r.depth++
	if r.depth > maxDepth {
		return r.syntax()
	}
	return nil
}

// OpenObject consumes '{'.
func (r *Reader) OpenObject() error { return r.open('{') }

// OpenArray consumes '['.
func (r *Reader) OpenArray() error { return r.open('[') }

// Member reads the next key of an open object and the ':' after it. ok is false at the closing '}',
// which it consumes. first is true for the first call after OpenObject.
func (r *Reader) Member(first bool) (key []byte, ok bool, err error) {
	c := r.Next()
	if c == '}' {
		r.pos++
		r.depth--
		return nil, false, nil
	}
	if !first {
		if c != ',' {
			return nil, false, r.syntax()
		}
		r.pos++
		c = r.Next()
	}
	if c != '"' {
		return nil, false, r.syntax()
	}
	if key, err = r.readString(); err != nil {
		return nil, false, err
	}
	if r.Next() != ':' {
		return nil, false, r.syntax()
	}
	r.pos++
	return key, true, nil
}

// Elem moves to the next element of an open array. ok is false at the closing ']', which it
// consumes.
func (r *Reader) Elem(first bool) (ok bool, err error) {
	switch c := r.Next(); {
	case c == ']':
		r.pos++
		r.depth--
		return false, nil
	case first:
	case c == ',':
		r.pos++
	default:
		return false, r.syntax()
	}
	return true, nil
}

// -- scalars -------------------------------------------------------------------------------------

// String reads a string and returns its unescaped bytes: a slice of the input when the string has no
// escapes and is valid UTF-8, otherwise a new slice.
func (r *Reader) String() ([]byte, error) {
	if r.Next() != '"' {
		return nil, Unexpected[string](r)
	}
	return r.readString()
}

// plainRun returns the first index from i where data holds a '"', a '\\', a control character or a
// non-ASCII byte, looking at 8 bytes at a time.
func plainRun(data []byte, i int) int {
	const ones, highs = 0x0101010101010101, 0x8080808080808080
	for i+8 <= len(data) {
		w := binary.LittleEndian.Uint64(data[i:])
		quote := w ^ ('"' * ones)
		slash := w ^ ('\\' * ones)
		if ((quote-ones)&^quote|(slash-ones)&^slash|(w-' '*ones)|w)&highs != 0 {
			break
		}
		i += 8
	}
	return i
}

// readString reads the string at r.pos (a '"').
func (r *Reader) readString() ([]byte, error) {
	start := r.pos + 1
	data := r.data
	i := plainRun(data, start)
	for i < len(data) {
		c := data[i]
		if c == '"' {
			r.pos = i + 1
			return data[start:i], nil
		}
		if c == '\\' || c >= utf8.RuneSelf {
			return r.readStringSlow(start, i)
		}
		if c < ' ' {
			r.pos = i
			return nil, r.syntax()
		}
		i++
	}
	r.pos = i
	return nil, r.syntax()
}

// readStringSlow finishes a string with escapes or non-ASCII bytes; data[start:i] is plain ASCII.
func (r *Reader) readStringSlow(start, i int) ([]byte, error) {
	end, escaped, err := r.scanString(i)
	if err != nil {
		return nil, err
	}
	s := r.data[start:end]
	r.pos = end + 1
	if !escaped && utf8.Valid(s) {
		return s, nil
	}
	return unquote(make([]byte, 0, len(s)), s, i-start), nil
}

// scanString validates a string from data[i] (inside the quotes) and returns the index of the
// closing quote and whether it has escapes.
func (r *Reader) scanString(i int) (end int, escaped bool, err error) {
	data := r.data
	i = plainRun(data, i)
	for i < len(data) {
		c := data[i]
		switch {
		case c == '"':
			return i, escaped, nil
		case c == '\\':
			escaped = true
			i++
			if i >= len(data) {
				r.pos = i
				return 0, false, r.syntax()
			}
			switch data[i] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				i++
			case 'u':
				i++
				for k := 0; k < 4; k++ {
					if i >= len(data) || !isHex(data[i]) {
						r.pos = i
						return 0, false, r.syntax()
					}
					i++
				}
			default:
				r.pos = i
				return 0, false, r.syntax()
			}
		case c < ' ':
			r.pos = i
			return 0, false, r.syntax()
		default:
			i++
		}
	}
	r.pos = i
	return 0, false, r.syntax()
}

func isHex(c byte) bool { return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F' }

// unquote appends the unescaped form of s (a validated string body) to b, as encoding/json does:
// invalid UTF-8 and unpaired surrogates become U+FFFD. s[:plain] needs no work.
func unquote(b, s []byte, plain int) []byte {
	b = append(b, s[:plain]...)
	r := plain
	for r < len(s) {
		c := s[r]
		switch {
		case c == '\\':
			r++
			switch s[r] {
			case '"', '\\', '/', '\'':
				b = append(b, s[r])
				r++
			case 'b':
				b = append(b, '\b')
				r++
			case 'f':
				b = append(b, '\f')
				r++
			case 'n':
				b = append(b, '\n')
				r++
			case 'r':
				b = append(b, '\r')
				r++
			case 't':
				b = append(b, '\t')
				r++
			case 'u':
				r--
				rr := getu4(s[r:])
				r += 6
				if utf16.IsSurrogate(rr) {
					rr1 := getu4(s[r:])
					if dec := utf16.DecodeRune(rr, rr1); dec != unicode.ReplacementChar {
						r += 6
						b = utf8.AppendRune(b, dec)
						break
					}
					rr = unicode.ReplacementChar
				}
				b = utf8.AppendRune(b, rr)
			}
		case c < utf8.RuneSelf:
			b = append(b, c)
			r++
		default:
			rr, size := utf8.DecodeRune(s[r:])
			r += size
			b = utf8.AppendRune(b, rr)
		}
	}
	return b
}

func getu4(s []byte) rune {
	if len(s) < 6 || s[0] != '\\' || s[1] != 'u' {
		return -1
	}
	var r rune
	for _, c := range s[2:6] {
		switch {
		case '0' <= c && c <= '9':
			c -= '0'
		case 'a' <= c && c <= 'f':
			c = c - 'a' + 10
		case 'A' <= c && c <= 'F':
			c = c - 'A' + 10
		default:
			return -1
		}
		r = r*16 + rune(c)
	}
	return r
}

// Number reads a number and returns its text.
func (r *Reader) Number() ([]byte, error) {
	c := r.Next()
	if c != '-' && (c < '0' || c > '9') {
		return nil, Unexpected[float64](r)
	}
	return r.number()
}

func (r *Reader) number() ([]byte, error) {
	data := r.data
	start := r.pos
	i := start
	if data[i] == '-' {
		i++
	}
	switch {
	case i < len(data) && data[i] == '0':
		i++
	case i < len(data) && '1' <= data[i] && data[i] <= '9':
		for i < len(data) && '0' <= data[i] && data[i] <= '9' {
			i++
		}
	default:
		r.pos = i
		return nil, r.numberErr()
	}
	if i < len(data) && data[i] == '.' {
		i++
		if i >= len(data) || data[i] < '0' || data[i] > '9' {
			r.pos = i
			return nil, r.numberErr()
		}
		for i < len(data) && '0' <= data[i] && data[i] <= '9' {
			i++
		}
	}
	if i < len(data) && (data[i] == 'e' || data[i] == 'E') {
		i++
		if i < len(data) && (data[i] == '+' || data[i] == '-') {
			i++
		}
		if i >= len(data) || data[i] < '0' || data[i] > '9' {
			r.pos = i
			return nil, r.numberErr()
		}
		for i < len(data) && '0' <= data[i] && data[i] <= '9' {
			i++
		}
	}
	r.pos = i
	return data[start:i], nil
}

func (r *Reader) numberErr() error { return r.syntax() }

// ValidNumber reports whether s is a JSON number.
func ValidNumber(s []byte) bool {
	if len(s) == 0 {
		return false
	}
	r := Reader{data: s}
	_, err := r.number()
	return err == nil && r.pos == len(s)
}

// -- skipping ------------------------------------------------------------------------------------

// Skip validates and passes over one value.
func (r *Reader) Skip() error {
	switch c := r.Next(); c {
	case '{', '[':
		return r.skipContainer()
	}
	return r.skipNested()
}

// skipNested is Skip by recursion; skipContainer falls back to it for very deep values.
func (r *Reader) skipNested() error {
	switch c := r.Next(); c {
	case '"':
		end, _, err := r.scanString(r.pos + 1)
		if err != nil {
			return err
		}
		r.pos = end + 1
		return nil
	case '{':
		if err := r.OpenObject(); err != nil {
			return err
		}
		for first := true; ; first = false {
			_, ok, err := r.Member(first)
			if err != nil || !ok {
				return err
			}
			if err := r.skipNested(); err != nil {
				return err
			}
		}
	case '[':
		if err := r.OpenArray(); err != nil {
			return err
		}
		for first := true; ; first = false {
			ok, err := r.Elem(first)
			if err != nil || !ok {
				return err
			}
			if err := r.skipNested(); err != nil {
				return err
			}
		}
	case 't':
		return r.literal("true")
	case 'f':
		return r.literal("false")
	case 'n':
		return r.literal("null")
	default:
		if c == '-' || '0' <= c && c <= '9' {
			_, err := r.number()
			return err
		}
		return r.syntax()
	}
}

// AppendRaw validates one value and appends its text to out.
func (r *Reader) AppendRaw(out []byte) ([]byte, error) {
	raw, err := r.Raw()
	if err != nil {
		return nil, err
	}
	return append(out, raw...), nil
}

// Raw validates one value and returns its text.
func (r *Reader) Raw() ([]byte, error) {
	r.skipWS()
	start := r.pos
	if err := r.Skip(); err != nil {
		return nil, err
	}
	return r.data[start:r.pos], nil
}

// -- keys ----------------------------------------------------------------------------------------

// Fold appends key folded as encoding/json folds field names (ASCII upper case, other runes to the
// smallest rune of their case-fold set), so two keys match without case when their folds are equal.
func Fold(out, key []byte) []byte {
	for i := 0; i < len(key); {
		if c := key[i]; c < utf8.RuneSelf {
			if 'a' <= c && c <= 'z' {
				c -= 'a' - 'A'
			}
			out = append(out, c)
			i++
			continue
		}
		r, n := utf8.DecodeRune(key[i:])
		out = utf8.AppendRune(out, foldRune(r))
		i += n
	}
	return out
}

func foldRune(r rune) rune {
	for {
		r2 := unicode.SimpleFold(r)
		if r2 <= r {
			return r2
		}
		r = r2
	}
}

func skipSpace(data []byte, i int) int {
	for i < len(data) && (data[i] == ' ' || data[i] == '\n' || data[i] == '\t' || data[i] == '\r') {
		i++
	}
	return i
}

// skipContainer is Skip for an object or array (at r.pos) as one loop: the open containers are a bit
// stack (1 = object), up to 64 deep; anything deeper goes through skipNested.
func (r *Reader) skipContainer() error {
	data := r.data
	start := r.pos
	i := start
	depth := 0
	var objects uint64
	needValue := true // i is at the start of a value
	for {
		if needValue {
			if i >= len(data) {
				r.pos = i
				return r.syntax()
			}
			switch c := data[i]; c {
			case '{', '[':
				if depth == 64 {
					r.pos = start
					return r.skipNested()
				}
				if r.depth+depth+1 > maxDepth {
					r.pos = i
					return r.syntax()
				}
				objects = objects<<1 | b2u(c == '{')
				depth++
				i = skipSpace(data, i+1)
				if i < len(data) && (c == '{' && data[i] == '}' || c == '[' && data[i] == ']') {
					i++
					depth--
					objects >>= 1
					needValue = false
					break
				}
				if c == '{' {
					var err error
					if i, err = r.skipKey(i); err != nil {
						return err
					}
				}
				continue
			case '"':
				end, _, err := r.scanString(i + 1)
				if err != nil {
					return err
				}
				i = end + 1
			case 't', 'f', 'n':
				r.pos = i
				word := "null"
				switch c {
				case 't':
					word = "true"
				case 'f':
					word = "false"
				}
				if err := r.literal(word); err != nil {
					return err
				}
				i = r.pos
			default:
				r.pos = i
				if c != '-' && (c < '0' || c > '9') {
					return r.syntax()
				}
				if _, err := r.number(); err != nil {
					return err
				}
				i = r.pos
			}
			needValue = false
		}
		// after a value
		if depth == 0 {
			r.pos = i
			return nil
		}
		i = skipSpace(data, i)
		if i >= len(data) {
			r.pos = i
			return r.syntax()
		}
		inObject := objects&1 == 1
		switch c := data[i]; {
		case c == ',':
			i = skipSpace(data, i+1)
			if inObject {
				var err error
				if i, err = r.skipKey(i); err != nil {
					return err
				}
			}
			needValue = true
		case c == '}' && inObject, c == ']' && !inObject:
			i++
			depth--
			objects >>= 1
		default:
			r.pos = i
			return r.syntax()
		}
	}
}

// skipKey passes over `"key" :` and the space after it, from data[i].
func (r *Reader) skipKey(i int) (int, error) {
	data := r.data
	if i >= len(data) || data[i] != '"' {
		r.pos = i
		return 0, r.syntax()
	}
	end, _, err := r.scanString(i + 1)
	if err != nil {
		return 0, err
	}
	i = skipSpace(data, end+1)
	if i >= len(data) || data[i] != ':' {
		r.pos = i
		return 0, r.syntax()
	}
	return skipSpace(data, i+1), nil
}

func b2u(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

// FoldedIndex returns the index of the name in folded (names already folded) that key matches without
// case, or -1. It is the slow path after an exact match failed.
func FoldedIndex(key []byte, folded []string) int {
	var buf [64]byte
	k := Fold(buf[:0], key)
	for i, name := range folded {
		if string(k) == name {
			return i
		}
	}
	return -1
}
