package payments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bds421/rho-stripe/meta"
)

// RefundReason mirrors Stripe's accepted refund reasons. Use the
// helper constants; any other string is rejected by Stripe.
type RefundReason string

const (
	RefundReasonDuplicate           RefundReason = "duplicate"
	RefundReasonFraudulent          RefundReason = "fraudulent"
	RefundReasonRequestedByCustomer RefundReason = "requested_by_customer"
)

// RefundInput describes a refund request.
//
// Exactly one of (ChargeID, PaymentIntentID) is required; populating
// both is an error. Amount = 0 means "full refund". Amount > 0 means
// a partial refund of that many smallest-currency units.
type RefundInput struct {
	ChargeID        string
	PaymentIntentID string

	// Amount in the original charge's currency (smallest unit). 0 = full refund.
	Amount int64

	// Reason is stored on Stripe's refund object. Optional; defaults
	// to RefundReasonRequestedByCustomer when empty.
	Reason RefundReason

	// Metadata is propagated to the Stripe refund object. The
	// connector facade stamps app_namespace for routing.
	Metadata map[string]string
}

// Refund is the projection returned by Operations.Refund.
type Refund struct {
	StripeID  string
	ChargeID  string
	Amount    int64
	Currency  string
	Status    string // "pending" | "succeeded" | "failed" | "canceled" | "requires_action"
	Reason    string
	CreatedAt time.Time
}

// Backend is the Stripe-side surface the refund operations depend on.
type Backend interface {
	Refund(ctx context.Context, in RefundInput) (Refund, error)
}

// Operations is the public refund-management surface. Constructed by
// the connector facade; exposed as conn.Refunds.
//
// Use IssueCreditNote (invoices package) instead when EU compliance
// matters — credit notes are the legally-required document for
// VAT-registered transactions. Refunds are the right tool for one-off
// charges, B2C scenarios, or when no invoice was issued.
type Operations struct {
	backend   Backend
	namespace string
}

// New constructs Operations.
func New(backend Backend, opts ...Option) *Operations {
	if backend == nil {
		panic("payments.New: backend is required")
	}
	o := &Operations{backend: backend}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// Option configures Operations.
type Option func(*Operations)

// WithNamespace stamps app_namespace on every created refund's
// metadata. The connector facade always passes this.
func WithNamespace(ns string) Option {
	return func(o *Operations) { o.namespace = ns }
}

// Refund issues a refund against a Charge or PaymentIntent.
//
// Validation:
//   - Exactly one of ChargeID / PaymentIntentID required.
//   - Amount must be >= 0 (0 = full refund).
//   - Reason defaults to "requested_by_customer".
func (o *Operations) Refund(ctx context.Context, in RefundInput) (Refund, error) {
	switch {
	case in.ChargeID == "" && in.PaymentIntentID == "":
		return Refund{}, errors.New("payments.Refund: ChargeID or PaymentIntentID is required")
	case in.ChargeID != "" && in.PaymentIntentID != "":
		return Refund{}, errors.New("payments.Refund: provide ChargeID OR PaymentIntentID, not both")
	}
	if in.Amount < 0 {
		return Refund{}, fmt.Errorf("payments.Refund: Amount must be >= 0 (0=full refund), got %d", in.Amount)
	}
	if in.Reason == "" {
		in.Reason = RefundReasonRequestedByCustomer
	} else {
		switch in.Reason {
		case RefundReasonDuplicate, RefundReasonFraudulent, RefundReasonRequestedByCustomer:
		default:
			return Refund{}, fmt.Errorf("payments.Refund: invalid Reason %q (use one of duplicate/fraudulent/requested_by_customer)", in.Reason)
		}
	}
	in.Metadata = meta.StampNamespace(in.Metadata, o.namespace)
	return o.backend.Refund(ctx, in)
}
