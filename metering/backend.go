package metering

import "context"

// Backend is the Stripe-side surface for pushing meter events. The
// default implementation lives in stripeapi; tests inject fakes.
//
// The cold-path reconciliation flow batches local aggregates into
// PushBatch calls, with deterministic identifiers so re-runs don't
// double-bill.
type Backend interface {
	// PushBatch sends an aggregated set of meter events to Stripe.
	// Each entry must carry a deterministic Identifier (hash of
	// subject + metric + period) for Stripe-side dedup.
	PushBatch(ctx context.Context, events []PushEvent) error
}

// PushEvent is one row in the Stripe-side push batch.
type PushEvent struct {
	EventName      string // namespaced metric name (e.g. "app1.api_calls")
	StripeCustomer string // resolved from CustomerRepo upstream
	Value          int64
	Identifier     string // for Stripe dedup
	TimestampUnix  int64  // when the period ended; defaults to now if 0
}
