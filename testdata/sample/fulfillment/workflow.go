// Package fulfillment ships paid orders with a Temporal workflow.
package fulfillment

import (
	"context"
	"time"

	"go.temporal.io/sdk/workflow"
)

// ShipOrder is the workflow shipping a paid order: it reserves the items,
// then has the inventory service notify the customer.
func ShipOrder(ctx workflow.Context, orderID string) error {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute})
	if err := workflow.ExecuteActivity(ctx, (*Activities).Reserve, orderID).Get(ctx, nil); err != nil {
		return err
	}
	return workflow.ExecuteActivity(ctx, "NotifyCustomer", orderID).Get(ctx, nil)
}

// ReturnOrder is the workflow taking back a returned order.
func ReturnOrder(ctx workflow.Context, orderID string) error {
	return workflow.ExecuteActivity(ctx, (*Activities).Release, orderID).Get(ctx, nil)
}

// Activities are the fulfillment activities, sharing one warehouse.
type Activities struct {
	Warehouse string
}

// Reserve holds an order's items in the warehouse.
func (a *Activities) Reserve(ctx context.Context, orderID string) error { return nil }

// Release frees the items of a returned order.
func (a *Activities) Release(ctx context.Context, orderID string) error { return nil }

func (a *Activities) audit(orderID string) {}
