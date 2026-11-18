package models

import (
	"encoding/json"
	"strconv"
)

// Hand-written wire helpers for the generated models (the generator owns only *_gen.go files).

// Uint64s is a list of 64-bit integers. The gateway sends them as JSON strings; numbers decode too.
type Uint64s []uint64

func (u Uint64s) MarshalJSON() ([]byte, error) {
	out := make([]string, len(u))
	for i, v := range u {
		out[i] = strconv.FormatUint(v, 10)
	}
	return json.Marshal(out)
}

func (u *Uint64s) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	out := make(Uint64s, 0, len(raw))
	for _, r := range raw {
		s := string(r)
		if len(s) >= 2 && s[0] == '"' {
			s = s[1 : len(s)-1]
		}
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return err
		}
		out = append(out, n)
	}
	*u = out
	return nil
}
