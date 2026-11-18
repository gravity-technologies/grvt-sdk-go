package grvt

// The generic, runtime-interpreted EIP-712 encoder the SDK shipped before the generated encoders
// (eip712_encode.go + signing_gen.go). Kept only as the test reference they are compared against.

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

type encKind int

const (
	encUint encKind = iota
	encInt
	encBool
	encAddress
	encFixedBytes
	encString
	encBytes
	encStruct
	encArray
)

type encoder struct {
	kind  encKind
	bits  int // uint/int bits, fixed bytes length
	index int // struct index
	inner *encoder
}

type compiled struct {
	name     string
	typeHash [32]byte
	fields   []FieldDef
	encs     []*encoder
}

// refType is the reference encoder compiled for one StructType.
type refType struct {
	structs []compiled
	primary int
}

func newRefType(st *StructType) (*refType, error) {
	types := st.types
	index := func(n string) int {
		for i, t := range types {
			if t.Name == n {
				return i
			}
		}
		return -1
	}
	r := &refType{primary: index(st.name)}
	for _, t := range types {
		c := compiled{name: t.Name, fields: t.Fields, typeHash: keccak256([]byte(encodeType(t.Name, types)))}
		for _, f := range t.Fields {
			e, err := newEncoder(f.Type, index)
			if err != nil {
				return nil, err
			}
			c.encs = append(c.encs, e)
		}
		r.structs = append(r.structs, c)
	}
	return r, nil
}

func (st *refType) HashStruct(message Value) ([32]byte, error) { return st.hash(st.primary, message) }

func (st *refType) Digest(d *Domain, message Value) ([32]byte, error) {
	h, err := st.HashStruct(message)
	if err != nil {
		return h, err
	}
	return keccak256([]byte{0x19, 0x01}, d.separator[:], h[:]), nil
}

func (st *refType) hash(idx int, v Value) ([32]byte, error) {
	s := &st.structs[idx]
	values, ok := v.(Struct)
	if !ok {
		return [32]byte{}, &SigningError{Message: s.name + ": expected a struct value"}
	}
	if len(values) != len(s.encs) {
		return [32]byte{}, &SigningError{Message: fmt.Sprintf("%s: expected %d fields, got %d", s.name, len(s.encs), len(values))}
	}
	buf := make([]byte, 0, 32*(1+len(values)))
	buf = append(buf, s.typeHash[:]...)
	for i, e := range s.encs {
		w, err := st.encode(e, values[i])
		if err != nil {
			return [32]byte{}, &SigningError{Message: fmt.Sprintf("%s.%s: %s", s.name, s.fields[i].Name, strings.TrimPrefix(err.Error(), "signing: "))}
		}
		buf = append(buf, w...)
	}
	return keccak256(buf), nil
}

var two256 = new(big.Int).Lsh(big.NewInt(1), 256)

func (st *refType) encode(e *encoder, v Value) ([]byte, error) {
	switch e.kind {
	case encUint:
		n, ok := v.(*big.Int)
		if !ok || n.Sign() < 0 || n.BitLen() > e.bits {
			return nil, &SigningError{Message: fmt.Sprintf("value %v out of range for uint%d", v, e.bits)}
		}
		return uintWord(n), nil
	case encInt:
		n, ok := v.(*big.Int)
		lim := new(big.Int).Lsh(big.NewInt(1), uint(e.bits-1))
		if !ok || n.Cmp(lim) >= 0 || n.Cmp(new(big.Int).Neg(lim)) < 0 {
			return nil, &SigningError{Message: fmt.Sprintf("value %v out of range for int%d", v, e.bits)}
		}
		if n.Sign() < 0 {
			return uintWord(new(big.Int).Add(n, two256)), nil
		}
		return uintWord(n), nil
	case encBool:
		b, ok := v.(bool)
		if !ok {
			break
		}
		w := make([]byte, 32)
		if b {
			w[31] = 1
		}
		return w, nil
	case encAddress:
		a, ok := v.(Address)
		if !ok {
			break
		}
		return append(make([]byte, 12), a[:]...), nil
	case encFixedBytes:
		b, ok := v.([]byte)
		if !ok || len(b) != e.bits {
			break
		}
		w := make([]byte, 32)
		copy(w, b)
		return w, nil
	case encString:
		s, ok := v.(string)
		if !ok {
			break
		}
		h := keccak256([]byte(s))
		return h[:], nil
	case encBytes:
		b, ok := v.([]byte)
		if !ok {
			break
		}
		h := keccak256(b)
		return h[:], nil
	case encStruct:
		h, err := st.hash(e.index, v)
		return h[:], err
	case encArray:
		items, ok := v.(List)
		if !ok {
			break
		}
		buf := make([]byte, 0, 32*len(items))
		for _, it := range items {
			w, err := st.encode(e.inner, it)
			if err != nil {
				return nil, err
			}
			buf = append(buf, w...)
		}
		h := keccak256(buf)
		return h[:], nil
	}
	return nil, &SigningError{Message: fmt.Sprintf("value %v (%T) does not fit the field type", v, v)}
}

func newEncoder(t string, index func(string) int) (*encoder, error) {
	if strings.HasSuffix(t, "]") {
		open := strings.LastIndexByte(t, '[')
		if open < 0 {
			return nil, &SigningError{Message: "bad type " + t}
		}
		inner, err := newEncoder(t[:open], index)
		if err != nil {
			return nil, err
		}
		return &encoder{kind: encArray, inner: inner}, nil
	}
	if i := index(t); i >= 0 {
		return &encoder{kind: encStruct, index: i}, nil
	}
	bits := func(s string) (int, error) {
		if s == "" {
			return 256, nil
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return 0, &SigningError{Message: "bad type " + t}
		}
		return n, nil
	}
	switch {
	case t == "bool":
		return &encoder{kind: encBool}, nil
	case t == "address":
		return &encoder{kind: encAddress}, nil
	case t == "string":
		return &encoder{kind: encString}, nil
	case t == "bytes":
		return &encoder{kind: encBytes}, nil
	case strings.HasPrefix(t, "uint"):
		n, err := bits(t[4:])
		return &encoder{kind: encUint, bits: n}, err
	case strings.HasPrefix(t, "int"):
		n, err := bits(t[3:])
		return &encoder{kind: encInt, bits: n}, err
	case strings.HasPrefix(t, "bytes"):
		n, err := bits(t[5:])
		return &encoder{kind: encFixedBytes, bits: n}, err
	}
	return nil, &SigningError{Message: "unsupported EIP-712 type " + t}
}
