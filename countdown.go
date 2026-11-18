package grvt

import (
	"context"
	"fmt"

	"github.com/gravity-technologies/grvt-sdk-go/internal/behavior"
	"github.com/gravity-technologies/grvt-sdk-go/models"
)

type CountdownClient interface {
	CancelOnDisconnect(context.Context, *models.ApiCancelOnDisconnectRequest) (*models.AckResponse, error)
}

// CountdownReceipt records an acknowledgment, not proof that a timer is still
// active. Another caller can replace the same sub-account's countdown.
type CountdownReceipt struct {
	SubAccountID     uint64
	CountdownSeconds uint16
	Acknowledged     bool
}

func (r CountdownReceipt) Scope() string { return "sub_account" }

// RefreshCancelCountdown arms or refreshes once. It never retries, disarms, or
// extends the countdown automatically on reconnect. Ambiguous writes propagate.
func RefreshCancelCountdown(ctx context.Context, client CountdownClient, subAccountID uint64, countdownSeconds uint16) (CountdownReceipt, error) {
	receipt := CountdownReceipt{SubAccountID: subAccountID, CountdownSeconds: countdownSeconds}
	if subAccountID == 0 || countdownSeconds < behavior.CountdownMinSeconds || countdownSeconds > behavior.CountdownMaxSeconds {
		return receipt, &ConfigError{Message: fmt.Sprintf("positive sub-account and countdown seconds from %d to %d required",
			behavior.CountdownMinSeconds, behavior.CountdownMaxSeconds)}
	}
	ms := uint64(countdownSeconds) * 1000
	response, err := client.CancelOnDisconnect(ctx, &models.ApiCancelOnDisconnectRequest{SubAccountID: subAccountID, CountdownTime: &ms})
	if err != nil {
		return receipt, err
	}
	receipt.Acknowledged = response != nil && response.Result.Ack
	return receipt, nil
}
