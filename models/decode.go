package models

import (
	"errors"
	"fmt"

	"github.com/gravity-technologies/grvt-sdk-go/internal/wirejson"
)

// Hand-written pieces of the generated decoders (decode<T> in models_gen.go).

func missingJSONField(name string) error { return fmt.Errorf("%s is required", name) }

var errDecimalNull = errors.New("decimal cannot be null")

// decodeDecimal: a decimal string; null is an error.
func decodeDecimal(r *wirejson.Reader, dst *Decimal) error {
	switch r.Next() {
	case '"':
		b, err := r.String()
		if err != nil {
			return err
		}
		if !validDecimal(b) {
			_, err := ParseDecimal(string(b))
			return err
		}
		dst.text = string(b)
		return nil
	case 'n':
		if _, err := r.Null(); err != nil {
			return err
		}
		return errDecimalNull
	}
	return wirejson.Unexpected[string](r)
}

// decodeDecimalPtr: null sets nil.
func decodeDecimalPtr(r *wirejson.Reader, dst **Decimal) error {
	if r.Next() == 'n' {
		*dst = nil
		_, err := r.Null()
		return err
	}
	if *dst == nil {
		*dst = new(Decimal)
	}
	return decodeDecimal(r, *dst)
}

// decodeDecimalList: null is nil; items already there are decoded into again.
func decodeDecimalList(r *wirejson.Reader, dst *[]Decimal) error {
	switch r.Next() {
	case 'n':
		*dst = nil
		_, err := r.Null()
		return err
	case '[':
	default:
		return wirejson.Unexpected[[]Decimal](r)
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
		s = wirejson.Grow(s, n)
		if err := decodeDecimal(r, &s[n]); err != nil {
			return err
		}
	}
	*dst = wirejson.Trim(s, n)
	return nil
}
