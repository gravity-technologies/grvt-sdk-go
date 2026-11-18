package models

import (
	"math/rand/v2"
	"testing"
)

// FuzzDecode compares the one-pass decoders with the encoding/json ones they replaced on arbitrary
// bytes, alone and after an earlier document in the same value. Seeds come from the sampler; run it with
// go test -run '^$' -fuzz FuzzDecode ./models.
func FuzzDecode(f *testing.F) {
	byName := map[string]*legacyModel{}
	for i := range legacyTable {
		byName[legacyTable[i].name] = &legacyTable[i]
	}
	for i := range legacyTable {
		g := &sampler{rng: rand.New(rand.NewPCG(uint64(i), 11)), models: byName} //nolint:gosec // reproducible seeds
		f.Add(uint16(i), g.document(&legacyTable[i], 0), g.document(&legacyTable[i], 1))
	}
	f.Fuzz(func(t *testing.T, model uint16, prev, doc string) {
		m := &legacyTable[int(model)%len(legacyTable)]
		compareDecoders(t, m, doc)
		compareReuse(t, m, prev, doc)
	})
}
