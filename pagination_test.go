package grvt

import (
	"context"
	"errors"
	"github.com/gravity-technologies/grvt-sdk-go/models"
	"reflect"
	"testing"
)

// Shared cases (laziness, cycles, fetch errors, stopping early) are in sdk/scenarios/runtime.json.

func TestPagesStopsOnCancellation(t *testing.T) {
	calls := 0
	fetch := func(_ context.Context, _ string) (*models.ApiTradeHistoryResponse, error) {
		calls++
		return &models.ApiTradeHistoryResponse{Next: "a"}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, err := range Pages(ctx, fetch, "") {
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	if calls != 0 {
		t.Fatal("fetched after cancellation")
	}
}

func TestPagesRestartsEachRange(t *testing.T) {
	var calls []string
	fetch := func(_ context.Context, cursor string) (*models.ApiTradeHistoryResponse, error) {
		calls = append(calls, cursor)
		next := ""
		if cursor == "start" {
			next = "b"
		}
		return &models.ApiTradeHistoryResponse{Next: next}, nil
	}
	seq := Pages(context.Background(), fetch, "start")
	for run := 0; run < 2; run++ {
		calls = nil
		for _, err := range seq {
			if err != nil {
				t.Fatal(err)
			}
		}
		if !reflect.DeepEqual(calls, []string{"start", "b"}) {
			t.Fatal(calls)
		}
	}
	calls = nil
	for range seq {
		break
	}
	if !reflect.DeepEqual(calls, []string{"start"}) {
		t.Fatal(calls)
	}
	calls = nil
	for _, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(calls, []string{"start", "b"}) {
		t.Fatal(calls)
	}
}
