// Package checkout turns a cart into a paid order.
package checkout

import (
	"fmt"

	"example.com/sample/payments"
)

// Order is a paid cart.
type Order struct {
	ID       string
	ChargeID string
}

// Checkout charges the customer for the cart total and creates an order.
func Checkout(gw payments.Gateway, customerID string, cents int64) (*Order, error) {
	svc := payments.NewPaymentService(gw)
	chargeID, err := svc.ProcessPayment(customerID, cents)
	if err != nil {
		return nil, fmt.Errorf("checkout: %w", err)
	}
	return newOrder(chargeID), nil
}

func newOrder(chargeID string) *Order {
	return &Order{ID: "ord_" + chargeID, ChargeID: chargeID}
}

// audit charges through a service it gets from a function value. Its type
// is not stated anywhere the index can see, so the call is linked by the
// method's unique name.
func audit(lookup func() *payments.PaymentService) {
	lookup().ProcessPayment("audit", 1)
}
