// Package webhooks receives, verifies, deduplicates, and dispatches
// Stripe events to typed app handlers. Signature verification uses
// stripe-go's webhook.ConstructEvent; idempotency uses rho-kit's
// idempotency.Store.
//
// See docs/adr/0006-webhook-idempotency.md and docs/design/webhooks.md
// for the design.
package webhooks

import (
	stripe "github.com/stripe/stripe-go/v82"
)

// Event is the lib's wrapper around stripe.Event. The wrapper exists
// so the lib's public API doesn't expose stripe-go types directly
// (see adr-0008): app handlers see Event, the lib stays free to evolve
// the internal representation or swap SDK versions without breaking
// consumers.
type Event struct {
	// ID is the Stripe event id, e.g. "evt_1NXxxxxx". Stable for the
	// lifetime of the event in Stripe.
	ID string

	// Type is the Stripe event type, e.g. "checkout.session.completed".
	Type string

	// LiveMode is false for events from test mode.
	LiveMode bool

	// Created is the time Stripe generated the event.
	CreatedUnix int64

	// Raw exposes the underlying stripe-go Event for callers that need
	// fields the wrapper doesn't surface. Treat as unstable across
	// stripe-go upgrades.
	//
	// Common patterns:
	//
	//   // Inspect a typed Stripe object (Stripe documents the data
	//   // shape per event type at https://stripe.com/docs/api/events/types).
	//   var sub stripe.Subscription
	//   if err := json.Unmarshal(evt.Raw.Data.Raw, &sub); err != nil { ... }
	//
	//   // Get the API version Stripe sent (useful when the event
	//   // shape varies by API version):
	//   apiVersion := evt.Raw.APIVersion
	//
	// evt.Raw.Data.Raw is the canonical JSON bytes of the event's
	// data.object field. Use this for typed parsing rather than
	// re-fetching from the Stripe API.
	Raw *stripe.Event
}

func fromStripeEvent(e *stripe.Event) Event {
	return Event{
		ID:          e.ID,
		Type:        string(e.Type),
		LiveMode:    e.Livemode,
		CreatedUnix: e.Created,
		Raw:         e,
	}
}
