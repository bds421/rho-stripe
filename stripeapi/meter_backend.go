package stripeapi

import (
	"context"
	"fmt"
	"strconv"

	"github.com/bds421/rho-stripe/metering"
	stripe "github.com/stripe/stripe-go/v82"
)

// MeterBackend implements metering.Backend against Stripe's billing
// meter_event API.
type MeterBackend struct {
	sc *stripe.Client
}

// NewMeterBackend wraps a stripe-go client.
func NewMeterBackend(sc *stripe.Client) *MeterBackend {
	if sc == nil {
		panic("stripeapi.NewMeterBackend: StripeClient is required")
	}
	return &MeterBackend{sc: sc}
}

var _ metering.Backend = (*MeterBackend)(nil)

// PushBatch sends each event sequentially via BillingMeterEvents.Create.
// Stripe doesn't have a batch endpoint; we make one HTTP call per
// event. Acceptable for typical reconcile cadence (hourly, hundreds
// of subjects).
func (b *MeterBackend) PushBatch(ctx context.Context, events []metering.PushEvent) error {
	for _, e := range events {
		params := &stripe.BillingMeterEventCreateParams{
			EventName:  stripe.String(e.EventName),
			Identifier: stripe.String(e.Identifier),
			Payload: map[string]string{
				"stripe_customer_id": e.StripeCustomer,
				"value":              strconv.FormatInt(e.Value, 10),
			},
		}
		if e.TimestampUnix > 0 {
			params.Timestamp = stripe.Int64(e.TimestampUnix)
		}
		// Meter events already dedup server-side on Identifier, but
		// idempotency-key protects against transport-layer dupes
		// (network retry of the same HTTP POST).
		applyIdem(params, ctx, "meter_event.push",
			e.EventName, e.Identifier, e.StripeCustomer,
			strconv.FormatInt(e.Value, 10),
			strconv.FormatInt(e.TimestampUnix, 10))
		if _, err := b.sc.V1BillingMeterEvents.Create(ctx, params); err != nil {
			return fmt.Errorf("stripe.BillingMeterEvents.Create(%s, %s): %w", e.EventName, e.StripeCustomer, err)
		}
	}
	return nil
}
