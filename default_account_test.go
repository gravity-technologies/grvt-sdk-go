package grvt

import (
	"context"
	"testing"

	"github.com/gravity-technologies/grvt-sdk-go/models"
)

func TestDefaultAccountAndOrderCopy(t *testing.T) {
	g := newFakeGateway(t)
	var captured *models.ApiCreateOrderRequestInput
	client, _ := testClient(t, g, func(c *Config) {
		c.APIKey = "key"
		c.SubAccountID = 123
		c.BeforeSend = func(call *OutgoingCall) {
			if req, ok := call.Params.(*models.ApiCreateOrderRequestInput); ok {
				captured = req
			}
		}
	})
	req := &models.ApiGetOrderRequest{ClientOrderID: Ptr[uint64](1)}
	if _, err := client.GetOrder(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if req.SubAccountID != 0 {
		t.Fatal("caller request mutated")
	}
	if got := string(g.restBodies[len(g.restBodies)-1]); got != `{"sub_account_id":"123","client_order_id":"1"}` {
		t.Fatal(got)
	}
	order := oneLegOrder()
	order.Order.SubAccountID = nil
	_, _ = client.CreateOrder(context.Background(), order) // no signer; capture precedes signing
	if captured == nil || captured.Order.SubAccountID == nil || *captured.Order.SubAccountID != 123 {
		t.Fatalf("%+v", captured)
	}
	if order.Order.SubAccountID != nil {
		t.Fatal("caller order mutated")
	}
	order.Order.SubAccountID = Ptr[uint64](456)
	_, _ = client.CreateOrder(context.Background(), order)
	if *captured.Order.SubAccountID != 456 {
		t.Fatal("explicit account replaced")
	}
	selector := &models.WSOrderFeedSelectorV1{}
	stream, err := client.OrderStream(context.Background(), selector)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if stream.sb.selector != "123" || selector.SubAccountID != 0 {
		t.Fatal("selector default or copy failed")
	}
}

func TestTransferSelectorKeepsOptionalAccountOmitted(t *testing.T) {
	for _, defaultID := range []uint64{0, 123} {
		g := newFakeGateway(t)
		client, _ := testClient(t, g, func(c *Config) { c.APIKey = "key"; c.SubAccountID = defaultID })
		for _, explicit := range []*uint64{nil, Ptr[uint64](456)} {
			selector := &models.WSTransferFeedSelectorV1{MainAccountID: "main", SubAccountID: explicit}
			stream, err := client.TransferStream(context.Background(), selector)
			if err != nil {
				t.Fatal(err)
			}
			want := "main"
			if explicit != nil {
				want = "main-456"
			}
			if stream.sb.selector != want || selector.SubAccountID != explicit {
				t.Fatalf("got %s, want %s", stream.sb.selector, want)
			}
			stream.Close()
		}
	}
}
