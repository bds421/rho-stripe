package stripeapi

import (
	"context"
	"fmt"
	"time"

	"github.com/bds421/rho-stripe/payments"
	stripe "github.com/stripe/stripe-go/v82"
)

// RefundBackend implements payments.Backend against stripe-go.
type RefundBackend struct {
	sc *stripe.Client
}

// NewRefundBackend wraps a stripe-go client.
func NewRefundBackend(sc *stripe.Client) *RefundBackend {
	if sc == nil {
		panic("stripeapi.NewRefundBackend: StripeClient is required")
	}
	return &RefundBackend{sc: sc}
}

var _ payments.Backend = (*RefundBackend)(nil)

// Refund creates a Stripe Refund.
func (b *RefundBackend) Refund(ctx context.Context, in payments.RefundInput) (payments.Refund, error) {
	params := &stripe.RefundCreateParams{
		Reason: stripe.String(string(in.Reason)),
	}
	if in.ChargeID != "" {
		params.Charge = stripe.String(in.ChargeID)
	}
	if in.PaymentIntentID != "" {
		params.PaymentIntent = stripe.String(in.PaymentIntentID)
	}
	if in.Amount > 0 {
		params.Amount = stripe.Int64(in.Amount)
	}
	for k, v := range in.Metadata {
		params.AddMetadata(k, v)
	}
	applyIdem(params, ctx, "refund.create",
		in.ChargeID, in.PaymentIntentID, canonicalInt64(in.Amount), string(in.Reason),
		canonicalMap(in.Metadata))
	r, err := b.sc.V1Refunds.Create(ctx, params)
	if err != nil {
		return payments.Refund{}, fmt.Errorf("stripe.Refunds.Create: %w", err)
	}
	out := payments.Refund{
		StripeID:  r.ID,
		Amount:    r.Amount,
		Currency:  string(r.Currency),
		Status:    string(r.Status),
		Reason:    string(r.Reason),
		CreatedAt: time.Unix(r.Created, 0).UTC(),
	}
	if r.Charge != nil {
		out.ChargeID = r.Charge.ID
	}
	return out, nil
}
