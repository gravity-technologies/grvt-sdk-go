package grvt

import (
	"context"
	"errors"
	"testing"

	"github.com/gravity-technologies/grvt-sdk-go/models"
)

type fakeCountdown struct {
	calls int
	err   error
	last  models.ApiCancelOnDisconnectRequest
}

func (f *fakeCountdown) CancelOnDisconnect(_ context.Context, req *models.ApiCancelOnDisconnectRequest) (*models.AckResponse, error) {
	f.calls++
	f.last = *req
	return &models.AckResponse{Result: models.Ack{Ack: true}}, f.err
}
func TestCountdownSingleExplicitWrite(t *testing.T) {
	client := &fakeCountdown{}
	receipt, err := RefreshCancelCountdown(context.Background(), client, 1, 120)
	if err != nil || !receipt.Acknowledged || receipt.Scope() != "sub_account" || *client.last.CountdownTime != 120000 {
		t.Fatalf("bad countdown: %+v %v", receipt, err)
	}
	for _, seconds := range []uint16{0, 301} {
		if _, err = RefreshCancelCountdown(context.Background(), client, 1, seconds); err == nil {
			t.Fatal("accepted invalid countdown")
		}
	}
	if client.calls != 1 {
		t.Fatal("invalid countdown sent")
	}
	client.err = errors.New("unknown result")
	if _, err = RefreshCancelCountdown(context.Background(), client, 1, 120); !errors.Is(err, client.err) {
		t.Fatal("lost unknown outcome")
	}
	if client.calls != 2 {
		t.Fatal("retried countdown")
	}
}
