package grvt

import (
	"encoding/json"
	"testing"

	"github.com/gravity-technologies/grvt-sdk-go/internal/behavior"
)

// Replays sdk/scenarios/decisions.json, the truth table of the generated write and retry decisions.
func TestSharedDecisions(t *testing.T) {
	raw, err := ReadShared("scenarios/decisions.json")
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		WriteOutcomeUnknown []struct {
			Case       string `json:"case"`
			Status     *int   `json:"status"`
			CodeStatus *int   `json:"code_status"`
			Unknown    bool   `json:"unknown"`
		} `json:"write_outcome_unknown"`
		ReadRetryable []struct {
			Case          string `json:"case"`
			Code          int    `json:"code"`
			Status        *int   `json:"status"`
			CodeRetryable bool   `json:"code_retryable"`
			Retryable     bool   `json:"retryable"`
		} `json:"read_retryable"`
		RetryRead []struct {
			Retryable bool `json:"retryable"`
			Attempt   int  `json:"attempt"`
			Retry     bool `json:"retry"`
		} `json:"retry_read"`
	}
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatal(err)
	}
	if len(table.WriteOutcomeUnknown) == 0 || len(table.ReadRetryable) == 0 || len(table.RetryRead) == 0 {
		t.Fatal("decisions.json has an empty section")
	}
	orZero := func(p *int) int { // the Go runtime's "no status"
		if p == nil {
			return 0
		}
		return *p
	}
	for _, r := range table.WriteOutcomeUnknown {
		if got := behavior.WriteOutcomeUnknown(orZero(r.Status), orZero(r.CodeStatus)); got != r.Unknown {
			t.Errorf("write_outcome_unknown %s: got %t", r.Case, got)
		}
	}
	for _, r := range table.ReadRetryable {
		if got := behavior.ReadRetryable(r.Code, orZero(r.Status), r.CodeRetryable); got != r.Retryable {
			t.Errorf("read_retryable %s: got %t", r.Case, got)
		}
	}
	for _, r := range table.RetryRead {
		if got := behavior.RetryRead(r.Retryable, r.Attempt); got != r.Retry {
			t.Errorf("retry_read retryable=%t attempt=%d: got %t", r.Retryable, r.Attempt, got)
		}
	}
}
