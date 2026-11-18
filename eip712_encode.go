package grvt

import (
	"encoding/binary"
	"fmt"
	"math/big"
	"strings"

	"golang.org/x/crypto/sha3"
)

// Helpers the generated EIP-712 encoders (signing_gen.go) call. Each generated hash<Type> function
// fills one fixed-size buffer (type hash + one 32-byte word per field) and hashes it once; a dynamic
// array hashes every item, then streams the item hashes through one keccak. The helpers write
// native words straight into the buffer, so hashing a message allocates nothing unless it fails.
// Errors read exactly like the generic encoder's (kept as the test reference in reference_test.go).

// hashFunc is a generated struct (or array item) hasher.
type hashFunc func(Value) ([32]byte, error)

func keccakInto(out *[32]byte, data []byte) {
	h := sha3.NewLegacyKeccak256()
	h.Write(data)
	h.Sum(out[:0])
}

// structFields checks v is a Struct of n fields.
func structFields(v Value, name string, n int) (Struct, error) {
	values, ok := v.(Struct)
	if !ok {
		return nil, &SigningError{Message: name + ": expected a struct value"}
	}
	if len(values) != n {
		return nil, &SigningError{Message: fmt.Sprintf("%s: expected %d fields, got %d", name, n, len(values))}
	}
	return values, nil
}

func fieldErr(name, field string, err error) error {
	return &SigningError{Message: fmt.Sprintf("%s.%s: %s", name, field, strings.TrimPrefix(err.Error(), "signing: "))}
}

func misfit(v Value) error {
	return &SigningError{Message: fmt.Sprintf("value %v (%T) does not fit the field type", v, v)}
}

// putUint writes an unsigned integer of the given bit width (<= 256) as a big-endian word.
func putUint(w []byte, v Value, bits int) error {
	n, ok := v.(*big.Int)
	if !ok || n.Sign() < 0 || n.BitLen() > bits {
		return &SigningError{Message: fmt.Sprintf("value %v out of range for uint%d", v, bits)}
	}
	if n.IsUint64() {
		binary.BigEndian.PutUint64(w[24:32], n.Uint64())
		return nil
	}
	n.FillBytes(w[:32])
	return nil
}

// putInt writes a signed integer of the given bit width (<= 256), two's complement over 256 bits.
func putInt(w []byte, v Value, bits int) error {
	n, ok := v.(*big.Int)
	if !ok || n.BitLen() >= bits && !minInt(n, bits) {
		return &SigningError{Message: fmt.Sprintf("value %v out of range for int%d", v, bits)}
	}
	if n.IsInt64() {
		x := n.Int64()
		if x < 0 {
			for i := 0; i < 24; i++ {
				w[i] = 0xff
			}
		}
		binary.BigEndian.PutUint64(w[24:32], uint64(x))
		return nil
	}
	n.FillBytes(w[:32]) // magnitude; negate in place for negative values
	if n.Sign() < 0 {
		carry := true
		for i := 31; i >= 0; i-- {
			w[i] = ^w[i]
			if carry {
				w[i]++
				carry = w[i] == 0
			}
		}
	}
	return nil
}

// minInt reports whether n is exactly -2^(bits-1), the one in-range value with BitLen == bits.
func minInt(n *big.Int, bits int) bool {
	return n.Sign() < 0 && n.BitLen() == bits && n.TrailingZeroBits() == uint(bits-1)
}

func putBool(w []byte, v Value) error {
	b, ok := v.(bool)
	if !ok {
		return misfit(v)
	}
	if b {
		w[31] = 1
	}
	return nil
}

func putAddress(w []byte, v Value) error {
	a, ok := v.(Address)
	if !ok {
		return misfit(v)
	}
	copy(w[12:32], a[:])
	return nil
}

func putFixedBytes(w []byte, v Value, size int) error {
	b, ok := v.([]byte)
	if !ok || len(b) != size {
		return misfit(v)
	}
	copy(w, b)
	return nil
}

func putString(w []byte, v Value) error {
	s, ok := v.(string)
	if !ok {
		return misfit(v)
	}
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte(s))
	h.Sum(w[:0])
	return nil
}

func putBytes(w []byte, v Value) error {
	b, ok := v.([]byte)
	if !ok {
		return misfit(v)
	}
	h := sha3.NewLegacyKeccak256()
	h.Write(b)
	h.Sum(w[:0])
	return nil
}

func putStruct(w []byte, v Value, item hashFunc) error {
	h, err := item(v)
	if err != nil {
		return err
	}
	copy(w, h[:])
	return nil
}

// putArray writes keccak256(concat(item(x) for x in v)).
func putArray(w []byte, v Value, item hashFunc) error {
	items, ok := v.(List)
	if !ok {
		return misfit(v)
	}
	h := sha3.NewLegacyKeccak256()
	for _, it := range items {
		ih, err := item(it)
		if err != nil {
			return err
		}
		h.Write(ih[:])
	}
	h.Sum(w[:0])
	return nil
}

// Atomic array item hashers: the 32-byte encoding of one element.

func itemUint(bits int) hashFunc {
	return func(v Value) (w [32]byte, err error) { return w, putUint(w[:], v, bits) }
}

func itemInt(bits int) hashFunc {
	return func(v Value) (w [32]byte, err error) { return w, putInt(w[:], v, bits) }
}

func itemBool(v Value) (w [32]byte, err error)    { return w, putBool(w[:], v) }
func itemAddress(v Value) (w [32]byte, err error) { return w, putAddress(w[:], v) }
func itemString(v Value) (w [32]byte, err error)  { return w, putString(w[:], v) }
func itemBytes(v Value) (w [32]byte, err error)   { return w, putBytes(w[:], v) }

func itemFixedBytes(size int) hashFunc {
	return func(v Value) (w [32]byte, err error) { return w, putFixedBytes(w[:], v, size) }
}

// typedDigest is keccak256(0x19 0x01 || domainSeparator || structHash).
func typedDigest(sep, structHash *[32]byte) [32]byte {
	var buf [66]byte
	buf[0], buf[1] = 0x19, 0x01
	copy(buf[2:34], sep[:])
	copy(buf[34:66], structHash[:])
	var out [32]byte
	keccakInto(&out, buf[:])
	return out
}
