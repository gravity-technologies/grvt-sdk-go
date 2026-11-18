package models

import (
	"encoding/json"
	"fmt"
	"math/big"
)

// Decimal carries an exact decimal without exposing an arithmetic library in the API.
// Its zero value is zero. Use ParseDecimal for other values.
type Decimal struct{ text string }

func ParseDecimal(text string) (Decimal, error) {
	if !validDecimal([]byte(text)) {
		return Decimal{}, fmt.Errorf("invalid decimal %q: want a finite base-10 string without an exponent", text)
	}
	return Decimal{text: text}, nil
}

func (d Decimal) String() string {
	if d.text == "" {
		return "0"
	}
	return d.text
}

// Rat returns an independent exact rational value for callers that need arithmetic.
func (d Decimal) Rat() *big.Rat {
	value, _ := new(big.Rat).SetString(d.String())
	return value
}

func (d Decimal) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func (d *Decimal) UnmarshalJSON(data []byte) error {
	var text string
	if string(data) == "null" {
		return fmt.Errorf("decimal cannot be null")
	}
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	value, err := ParseDecimal(text)
	if err != nil {
		return err
	}
	*d = value
	return nil
}

// MustParseDecimal is ParseDecimal for constants. It panics on invalid input.
func MustParseDecimal(text string) Decimal {
	value, err := ParseDecimal(text)
	if err != nil {
		panic(err)
	}
	return value
}

// validDecimal matches -?(0|[1-9][0-9]*)(\.[0-9]+)? exactly.
func validDecimal(s []byte) bool {
	i := 0
	if i < len(s) && s[i] == '-' {
		i++
	}
	switch {
	case i < len(s) && s[i] == '0':
		i++
	case i < len(s) && '1' <= s[i] && s[i] <= '9':
		for i < len(s) && '0' <= s[i] && s[i] <= '9' {
			i++
		}
	default:
		return false
	}
	if i < len(s) && s[i] == '.' {
		i++
		if i >= len(s) || s[i] < '0' || s[i] > '9' {
			return false
		}
		for i < len(s) && '0' <= s[i] && s[i] <= '9' {
			i++
		}
	}
	return i == len(s)
}
