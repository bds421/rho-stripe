package webhooks

import (
	"context"
)

// Handlers is the registry of typed event handler functions an app
// provides. Each field is optional; unset handlers cause the
// dispatcher to skip the event after deduping it (it's still marked
// processed, so it won't be redelivered).
//
// Apps that need an event not yet typed can either use OnOtherEvent
// (catch-all) OR call (*Webhooks).Register(eventType, fn) after
// construction for typed-per-event-type dispatch.
//
// Handlers is a pure value type (no mutexes, no atomics) — safe to
// copy through Config plumbing. The dynamic Register surface lives
// on *Webhooks where the lock naturally fits.
type Handlers struct {
	OnCheckoutCompleted        func(ctx context.Context, evt Event) error
	OnPaymentSucceeded         func(ctx context.Context, evt Event) error
	OnPaymentFailed            func(ctx context.Context, evt Event) error
	OnInvoicePaid              func(ctx context.Context, evt Event) error
	OnInvoicePaymentFailed     func(ctx context.Context, evt Event) error
	OnSubscriptionCreated      func(ctx context.Context, evt Event) error
	OnSubscriptionUpdated      func(ctx context.Context, evt Event) error
	OnSubscriptionCanceled     func(ctx context.Context, evt Event) error
	OnSubscriptionTrialWillEnd func(ctx context.Context, evt Event) error
	OnRefundCreated            func(ctx context.Context, evt Event) error

	// OnRefundFailed fires when Stripe's refund attempt fails (e.g.
	// the original payment method was closed). Apps that want to
	// auto-cancel the related subscription when this happens wire
	// it via Config.AutoCancelSubscriptionOnFailedRefund.
	OnRefundFailed func(ctx context.Context, evt Event) error

	// OnCustomerBalanceFunded fires when a customer's customer_balance
	// changes (typical trigger: incoming bank transfer settled). Apps
	// using B2B bank-transfer flows wire this to trigger fulfillment.
	OnCustomerBalanceFunded func(ctx context.Context, evt Event) error

	// Disputes / chargebacks. The lifecycle is:
	//   1. OnDisputeCreated — customer's bank opened the dispute.
	//      Funds are usually held immediately (OnDisputeFundsWithdrawn).
	//   2. OnDisputeUpdated — evidence due-date set / changed.
	//   3. OnDisputeClosed — won (funds reinstated) or lost (funds gone).
	//      Stripe sends a discriminated event; check evt.Type internally
	//      OR wire OnDisputeFundsReinstated for the "won" case.
	// Apps that don't care about disputes leave all five nil.
	OnDisputeCreated         func(ctx context.Context, evt Event) error
	OnDisputeUpdated         func(ctx context.Context, evt Event) error
	OnDisputeClosed          func(ctx context.Context, evt Event) error
	OnDisputeFundsWithdrawn  func(ctx context.Context, evt Event) error
	OnDisputeFundsReinstated func(ctx context.Context, evt Event) error

	// OnOtherEvent fires for any event type without a dedicated
	// handler above AND not registered via (*Webhooks).Register.
	// Useful as a catch-all for events the lib doesn't yet have
	// typed handlers for AND the app hasn't registered explicitly.
	OnOtherEvent func(ctx context.Context, evt Event) error
}

// dispatch picks the handler for evt and invokes it. Returns nil if
// no handler is registered for the event type (the event will be
// marked processed but no app code runs).
//
// Lookup order:
//  1. dynamically Register-ed handler for evt.Type (overrides typed)
//  2. typed handler field matching evt.Type
//  3. OnOtherEvent (catch-all)
//
// This is a method on *Webhooks (not *Handlers) because the dynamic
// registry lives on Webhooks — Handlers stays a pure value-type.
func (w *Webhooks) dispatchEvent(ctx context.Context, evt Event) error {
	if fn := w.lookupCustomHandler(evt.Type); fn != nil {
		return fn(ctx, evt)
	}
	h := w.handlers
	switch evt.Type {
	case "checkout.session.completed":
		return runIfSet(ctx, evt, h.OnCheckoutCompleted)
	case "payment_intent.succeeded":
		return runIfSet(ctx, evt, h.OnPaymentSucceeded)
	case "payment_intent.payment_failed":
		return runIfSet(ctx, evt, h.OnPaymentFailed)
	case "invoice.paid", "invoice.payment_succeeded":
		return runIfSet(ctx, evt, h.OnInvoicePaid)
	case "invoice.payment_failed":
		return runIfSet(ctx, evt, h.OnInvoicePaymentFailed)
	case "customer.subscription.created":
		return runIfSet(ctx, evt, h.OnSubscriptionCreated)
	case "customer.subscription.updated":
		return runIfSet(ctx, evt, h.OnSubscriptionUpdated)
	case "customer.subscription.deleted":
		return runIfSet(ctx, evt, h.OnSubscriptionCanceled)
	case "customer.subscription.trial_will_end":
		return runIfSet(ctx, evt, h.OnSubscriptionTrialWillEnd)
	case "charge.refunded", "refund.created":
		return runIfSet(ctx, evt, h.OnRefundCreated)
	case "refund.failed", "charge.refund.updated":
		return runIfSet(ctx, evt, h.OnRefundFailed)
	case "customer_balance_funds_reinstated", "customer_balance_transaction.created":
		return runIfSet(ctx, evt, h.OnCustomerBalanceFunded)
	case "charge.dispute.created":
		return runIfSet(ctx, evt, h.OnDisputeCreated)
	case "charge.dispute.updated":
		return runIfSet(ctx, evt, h.OnDisputeUpdated)
	case "charge.dispute.closed":
		return runIfSet(ctx, evt, h.OnDisputeClosed)
	case "charge.dispute.funds_withdrawn":
		return runIfSet(ctx, evt, h.OnDisputeFundsWithdrawn)
	case "charge.dispute.funds_reinstated":
		return runIfSet(ctx, evt, h.OnDisputeFundsReinstated)
	default:
		return runIfSet(ctx, evt, h.OnOtherEvent)
	}
}

func runIfSet(ctx context.Context, evt Event, fn func(context.Context, Event) error) error {
	if fn == nil {
		return nil
	}
	return fn(ctx, evt)
}
