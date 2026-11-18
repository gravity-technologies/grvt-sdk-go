package wirejson

import (
	"encoding/json"
	"strings"
	"testing"
)

// FuzzSkip: Skip accepts exactly the texts encoding/json accepts. Run it with
// go test -run '^$' -fuzz FuzzSkip ./internal/wirejson.
func FuzzSkip(f *testing.F) {
	for _, seed := range []string{
		`{"a":[1,-0.5e+3,"é",true,null,{}]}`, `"\ud800"`, "\"\xff\"", "01", "1.", `{"a":1,}`, "[] []",
		strings.Repeat("[", 64) + strings.Repeat("]", 64),
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, doc []byte) {
		r := NewReader(doc)
		err := r.Skip()
		if err == nil {
			err = r.End()
		}
		if want := json.Valid(doc); want != (err == nil) {
			t.Fatalf("%q: encoding/json valid %t, Skip %v", doc, want, err)
		}
	})
}
