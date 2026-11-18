package grvt

import (
	"context"
	"fmt"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// Signer plug-in. A signer gets the exact EIP-712 input (domain, one of Grvt's generated signed types,
// message) and returns r/s/v. It needs no Grvt knowledge: a KMS/HSM/wallet signer can call
// primary.TypedData(domain, message) and hand that JSON to its backend. The runtime checks that the
// returned signature recovers to Address() for the SDK's own digest and rejects it otherwise.
type Signer interface {
	// Address is the 0x-prefixed lowercase signer address.
	Address() string
	SignTyped(ctx context.Context, domain *Domain, primary *StructType, message Value) (Signature, error)
}

// Signature is what a Signer returns.
type Signature struct {
	// Signer is the 0x-prefixed lowercase address.
	Signer string
	// R and S are 0x-prefixed 32-byte hex; S is low-s (the server rejects high s, code 2007).
	R, S string
	// V is 27 or 28.
	V uint8
}

// PrivateKeySigner signs with an in-process secp256k1 key. Also fits session keys.
type PrivateKeySigner struct {
	key     *secp256k1.PrivateKey
	address string
}

// NewPrivateKeySigner takes a 32-byte key as hex, with or without 0x.
func NewPrivateKeySigner(privateKey string) (*PrivateKeySigner, error) {
	raw, ok := parseHex(privateKey)
	if !ok || len(raw) != 32 {
		return nil, &ConfigError{Message: "private key must be 32 bytes of hex"}
	}
	key := secp256k1.PrivKeyFromBytes(raw)
	if key.Key.IsZero() {
		return nil, &ConfigError{Message: "bad private key"}
	}
	pub := key.PubKey().SerializeUncompressed()
	h := keccak256(pub[1:])
	return &PrivateKeySigner{key: key, address: Hex0x(h[12:])}, nil
}

func (s *PrivateKeySigner) Address() string { return s.address }

// String never prints the key.
func (s *PrivateKeySigner) String() string { return fmt.Sprintf("PrivateKeySigner(%s)", s.address) }

// SignDigest signs a 32-byte digest (RFC 6979, low-s).
func (s *PrivateKeySigner) SignDigest(digest [32]byte) Signature {
	compact := ecdsa.SignCompact(s.key, digest[:], false) // [27+recid, R, S]
	return Signature{Signer: s.address, R: Hex0x(compact[1:33]), S: Hex0x(compact[33:65]), V: compact[0]}
}

func (s *PrivateKeySigner) SignTyped(_ context.Context, domain *Domain, primary *StructType, message Value) (Signature, error) {
	d, err := primary.Digest(domain, message)
	if err != nil {
		return Signature{}, err
	}
	return s.SignDigest(d), nil
}

// verifySignature checks that sig is a low-s signature of digest that recovers to address (and to
// sig.Signer when set).
func verifySignature(digest [32]byte, sig Signature, address string) error {
	fail := func(why string) error {
		return &SigningError{Message: fmt.Sprintf("signer %s returned an invalid signature: %s", address, why)}
	}
	r, okR := parseHex(sig.R)
	sv, okS := parseHex(sig.S)
	if !okR || !okS || len(r) != 32 || len(sv) != 32 {
		return fail("r and s must be 32 bytes of hex")
	}
	if sig.V != 27 && sig.V != 28 {
		return fail(fmt.Sprintf("v must be 27 or 28, got %d", sig.V))
	}
	var s secp256k1.ModNScalar
	s.SetByteSlice(sv)
	if s.IsOverHalfOrder() {
		return fail("s is not low-s")
	}
	compact := make([]byte, 65)
	compact[0] = sig.V
	copy(compact[1:33], r)
	copy(compact[33:], sv)
	pub, _, err := ecdsa.RecoverCompact(compact, digest[:])
	if err != nil {
		return fail(err.Error())
	}
	raw := pub.SerializeUncompressed()
	h := keccak256(raw[1:])
	got := Hex0x(h[12:])
	if !strings.EqualFold(got, address) {
		return fail("it recovers to " + got + ", not to the signer address (wrong key, or a different message was signed)")
	}
	if sig.Signer != "" && !strings.EqualFold(sig.Signer, address) {
		return fail("its signer field " + sig.Signer + " is not the signer address")
	}
	return nil
}
