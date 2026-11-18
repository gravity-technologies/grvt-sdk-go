package grvt

import (
	"errors"
	"net/http"
	"testing"
)

func TestRedirectPolicyDoesNotMutateSuppliedClient(t *testing.T) {
	marker := errors.New("caller policy")
	supplied := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return marker }}
	rt, err := NewRuntime(Config{Env: Testnet(), HTTPClient: supplied}, errorTable)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	if rt.http == supplied {
		t.Fatal("runtime must own its client settings")
	}
	if !errors.Is(rt.http.CheckRedirect(nil, nil), http.ErrUseLastResponse) {
		t.Fatal("redirect can replay a POST")
	}
	if !errors.Is(supplied.CheckRedirect(nil, nil), marker) {
		t.Fatal("caller client was changed")
	}
}
