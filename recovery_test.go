package grvt

import (
	"context"
	"errors"
	"github.com/gravity-technologies/grvt-sdk-go/models"
	"testing"
)

type lookupFunc func(context.Context, *models.ApiGetOrderRequest) (*models.ApiGetOrderResponse, error)

func (f lookupFunc) GetOrder(ctx context.Context, req *models.ApiGetOrderRequest) (*models.ApiGetOrderResponse, error) {
	return f(ctx, req)
}

func TestResolveOrderReadOnly(t *testing.T) {
	for _, notFound := range []bool{false, true} {
		calls := 0
		lookup := lookupFunc(func(_ context.Context, req *models.ApiGetOrderRequest) (*models.ApiGetOrderResponse, error) {
			calls++
			if req.SubAccountID != 1 || *req.ClientOrderID != 2 || req.OrderID != "" {
				t.Fatal("wrong lookup")
			}
			if notFound {
				return nil, &APIError{Code: CodeNotFound}
			}
			return &models.ApiGetOrderResponse{Result: models.Order{SubAccountID: 1, Metadata: models.OrderMetadata{ClientOrderID: 2}}}, nil
		})
		result, err := ResolveOrder(context.Background(), lookup, 1, 2)
		if err != nil || calls != 1 {
			t.Fatalf("calls=%d error=%v", calls, err)
		}
		if notFound && (result.Status != OrderStillUnknown || result.Order != nil) {
			t.Fatal(result)
		}
		if !notFound && (result.Status != OrderFound || result.Order == nil) {
			t.Fatal(result)
		}
	}
	forbidden := &APIError{Code: CodeForbidden}
	_, err := ResolveOrder(context.Background(), lookupFunc(func(context.Context, *models.ApiGetOrderRequest) (*models.ApiGetOrderResponse, error) {
		return nil, forbidden
	}), 1, 2)
	if !errors.Is(err, forbidden) {
		t.Fatal("hid lookup failure")
	}
}
