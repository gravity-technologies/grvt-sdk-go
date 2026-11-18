package grvt

import (
	"context"
	"errors"
	"math/big"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

// Generated encoders vs the reference encoder (reference_test.go): random valid messages must give
// the same digest; out-of-range and mistyped values must give the same error.

var generatedTypes = signedTypes

func bitsOf(typ, prefix string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(typ, prefix))
	if err != nil {
		return 256
	}
	return n
}

type gen struct {
	r  *rand.Rand
	st *StructType
}

// randUint: edge values (0, 1, max) half the time, else random bits.
func (g gen) randUint(bits int) *big.Int {
	top := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), uint(bits)), big.NewInt(1))
	switch g.r.Intn(6) {
	case 0:
		return big.NewInt(0)
	case 1:
		return big.NewInt(1)
	case 2:
		return top
	}
	return new(big.Int).Rand(g.r, new(big.Int).Add(top, big.NewInt(1)))
}

func (g gen) randInt(bits int) *big.Int {
	lim := new(big.Int).Lsh(big.NewInt(1), uint(bits-1))
	switch g.r.Intn(6) {
	case 0:
		return new(big.Int).Neg(lim) // min
	case 1:
		return new(big.Int).Sub(lim, big.NewInt(1)) // max
	case 2:
		return big.NewInt(-1)
	}
	n := new(big.Int).Rand(g.r, new(big.Int).Lsh(lim, 1))
	return n.Sub(n, lim)
}

func (g gen) value(typ string) Value {
	if strings.HasSuffix(typ, "[]") {
		n := g.r.Intn(5) // includes empty arrays
		l := make(List, n)
		for i := range l {
			l[i] = g.value(typ[:len(typ)-2])
		}
		return l
	}
	if def, ok := g.st.def(typ); ok {
		s := make(Struct, len(def.Fields))
		for i, f := range def.Fields {
			s[i] = g.value(f.Type)
		}
		return s
	}
	switch {
	case typ == "bool":
		return g.r.Intn(2) == 1
	case typ == "address":
		var a Address
		g.r.Read(a[:])
		return a
	case strings.HasPrefix(typ, "uint"):
		return g.randUint(bitsOf(typ, "uint"))
	case strings.HasPrefix(typ, "int"):
		return g.randInt(bitsOf(typ, "int"))
	}
	panic("no generator for " + typ)
}

// bad returns an invalid value for typ: out of range, or the wrong Go type.
func (g gen) bad(typ string) Value {
	switch {
	case strings.HasSuffix(typ, "[]"):
		return Struct{}
	case strings.HasPrefix(typ, "uint"):
		bits := bitsOf(typ, "uint")
		if g.r.Intn(2) == 0 {
			return big.NewInt(-1)
		}
		return new(big.Int).Lsh(big.NewInt(1), uint(bits))
	case strings.HasPrefix(typ, "int"):
		lim := new(big.Int).Lsh(big.NewInt(1), uint(bitsOf(typ, "int")-1))
		if g.r.Intn(2) == 0 {
			return lim
		}
		return new(big.Int).Sub(new(big.Int).Neg(lim), big.NewInt(1))
	}
	if g.r.Intn(2) == 0 {
		return "nope"
	}
	return nil
}

func TestGeneratedEncodersMatchReference(t *testing.T) {
	r := rand.New(rand.NewSource(1)) //nolint:gosec // reproducible test data
	for _, d := range []*Domain{ExchangeDomain(325), ExchangeDomain(1), ExchangeDomain(^uint64(0))} {
		for _, st := range generatedTypes {
			ref, err := newRefType(st)
			if err != nil {
				t.Fatal(err)
			}
			if st.TypeHash() != ref.structs[ref.primary].typeHash {
				t.Fatalf("%s: type hash differs from the reference", st.name)
			}
			g := gen{r, st}
			for i := 0; i < 2000; i++ {
				msg := g.value(st.name)
				want, err := ref.Digest(d, msg)
				if err != nil {
					t.Fatalf("%s: reference rejects a valid message: %v", st.name, err)
				}
				got, err := st.Digest(d, msg)
				if err != nil || got != want {
					t.Fatalf("%s: digest %x (%v), reference %x\nmessage %v", st.name, got, err, want, msg)
				}
			}
		}
	}
}

// Every invalid field (top level and inside a nested struct) must fail the same way.
func TestGeneratedEncodersRejectLikeReference(t *testing.T) {
	r := rand.New(rand.NewSource(2)) //nolint:gosec // reproducible test data
	for _, st := range generatedTypes {
		ref, _ := newRefType(st)
		g := gen{r, st}
		def, _ := st.def(st.name)
		check := func(msg Value) {
			_, want := ref.HashStruct(msg)
			_, got := st.HashStruct(msg)
			if want == nil || got == nil || got.Error() != want.Error() {
				t.Fatalf("%s: error %v, reference %v", st.name, got, want)
			}
		}
		for i, f := range def.Fields {
			for k := 0; k < 20; k++ {
				msg := g.value(st.name).(Struct)
				msg[i] = g.bad(f.Type)
				check(msg)
			}
			if !strings.HasSuffix(f.Type, "[]") {
				continue
			}
			inner, _ := st.def(f.Type[:len(f.Type)-2])
			for j, nf := range inner.Fields {
				for k := 0; k < 20; k++ {
					msg := g.value(st.name).(Struct)
					item := g.value(inner.Name).(Struct)
					item[j] = g.bad(nf.Type)
					msg[i] = List{g.value(inner.Name), item}
					check(msg)
				}
			}
			msg := g.value(st.name).(Struct)
			msg[i] = List{"not a struct"}
			check(msg)
		}
		check("not a struct")
		check(Struct{})
	}
}

// -- benchmarks: go test -run '^$' -bench . -benchmem ---------------------------------------------

func benchOrder() Value {
	leg := func(asset string, size int64, price int64, buy bool) Value {
		a, _ := new(big.Int).SetString(asset, 0)
		return Struct{a, big.NewInt(size), big.NewInt(price), buy}
	}
	return Struct{
		new(big.Int).SetUint64(8289849667772468), false, big.NewInt(1), false, false,
		List{leg("0x030501", 1_000_000_000, 65_000_000_000_000, true), leg("0x030401", 2_000_000_000, 3_000_000_000_000, false)},
		big.NewInt(1234567), big.NewInt(1_730_000_000_000_000_000),
	}
}

func BenchmarkOrderHash(b *testing.B) {
	msg, d := benchOrder(), ExchangeDomain(325)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := eip712Order.Digest(d, msg); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOrderHashReference(b *testing.B) {
	msg, d := benchOrder(), ExchangeDomain(325)
	ref, _ := newRefType(eip712Order)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := ref.Digest(d, msg); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOrderHashSign(b *testing.B) {
	msg, d := benchOrder(), ExchangeDomain(325)
	s, _ := NewPrivateKeySigner("0x" + strings.Repeat("ab", 32))
	ctx := context.Background()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := s.SignTyped(ctx, d, eip712Order, msg); err != nil {
			b.Fatal(err)
		}
	}
}

// External signers: the runtime accepts only a signature that recovers to the signer's address for
// the SDK's own digest.
func TestVerifyExternalSignature(t *testing.T) {
	key, _ := NewPrivateKeySigner("0x" + strings.Repeat("ab", 32))
	other, _ := NewPrivateKeySigner("0x" + strings.Repeat("cd", 32))
	d := ExchangeDomain(325)
	digest, err := eip712Order.Digest(d, benchOrder())
	if err != nil {
		t.Fatal(err)
	}
	good := key.SignDigest(digest)
	if err := verifySignature(digest, good, key.Address()); err != nil {
		t.Fatal(err)
	}
	var wrongDigest [32]byte
	highS := good
	highS.S = "0xffffffffffffffffffffffffffffffffbaaedce6af48a03bbfd25e8cd0364140"
	for name, c := range map[string]struct {
		digest [32]byte
		sig    Signature
	}{
		"other key":     {digest, other.SignDigest(digest)},
		"other message": {digest, key.SignDigest(wrongDigest)},
		"high s":        {digest, highS},
		"bad v":         {digest, Signature{R: good.R, S: good.S, V: 2}},
	} {
		var se *SigningError
		if err := verifySignature(c.digest, c.sig, key.Address()); !errors.As(err, &se) {
			t.Fatalf("%s: want a SigningError, got %v", name, err)
		}
	}
}
