package grvt

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
)

// Order checks: instrument limits (tick, minimum size, size step, minimum notional) checked before
// signing. The rules come from the contract (order_checks) through the generated orderChecks table; this
// file only applies them. Values are compared as exact integers, scaled like the signed fields. Nothing
// is ever rounded or changed: a failing order returns *OrderOutOfBoundsError with the nearest valid values.

// OrderCheckRule is one contract rule (contract.json order_checks.rules).
type OrderCheckRule struct {
	Field, Times, Check, Limit string
	SkipIf                     []string
	ServerCode                 int
}

// OrderChecks is the generated table. A scale is fixed decimals, or -1 for the instrument's base_decimals.
type OrderChecks struct {
	Instrument string
	Scales     map[string]int
	Rules      []OrderCheckRule
}

// BaseDecimals marks a field scaled by the instrument's base_decimals in OrderChecks.Scales.
const BaseDecimals = -1

var u64Max = new(big.Int).SetUint64(^uint64(0))

func checkLeg(checks *OrderChecks, skip map[string]bool, leg map[string]string, index int, meta *Meta) error {
	name := leg[checks.Instrument]
	inst, err := meta.Instrument(name)
	if err != nil {
		return err
	}
	decimals := func(field string) uint32 {
		if s := checks.Scales[field]; s != BaseDecimals {
			return uint32(s)
		}
		return inst.BaseDecimals
	}
	limit := func(field string, d uint32) (*big.Int, error) {
		raw := inst.Limits[field]
		if raw == "" {
			return new(big.Int), nil
		}
		n, err := ScaleDecimal(raw, d)
		if err != nil {
			return nil, &SigningError{Message: fmt.Sprintf("instrument %s %s: %v", name, field, err)}
		}
		return n, nil
	}
	skipped := func(r OrderCheckRule) bool {
		for _, p := range r.SkipIf {
			if skip[p] {
				return true
			}
		}
		return false
	}
	values := map[string]*big.Int{} // a malformed value is reported as such, whatever the rules
	var invalidField string
	var invalid *SigningError
	for field := range checks.Scales {
		raw := leg[field]
		if raw == "" {
			continue
		}
		n, err := ScaleDecimal(raw, decimals(field))
		if err != nil {
			if invalid == nil || field < invalidField {
				invalidField = field
				invalid = &SigningError{Message: fmt.Sprintf("OrderLeg.%s: %v", field, err)}
			}
			continue
		}
		if n.Sign() < 0 || n.Cmp(u64Max) > 0 {
			if invalid == nil || field < invalidField {
				invalidField = field
				invalid = &SigningError{Message: fmt.Sprintf("OrderLeg.%s: %s is out of range", field, raw)}
			}
			continue
		}
		values[field] = n
	}
	// Map traversal order must not choose which malformed field the caller sees.
	// Select the first field by name without sorting or allocating on valid orders.
	if invalid != nil {
		return invalid
	}
	for _, r := range checks.Rules {
		if skipped(r) {
			continue
		}
		v := values[r.Field]
		if v == nil {
			continue
		}
		d, label := decimals(r.Field), r.Field
		if r.Times != "" {
			t := values[r.Times]
			if t == nil || v.Sign() == 0 || t.Sign() == 0 {
				continue
			}
			v = new(big.Int).Mul(v, t)
			d += decimals(r.Times)
			label = r.Field + " * " + r.Times
		}
		lim, err := limit(r.Limit, d)
		if err != nil {
			return err
		}
		if lim.Sign() == 0 {
			continue
		}
		var nearest []*big.Int
		nd := d
		var msg string
		switch r.Check {
		case "at_least":
			if v.Cmp(lim) >= 0 {
				continue
			}
			if r.Times == "" {
				nearest = []*big.Int{lim}
				msg = fmt.Sprintf("%s %s is below %s %s", label, fmtScaled(v, d), r.Limit, fmtScaled(lim, d))
				break
			}
			nd = decimals(r.Times)
			x := ceilDiv(lim, values[r.Field])
			for _, r2 := range checks.Rules {
				if r2.Field != r.Times || r2.Times != "" || skipped(r2) {
					continue
				}
				l2, err := limit(r2.Limit, nd)
				if err != nil {
					return err
				}
				if l2.Sign() == 0 {
					continue
				}
				if r2.Check == "at_least" && x.Cmp(l2) < 0 {
					x = l2
				}
				if r2.Check == "multiple_of" {
					x = new(big.Int).Mul(ceilDiv(x, l2), l2)
				}
			}
			nearest = []*big.Int{x}
			msg = fmt.Sprintf("%s = %s is below %s %s (smallest %s at this %s: %s)",
				label, fmtScaled(v, d), r.Limit, fmtScaled(lim, d), r.Times, r.Field, fmtScaled(x, nd))
		case "multiple_of":
			rem := new(big.Int).Mod(v, lim)
			if rem.Sign() == 0 {
				continue
			}
			lo := new(big.Int).Sub(v, rem)
			if lo.Sign() > 0 {
				nearest = append(nearest, lo)
			}
			nearest = append(nearest, new(big.Int).Add(lo, lim))
			parts := make([]string, len(nearest))
			for i, n := range nearest {
				parts[i] = fmtScaled(n, d)
			}
			msg = fmt.Sprintf("%s %s isn't a multiple of %s %s (nearest: %s)",
				label, fmtScaled(v, d), r.Limit, fmtScaled(lim, d), strings.Join(parts, " or "))
		default:
			continue
		}
		e := &OrderOutOfBoundsError{
			Instrument: name, Leg: index, Field: label, Check: r.Check, Limit: r.Limit,
			Value: fmtScaled(v, d), LimitValue: fmtScaled(lim, d), ServerCode: r.ServerCode,
			Message: name + ": " + msg,
		}
		for _, n := range nearest {
			e.Nearest = append(e.Nearest, fmtScaled(n, nd))
		}
		return e
	}
	return nil
}

func ceilDiv(a, b *big.Int) *big.Int {
	q, r := new(big.Int).QuoRem(a, b, new(big.Int))
	if r.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	return q
}

// fmtScaled renders n / 10^d without trailing zeros ("60000", "60000.1").
func fmtScaled(n *big.Int, d uint32) string {
	s := n.String()
	if d == 0 {
		return s
	}
	if len(s) <= int(d) {
		s = strings.Repeat("0", int(d)-len(s)+1) + s
	}
	whole, frac := s[:len(s)-int(d)], strings.TrimRight(s[len(s)-int(d):], "0")
	if frac == "" {
		return whole
	}
	return whole + "." + frac
}

// legInstruments lists every instrument named by a leg anywhere in an encoded request.
func legInstruments(body []byte, legs, instrument string) []string {
	var v any
	if json.Unmarshal(body, &v) != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			for k, e := range t {
				list, isList := e.([]any)
				if k != legs || !isList {
					walk(e)
					continue
				}
				for _, l := range list {
					m, _ := l.(map[string]any)
					if name, _ := m[instrument].(string); name != "" && !seen[name] {
						seen[name] = true
						out = append(out, name)
					}
				}
			}
		}
	}
	walk(v)
	return out
}
