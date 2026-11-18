package grvt

import (
	"testing"
	"time"

	"github.com/gravity-technologies/grvt-sdk-go/internal/behavior"
)

// The runtime's shared defaults and rules equal sdk/contract/behavior.json (internal/behavior). A failure
// here means the runtime holds its own copy of a shared constant: use the generated one instead.

func TestSharedClientDefaults(t *testing.T) {
	rt, err := NewRuntime(Config{Env: Testnet()}, errorTable)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	checks := []struct {
		name      string
		got, want any
	}{
		{"CallTimeout", rt.callTimeout, behavior.CallTimeout},
		{"HTTPAttemptTimeout", rt.httpAttemptTimeout, behavior.HTTPAttemptTimeout},
		{"session attempt timeout", rt.session.attemptTimeout, behavior.HTTPAttemptTimeout},
		{"WSRequestTimeout", rt.wsRequestTimeout, behavior.WSRequestTimeout},
		{"QueueSize", rt.queueSize, behavior.QueueSize},
		{"session retry delay", rt.sessionRetryDelay, behavior.SessionRetryDelay},
		{"read backoff base", newReadBackoff().Base, behavior.ReadBackoffBase},
		{"read backoff cap", newReadBackoff().Cap, behavior.ReadBackoffCap},
		{"reconnect backoff base", newReconnectBackoff().Base, behavior.ReconnectBackoffBase},
		{"reconnect backoff cap", newReconnectBackoff().Cap, behavior.ReconnectBackoffCap},
		{"md resync timeout", time.Duration(rt.md.resyncTimeout.Load()), behavior.ResyncTimeout},
		{"td resync timeout", time.Duration(rt.td.resyncTimeout.Load()), behavior.ResyncTimeout},
		{"ReadAttempts", ReadAttempts, behavior.ReadAttempts},
		{"refreshAt", refreshAt, behavior.SessionRenewAt},
		{"defaultSessionTTL", defaultSessionTTL, behavior.SessionDefaultLifetime},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestSharedClientOrderIDLayout(t *testing.T) {
	var ids TimeCounterClientOrderID
	before := uint64(time.Now().Unix())
	a, b := ids.NextClientOrderID(), ids.NextClientOrderID()
	after := uint64(time.Now().Unix())
	for _, n := range []uint64{a, b} {
		seconds := n >> behavior.ClientOrderIDCounterBits & behavior.ClientOrderIDSecondsMask
		if n>>63 != 1 || seconds < before || seconds > after {
			t.Fatalf("id %d: top bit %d, seconds %d not in [%d, %d]", n, n>>63, seconds, before, after)
		}
	}
	if b&behavior.ClientOrderIDCounterMask != (a+1)&behavior.ClientOrderIDCounterMask {
		t.Fatalf("counter did not step: %d then %d", a, b)
	}
}

func TestSharedErrorRules(t *testing.T) {
	if RateLimitCode != behavior.RateLimitCode {
		t.Fatalf("RateLimitCode = %d", RateLimitCode)
	}
	for _, s := range behavior.ReadRetryStatuses() {
		if !errorTable.Retryable(99999, s) {
			t.Errorf("status %d is not retryable", s)
		}
	}
	if errorTable.Retryable(99999, 500) {
		t.Error("status 500 is retryable")
	}
	const known = 1003 // a contract code with HTTP status 400
	low, high := behavior.WriteRejectedStatusMin, behavior.WriteRejectedStatusMax
	for status, want := range map[int]bool{low: false, high: false, low - 1: true, high + 1: true} {
		if got := errorTable.uncertain(&APIError{Code: known, Status: status}); got != want {
			t.Errorf("status %d: uncertain = %t, want %t", status, got, want)
		}
	}
}
