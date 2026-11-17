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

// StripeGateway charges cards through Stripe.
type StripeGateway struct {
	apiKey string
}

// Charge creates a Stripe charge.
func (g *StripeGateway) Charge(customerID string, cents int64) (string, error) {
	return "ch_" + customerID, nil
}

// Refund reverses a Stripe charge.
func (g *StripeGateway) Refund(chargeID string) error { return nil }

// Void cancels a charge before it settles.
func (g *StripeGateway) Void(chargeID string) error { return nil }

// VoidingGateway is a gateway that can also cancel unsettled charges.
type VoidingGateway interface {
	Gateway
	Void(chargeID string) error
}

// LoggingGateway logs every call to the Stripe gateway it wraps. It
// implements Gateway through the methods promoted from StripeGateway.
type LoggingGateway struct {
	*StripeGateway
}

// FakeGateway records charges in tests. Its Refund takes no charge id, so
// it does not implement Gateway.
type FakeGateway struct{}

// Charge records a charge.
func (FakeGateway) Charge(customerID string, cents int64) (string, error) { return "fake", nil }

// Refund forgets the last charge.
func (FakeGateway) Refund() error { return nil }

// DeclineError is returned when a charge is declined.
type DeclineError struct{ Code string }

func (e DeclineError) Error() string { return "declined: " + e.Code }

// CodedError is an error with a machine-readable code.
type CodedError interface {
	error
	ErrorCode() string
}

// ErrorCode returns the decline code.
func (e DeclineError) ErrorCode() string { return e.Code }

// AuditedGateway embeds two interfaces that overlap in Charge and Refund,
// which Go allows: its method set is their union.
type AuditedGateway interface {
	Gateway
	VoidingGateway
}

// DoubleGateway embeds two types that both have Void at the same depth.
// The selector d.Void is ambiguous, so DoubleGateway has no Void method
// and is a Gateway but not a VoidingGateway.
type DoubleGateway struct {
	*StripeGateway
	voider
}

type voider struct{}

func (voider) Void(chargeID string) error { return nil }
