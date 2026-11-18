package grvt

import (
	"context"
	"fmt"
	"math/big"
	"reflect"
	"strings"
	"sync"
	"time"
)

// Helpers the generated sign functions call: one MessageBuilder method per contract transform (see
// sdk/contract/contract.json signed.transforms). The first error is kept in MessageBuilder.Err and
// the sign function returns it before anything is signed.

// MessageBuilder collects the first transform error while a message is built.
type MessageBuilder struct{ Err error }

func (b *MessageBuilder) fail(format string, a ...any) Value {
	if b.Err == nil {
		b.Err = &SigningError{Message: fmt.Sprintf(format, a...)}
	}
	return nil
}

// scalar reads a model field: a string, a named string (enum) or a pointer to a number or bool.
// Absent (nil pointer, "") returns ok=false.
func scalar(v any) (any, bool) {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return nil, false
	}
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, false
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.String:
		if rv.String() == "" {
			return nil, false
		}
		return rv.String(), true
	case reflect.Bool:
		return rv.Bool(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return big.NewInt(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return new(big.Int).SetUint64(rv.Uint()), true
	}
	return nil, false
}

// Copy is the copy, sig.nonce and sig.expiration transforms: the wire value as the EIP-712 type, or
// def when absent (def nil: the field is required). def is a bool, string or int64.
func (b *MessageBuilder) Copy(v any, eipType, at string, def any) Value {
	if value, ok := v.(time.Time); ok {
		v = &value
	}
	if timestamp, ok := v.(*time.Time); ok && timestamp != nil {
		ns := timestamp.UnixNano()
		if !time.Unix(0, ns).Equal(*timestamp) {
			return b.fail("%s: timestamp is outside the signed 64-bit nanosecond range", at)
		}
		v = &ns
	}
	x, ok := scalar(v)
	if !ok {
		if def == nil {
			return b.fail("%s is required for signing", at)
		}
		x = def
		if n, isInt := def.(int64); isInt {
			x = big.NewInt(n)
		}
	}
	switch {
	case eipType == "bool":
		switch t := x.(type) {
		case bool:
			return t
		case string:
			return strings.EqualFold(t, "true")
		case *big.Int:
			return t.Sign() != 0
		}
	case eipType == "address":
		if s, ok := x.(string); ok {
			a, err := toAddress(s)
			if err != nil {
				return b.fail("%s: %v", at, err)
			}
			return a
		}
	case eipType == "string":
		if s, ok := x.(string); ok {
			return s
		}
	case strings.HasPrefix(eipType, "uint") || strings.HasPrefix(eipType, "int"):
		switch t := x.(type) {
		case *big.Int:
			return t
		case string:
			n, err := toInt(t)
			if err != nil {
				return b.fail("%s: %v", at, err)
			}
			return n
		case bool:
			if t {
				return big.NewInt(1)
			}
			return big.NewInt(0)
		}
	default:
		return b.fail("%s: unsupported copy type %s", at, eipType)
	}
	return b.fail("%s: %v does not fit %s", at, x, eipType)
}

// Address is the address transform: absent is the zero address.
func (b *MessageBuilder) Address(v, at string) Value {
	if v == "" {
		return Address{}
	}
	a, err := toAddress(v)
	if err != nil {
		return b.fail("%s: %v", at, err)
	}
	return a
}

// AddressSet is the builder variant rule: is this address set to something other than zero?
func AddressSet(v string) bool {
	raw, ok := parseHex(v)
	if v == "" || !ok {
		return false
	}
	for _, x := range raw {
		if x != 0 {
			return true
		}
	}
	return false
}

// WireEnum is a generated enum: its wire name and the integer signed for it.
type WireEnum interface {
	String() string
	Code() (int64, bool)
}

// EnumCode is the enumCode transform. Absent is 0; a value this SDK does not know is an error.
func (b *MessageBuilder) EnumCode(v WireEnum, at string) Value {
	if v.String() == "" {
		return big.NewInt(0)
	}
	c, ok := v.Code()
	if !ok {
		return b.fail("%s: unknown enum value %q", at, v.String())
	}
	return big.NewInt(c)
}

// Scaled is the scaled transform: decimal string x 10^decimals. Over-precision is an error, never
// rounded. def ("" = none) is used when v is absent.
func (b *MessageBuilder) Scaled(v string, decimals uint32, at, def string) Value {
	if v == "" {
		if def == "" {
			return b.fail("%s is required for signing", at)
		}
		v = def
	}
	n, err := ScaleDecimal(v, decimals)
	if err != nil {
		return b.fail("%s: %v", at, err)
	}
	return n
}

// ScaleDecimal turns "65000.1" with 9 decimals into 65000100000000. More decimal places than
// decimals is an error.
func ScaleDecimal(s string, decimals uint32) (*big.Int, error) {
	t := strings.TrimSpace(s)
	neg := strings.HasPrefix(t, "-")
	t = strings.TrimPrefix(strings.TrimPrefix(t, "-"), "+")
	whole, frac, _ := strings.Cut(t, ".")
	if whole == "" && frac == "" || !digitsOnly(whole) || !digitsOnly(frac) {
		return nil, fmt.Errorf("not a decimal: %q", s)
	}
	frac = strings.TrimRight(frac, "0")
	if len(frac) > int(decimals) {
		return nil, fmt.Errorf("%s has more than %d decimal places", s, decimals)
	}
	if len(whole)+int(decimals) <= 19 { // fits a uint64: no text to build
		var u uint64
		for _, digits := range [2]string{whole, frac} {
			for i := 0; i < len(digits); i++ {
				u = u*10 + uint64(digits[i]-'0')
			}
		}
		for i := len(frac); i < int(decimals); i++ {
			u *= 10
		}
		n := new(big.Int).SetUint64(u)
		if neg {
			n.Neg(n)
		}
		return n, nil
	}
	n, _ := new(big.Int).SetString("0"+whole+frac+strings.Repeat("0", int(decimals)-len(frac)), 10)
	if neg {
		n.Neg(n)
	}
	return n, nil
}

func digitsOnly(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// InstrumentBaseDecimals is the scale for instrument_base_decimals.
func (b *MessageBuilder) InstrumentBaseDecimals(meta *Meta, name string) uint32 {
	m, err := meta.Instrument(name)
	if err != nil {
		b.fail("%v", strings.TrimPrefix(err.Error(), "signing: "))
		return 0
	}
	return m.BaseDecimals
}

// InstrumentQuoteDecimals is the scale for instrument_quote_decimals.
func (b *MessageBuilder) InstrumentQuoteDecimals(meta *Meta, name string) uint32 {
	m, err := meta.Instrument(name)
	if err != nil {
		b.fail("%v", strings.TrimPrefix(err.Error(), "signing: "))
		return 0
	}
	return m.QuoteDecimals
}

// MainAccount is the mainAccount transform: the session's main account address.
func (b *MessageBuilder) MainAccount(meta *Meta) Value {
	if meta.MainAccount == "" {
		return b.fail("the session's main account is not loaded")
	}
	a, err := toAddress(meta.MainAccount)
	if err != nil {
		return b.fail("main account: %v", err)
	}
	return a
}

// CurrencyBalanceDecimals is the scale for currency_balance_decimals.
func (b *MessageBuilder) CurrencyBalanceDecimals(meta *Meta, symbol string) uint32 {
	c, err := meta.Currency(symbol)
	if err != nil {
		b.fail("%v", strings.TrimPrefix(err.Error(), "signing: "))
		return 0
	}
	return c.BalanceDecimals
}

// AssetKey is the assetKey transform: the instrument hash as a uint256.
func (b *MessageBuilder) AssetKey(meta *Meta, name string) Value {
	m, err := meta.Instrument(name)
	if err != nil {
		return b.fail("%v", strings.TrimPrefix(err.Error(), "signing: "))
	}
	n, err := toInt(m.InstrumentHash)
	if err != nil {
		return b.fail("instrument_hash of %q: %v", name, err)
	}
	return n
}

// CurrencyID is the currencyId transform.
func (b *MessageBuilder) CurrencyID(meta *Meta, symbol string) Value {
	c, err := meta.Currency(symbol)
	if err != nil {
		return b.fail("%v", strings.TrimPrefix(err.Error(), "signing: "))
	}
	return new(big.Int).SetUint64(uint64(c.ID))
}

// Each is the each transform: one nested message per list item.
func Each[T any](items []T, f func(*T) Value) List {
	out := make(List, len(items))
	for i := range items {
		out[i] = f(&items[i])
	}
	return out
}

func toInt(s string) (*big.Int, error) {
	t := strings.TrimSpace(s)
	if strings.HasPrefix(t, "0x") || strings.HasPrefix(t, "0X") {
		n, ok := new(big.Int).SetString(t[2:], 16)
		if !ok || n.BitLen() > 256 {
			return nil, fmt.Errorf("not a hex integer: %q", s)
		}
		return n, nil
	}
	n, ok := new(big.Int).SetString(t, 10)
	if !ok {
		return nil, fmt.Errorf("not an integer: %q", s)
	}
	return n, nil
}

func toAddress(s string) (Address, error) {
	raw, ok := parseHex(s)
	if !ok || len(raw) != 20 {
		return Address{}, fmt.Errorf("not an address: %q", s)
	}
	var a Address
	copy(a[:], raw)
	return a, nil
}

// -- metadata ----------------------------------------------------------------------------------

// InstrumentMeta is what signing needs from an instrument (get_instrument).
type InstrumentMeta struct {
	InstrumentHash string
	BaseDecimals   uint32
	QuoteDecimals  uint32
	// Limits holds the instrument limits the order checks read (tick_size, min_size, min_notional), as
	// decimal strings.
	Limits map[string]string
}

// CurrencyMeta is what signing needs from a currency (get_currency).
type CurrencyMeta struct {
	ID              uint32
	Symbol          string
	BalanceDecimals uint32
}

// Meta is the metadata one sign call needs, already loaded: instruments, currencies and, for calls that
// sign it, the session's main account.
type Meta struct {
	Instruments map[string]InstrumentMeta
	Currencies  map[string]CurrencyMeta
	MainAccount string
}

func (m *Meta) Instrument(name string) (InstrumentMeta, error) {
	v, ok := m.Instruments[name]
	if !ok {
		return v, &SigningError{Message: fmt.Sprintf("no metadata for instrument %q", name)}
	}
	return v, nil
}

func (m *Meta) Currency(symbol string) (CurrencyMeta, error) {
	v, ok := m.Currencies[symbol]
	if !ok {
		return v, &SigningError{Message: fmt.Sprintf("no metadata for currency %q", symbol)}
	}
	return v, nil
}

type metaEntry[T any] struct {
	mu       sync.Mutex
	v        T
	ok       bool
	inflight *metaLoad[T]
}

// MetadataCache holds instrument and currency metadata and the session's main account for signing. Each
// key loads once; concurrent callers share one load; a failed load is not cached. The main account is
// loaded once per client.
type MetadataCache struct {
	mu          sync.Mutex
	instruments map[string]*metaEntry[InstrumentMeta]
	currencies  map[string]*metaEntry[CurrencyMeta]
	mainAccount metaEntry[string]
	owner       context.Context //nolint:containedctx // client lifetime for shared metadata reads
	loadTimeout time.Duration
}

func entry[T any](mu *sync.Mutex, m *map[string]*metaEntry[T], key string) *metaEntry[T] {
	mu.Lock()
	defer mu.Unlock()
	if *m == nil {
		*m = map[string]*metaEntry[T]{}
	}
	e := (*m)[key]
	if e == nil {
		e = &metaEntry[T]{}
		(*m)[key] = e
	}
	return e
}

// The cache owns metadata loads. A caller only cancels its own wait.
type metaLoad[T any] struct {
	done  chan struct{}
	value T
	err   error
}

func load[T any](ctx context.Context, owner context.Context, timeout time.Duration, e *metaEntry[T], key string, fn func(context.Context, string) (T, error)) (T, error) {
	e.mu.Lock()
	if e.ok {
		v := e.v
		e.mu.Unlock()
		return v, nil
	}
	task := e.inflight
	if task == nil {
		task = &metaLoad[T]{done: make(chan struct{})}
		e.inflight = task
		if owner == nil {
			owner = context.WithoutCancel(ctx)
		}
		go func() {
			if timeout <= 0 {
				timeout = 30 * time.Second
			}
			loadCtx, cancel := context.WithTimeout(owner, timeout)
			defer cancel()
			task.value, task.err = fn(loadCtx, key)
			e.mu.Lock()
			if task.err == nil {
				e.v, e.ok = task.value, true
			}
			e.inflight = nil
			close(task.done)
			e.mu.Unlock()
		}()
	}
	e.mu.Unlock()
	select {
	case <-ctx.Done():
		var zero T
		return zero, context.Cause(ctx)
	case <-task.done:
		return task.value, task.err
	}
}

func put[T any](e *metaEntry[T], v T) {
	if e.mu.TryLock() { // a load in progress for this key sets it itself
		if e.inflight == nil {
			e.v, e.ok = v, true
		}
		e.mu.Unlock()
	}
}

func (c *MetadataCache) Instrument(ctx context.Context, name string, fn func(context.Context, string) (InstrumentMeta, error)) (InstrumentMeta, error) {
	return load(ctx, c.owner, c.loadTimeout, entry(&c.mu, &c.instruments, name), name, fn)
}

func (c *MetadataCache) Currency(ctx context.Context, symbol string, fn func(context.Context, string) (CurrencyMeta, error)) (CurrencyMeta, error) {
	return load(ctx, c.owner, c.loadTimeout, entry(&c.mu, &c.currencies, symbol), symbol, fn)
}

func (c *MetadataCache) PutInstrument(name string, v InstrumentMeta) {
	put(entry(&c.mu, &c.instruments, name), v)
}

// DropInstrument forgets an instrument; the next call that needs it loads it again.
func (c *MetadataCache) DropInstrument(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.instruments, name)
}

func (c *MetadataCache) PutCurrency(symbol string, v CurrencyMeta) {
	put(entry(&c.mu, &c.currencies, symbol), v)
}

// MainAccount returns the session's main account address, loading it with fn the first time.
func (c *MetadataCache) MainAccount(ctx context.Context, fn func(context.Context) (string, error)) (string, error) {
	return load(ctx, c.owner, c.loadTimeout, &c.mainAccount, "", func(ctx context.Context, _ string) (string, error) { return fn(ctx) })
}

// PutMainAccount sets the session's main account address, so signing does not fetch it.
func (c *MetadataCache) PutMainAccount(address string) {
	put(&c.mainAccount, address)
}
