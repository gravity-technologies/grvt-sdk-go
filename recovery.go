package grvt

import (
	"context"
	"errors"

	"github.com/gravity-technologies/grvt-sdk-go/models"
)

// OrderLookup deliberately exposes only the read needed to resolve an order.
type OrderLookup interface {
	GetOrder(context.Context, *models.ApiGetOrderRequest) (*models.ApiGetOrderResponse, error)
}

type OrderResolutionStatus string

const (
	OrderFound        OrderResolutionStatus = "found"
	OrderStillUnknown OrderResolutionStatus = "still_unknown"
)

// OrderResolution describes an observed ID match, not proof of which submission placed it.
// Reusing client IDs can make different submissions indistinguishable.
type OrderResolution struct {
	Status OrderResolutionStatus
	Order  *models.Order
}

// ResolveOrder only looks up an order. Not found remains unknown: history can lag.
// Neither outcome means it is safe to resend the original order.
func ResolveOrder(ctx context.Context, client OrderLookup, subAccountID, clientOrderID uint64) (OrderResolution, error) {
	unknown := OrderResolution{Status: OrderStillUnknown}
	if subAccountID == 0 || clientOrderID == 0 {
		return unknown, &ConfigError{Message: "sub_account_id and client_order_id must be positive"}
	}
	response, err := client.GetOrder(ctx, &models.ApiGetOrderRequest{SubAccountID: subAccountID, ClientOrderID: &clientOrderID})
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Code == CodeNotFound {
			return unknown, nil
		}
		return unknown, err
	}
	if response == nil {
		return unknown, nil
	}
	order := &response.Result
	if order.SubAccountID != subAccountID || order.Metadata.ClientOrderID != clientOrderID {
		return unknown, nil
	}
	return OrderResolution{Status: OrderFound, Order: order}, nil
}
