// Package payments processes customer payments.
package payments

import "errors"

// ErrDeclined is returned when the provider rejects a charge.
var ErrDeclined = errors.New("payment declined")

// Gateway talks to an external payment provider.
type Gateway interface {
	Charge(customerID string, cents int64) (string, error)
	Refund(chargeID string) error
}

// PaymentService coordinates charges and refunds.
type PaymentService struct {
	gw Gateway
}

// NewPaymentService wires a service to a gateway.
func NewPaymentService(gw Gateway) *PaymentService {
	return &PaymentService{gw: gw}
}

// ProcessPayment charges a customer and returns the provider's charge id.
func (s *PaymentService) ProcessPayment(customerID string, cents int64) (string, error) {
	if cents <= 0 {
		return "", errors.New("amount must be positive")
	}
	return s.gw.Charge(customerID, cents)
}
