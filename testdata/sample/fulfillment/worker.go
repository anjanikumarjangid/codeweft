package fulfillment

import (
	"context"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

const taskQueue = "fulfillment"

// RunWorker serves the fulfillment workflows and activities.
func RunWorker(c client.Client) error {
	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterWorkflow(ShipOrder)
	w.RegisterWorkflowWithOptions(ReturnOrder, workflow.RegisterOptions{Name: "process-return"})
	w.RegisterActivity(&Activities{Warehouse: "main"})
	return w.Run(worker.InterruptCh())
}

// StartShipping starts shipping an order.
func StartShipping(ctx context.Context, c client.Client, orderID string) error {
	opts := client.StartWorkflowOptions{ID: "ship-" + orderID, TaskQueue: taskQueue}
	_, err := c.ExecuteWorkflow(ctx, opts, ShipOrder, orderID)
	return err
}
