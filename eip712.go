package grvt

import (
	"encoding/hex"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/sha3"
)

// EIP-712 types, domains and typed-data JSON.
//
// The generator emits one StructType per signed message: the type definitions in eth_signTypedData
// order, the expected type hash (checked when the package loads, so a template bug fails early
// instead of producing signatures the server rejects) and a generated encoder (eip712_encode.go).
//
// Messages are positional: Struct{...} in the order of the type's fields.

// Value is an EIP-712 value: *big.Int, bool, Address, string, []byte, List or Struct. Integers are
// range-checked against the declared type when hashed.
type Value = any

// Address is a 20-byte EVM address.
type Address [20]byte

// List is an EIP-712 array value.
type List []Value

// Struct is an EIP-712 struct value: field values in the type's field order.
type Struct []Value

// FieldDef is one EIP-712 field: name and type.
type FieldDef struct{ Name, Type string }

// TypeDef is one EIP-712 struct definition.
type TypeDef struct {
	Name   string
	Fields []FieldDef
}

func keccak256(data ...[]byte) [32]byte {
	h := sha3.NewLegacyKeccak256()
	for _, d := range data {
		h.Write(d)
	}
	var out [32]byte
	h.Sum(out[:0])
	return out
}

// Domain is an EIP-712 domain with its separator computed once.
type Domain struct {
	Name              string
	Version           string
	ChainID           uint64
	VerifyingContract *Address
	separator         [32]byte
}

// NewDomain builds a domain. verifyingContract may be nil (Grvt's domain has none).
func NewDomain(name, version string, chainID uint64, verifyingContract *Address) *Domain {
	typ := "EIP712Domain(string name,string version,uint256 chainId"
	if verifyingContract != nil {
		typ += ",address verifyingContract"
	}
	typ += ")"
	th, nh, vh := keccak256([]byte(typ)), keccak256([]byte(name)), keccak256([]byte(version))
	buf := append(append(append([]byte{}, th[:]...), nh[:]...), vh[:]...)
	buf = append(buf, uintWord(new(big.Int).SetUint64(chainID))...)
	if verifyingContract != nil {
		buf = append(buf, make([]byte, 12)...)
		buf = append(buf, verifyingContract[:]...)
	}
	return &Domain{Name: name, Version: version, ChainID: chainID, VerifyingContract: verifyingContract, separator: keccak256(buf)}
}

// ExchangeDomain is the "GRVT Exchange" domain for a chain id.
// Domains are cached per chain id.
func ExchangeDomain(chainID uint64) *Domain {
	exchangeDomains.Lock()
	defer exchangeDomains.Unlock()
	d := exchangeDomains.m[chainID]
	if d == nil {
		if exchangeDomains.m == nil {
			exchangeDomains.m = map[uint64]*Domain{}
		}
		d = NewDomain("GRVT Exchange", "0", chainID, nil)
		exchangeDomains.m[chainID] = d
	}
	return d
}

var exchangeDomains struct {
	sync.Mutex
	m map[uint64]*Domain
}

// Separator is the domain separator.
func (d *Domain) Separator() [32]byte { return d.separator }

func (d *Domain) json() map[string]any {
	m := map[string]any{"name": d.Name, "version": d.Version, "chainId": strconv.FormatUint(d.ChainID, 10)}
	if d.VerifyingContract != nil {
		m["verifyingContract"] = ChecksumAddress(*d.VerifyingContract)
	}
	return m
}

func (d *Domain) typesJSON() []any {
	t := []any{
		map[string]any{"name": "name", "type": "string"},
		map[string]any{"name": "version", "type": "string"},
		map[string]any{"name": "chainId", "type": "uint256"},
	}
	if d.VerifyingContract != nil {
		t = append(t, map[string]any{"name": "verifyingContract", "type": "address"})
	}
	return t
}

// StructType is one of Grvt's signed EIP-712 types (signing_gen.go), with its generated encoder.
// Only the SDK builds them: signers receive one and can read its name, definitions and typed data.
type StructType struct {
	name     string
	types    []TypeDef
	typeHash [32]byte
	hash     hashFunc
}

// newStructType checks the definitions and the primary type's hash against expectedTypeHash (0x hex)
// and attaches the generated encoder.
func newStructType(name string, types []TypeDef, expectedTypeHash string, enc hashFunc) (*StructType, error) {
	byName := map[string]bool{}
	for _, t := range types {
		byName[t.Name] = true
	}
	if !byName[name] {
		return nil, &SigningError{Message: name + ": primary type not defined"}
	}
	for _, t := range types {
		for _, f := range t.Fields {
			if !validType(f.Type, byName) {
				return nil, &SigningError{Message: "unsupported EIP-712 type " + f.Type}
			}
		}
	}
	st := &StructType{name: name, types: types, typeHash: keccak256([]byte(encodeType(name, types))), hash: enc}
	got := Hex0x(st.typeHash[:])
	if !strings.EqualFold(got, expectedTypeHash) {
		return nil, &SigningError{Message: fmt.Sprintf("%s: type hash mismatch, generated %s vs computed %s", name, expectedTypeHash, got)}
	}
	return st, nil
}

// mustStructType is newStructType for generated code: a mismatch is a generator bug, so it panics
// when the package loads.
func mustStructType(name string, types []TypeDef, expectedTypeHash string, enc hashFunc) *StructType {
	st, err := newStructType(name, types, expectedTypeHash, enc)
	if err != nil {
		panic(err)
	}
	return st
}

// Name is the primary type name, e.g. "Order".
func (st *StructType) Name() string { return st.name }

// Types are the definitions of the primary type and every struct it references.
func (st *StructType) Types() []TypeDef { return st.types }

// TypeHash is keccak256(encodeType) of the primary type.
func (st *StructType) TypeHash() [32]byte { return st.typeHash }

// EncodeType is the primary type's encodeType string.
func (st *StructType) EncodeType() string { return encodeType(st.name, st.types) }

// HashStruct is hashStruct(message) for the primary type. message must be a Struct.
func (st *StructType) HashStruct(message Value) ([32]byte, error) { return st.hash(message) }

// Digest is keccak256(0x1901 || domainSeparator || hashStruct(message)): what gets signed.
func (st *StructType) Digest(d *Domain, message Value) ([32]byte, error) {
	h, err := st.HashStruct(message)
	if err != nil {
		return h, err
	}
	return typedDigest(&d.separator, &h), nil
}

func uintWord(n *big.Int) []byte {
	w := make([]byte, 32)
	n.FillBytes(w)
	return w
}

// validType: an atomic type, a defined struct, or an array of either.
func validType(t string, structs map[string]bool) bool {
	if strings.HasSuffix(t, "]") {
		open := strings.LastIndexByte(t, '[')
		return open >= 0 && validType(t[:open], structs)
	}
	if structs[t] {
		return true
	}
	bits := func(s string, limit int, step int) bool {
		if s == "" {
			return step == 8
		}
		n, err := strconv.Atoi(s)
		return err == nil && n > 0 && n <= limit && n%step == 0
	}
	switch {
	case t == "bool", t == "address", t == "string", t == "bytes":
		return true
	case strings.HasPrefix(t, "uint"):
		return bits(t[4:], 256, 8)
	case strings.HasPrefix(t, "int"):
		return bits(t[3:], 256, 8)
	case strings.HasPrefix(t, "bytes"):
		return bits(t[5:], 32, 1) && t[5:] != ""
	}
	return false
}

// TypedData is the standard eth_signTypedData_v4 JSON, for signers that need the full struct (KMS,
// wallets).
func (st *StructType) TypedData(d *Domain, message Value) (map[string]any, error) {
	types := map[string]any{"EIP712Domain": d.typesJSON()}
	for _, t := range st.types {
		fs := make([]any, len(t.Fields))
		for i, f := range t.Fields {
			fs[i] = map[string]any{"name": f.Name, "type": f.Type}
		}
		types[t.Name] = fs
	}
	msg, err := st.json(st.name, message)
	if err != nil {
		return nil, err
	}
	return map[string]any{"types": types, "primaryType": st.name, "domain": d.json(), "message": msg}, nil
}

func (st *StructType) def(name string) (TypeDef, bool) {
	for _, t := range st.types {
		if t.Name == name {
			return t, true
		}
	}
	return TypeDef{}, false
}

func (st *StructType) json(name string, v Value) (map[string]any, error) {
	t, _ := st.def(name)
	values, ok := v.(Struct)
	if !ok || len(values) != len(t.Fields) {
		return nil, &SigningError{Message: name + ": expected a struct value"}
	}
	m := map[string]any{}
	for i, f := range t.Fields {
		j, err := st.jsonValue(f.Type, values[i])
		if err != nil {
			return nil, err
		}
		m[f.Name] = j
	}
	return m, nil
}

func (st *StructType) jsonValue(typ string, v Value) (any, error) {
	if strings.HasSuffix(typ, "]") {
		inner := typ[:strings.LastIndexByte(typ, '[')]
		items, _ := v.(List)
		out := make([]any, len(items))
		for i, it := range items {
			j, err := st.jsonValue(inner, it)
			if err != nil {
				return nil, err
			}
			out[i] = j
		}
		return out, nil
	}
	if _, ok := st.def(typ); ok {
		return st.json(typ, v)
	}
	switch x := v.(type) {
	case *big.Int:
		return x.String(), nil
	case bool, string:
		return x, nil
	case Address:
		return ChecksumAddress(x), nil
	case []byte:
		return Hex0x(x), nil
	}
	return nil, &SigningError{Message: fmt.Sprintf("cannot render %T", v)}
}

// encodeType: the primary type, then referenced struct types sorted by name.
func encodeType(primary string, types []TypeDef) string {
	byName := map[string]TypeDef{}
	for _, t := range types {
		byName[t.Name] = t
	}
	seen := map[string]bool{}
	var deps func(string)
	deps = func(n string) {
		if seen[n] {
			return
		}
		seen[n] = true
		for _, f := range byName[n].Fields {
			b, _, _ := strings.Cut(f.Type, "[")
			if _, ok := byName[b]; ok {
				deps(b)
			}
		}
	}
	deps(primary)
	delete(seen, primary)
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range append([]string{primary}, names...) {
		parts := make([]string, len(byName[n].Fields))
		for i, f := range byName[n].Fields {
			parts[i] = f.Type + " " + f.Name
		}
		b.WriteString(n + "(" + strings.Join(parts, ",") + ")")
	}
	return b.String()
}

// Hex0x is lowercase 0x-prefixed hex.
func Hex0x(b []byte) string {
	if len(b) <= 32 { // one allocation for hashes and signature words
		var buf [66]byte
		buf[0], buf[1] = '0', 'x'
		n := hex.Encode(buf[2:], b)
		return string(buf[:2+n])
	}
	return "0x" + hex.EncodeToString(b)
}

// ChecksumAddress is the EIP-55 mixed-case address, as eth_signTypedData tooling prints it.
func ChecksumAddress(a Address) string {
	lower := hex.EncodeToString(a[:])
	h := keccak256([]byte(lower))
	out := []byte(lower)
	for i, c := range out {
		nibble := h[i/2] >> 4
		if i%2 == 1 {
			nibble = h[i/2] & 0x0f
		}
		if c >= 'a' && nibble >= 8 {
			out[i] = c - 32
		}
	}
	return "0x" + string(out)
}

func parseHex(s string) ([]byte, bool) {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	b, err := hex.DecodeString(s)
	return b, err == nil
}
