// Package subscriptions mirrors Stripe subscription state into the
// integrating app's database so hot-path queries ("is this org on
// the Pro plan?") never hit Stripe. State changes flow Stripe →
// webhook → ApplyEventToMirror → SubscriptionRepo.
//
// Phase-1 scope: mirroring only (read-side helpers + auto-handler
// on subscription events). App-initiated operations
// (CancelAtPeriodEnd, Migrate, etc.) ship in a follow-up slice.
//
// See docs/design/subscription-mirroring.md for the full design.
package subscriptions

import (
	"time"

	"github.com/bds421/rho-stripe/subject"
)

// SubjectID identifies the customer the subscription belongs to.
// Resolved from session/subscription metadata at mirror time.
//
// As of slice 40 this is a type alias of [subject.ID] so values flow
// freely between the connector packages without conversion.
type SubjectID = subject.ID

// Status is the lib's mirror of stripe.SubscriptionStatus.
type Status string

const (
	StatusIncomplete        Status = "incomplete"
	StatusIncompleteExpired Status = "incomplete_expired"
	StatusTrialing          Status = "trialing"
	StatusActive            Status = "active"
	StatusPastDue           Status = "past_due"
	StatusUnpaid            Status = "unpaid"
	StatusCanceled          Status = "canceled"
	StatusPaused            Status = "paused"
)

// IsAccessGranting reports whether the status should grant access to
// app features. Apps usually want this rather than enumerating each
// status — the set is curated to match real-world UX expectations
// (e.g. past_due still grants access during Stripe's smart-retry
// window; unpaid does not).
func (s Status) IsAccessGranting() bool {
	switch s {
	case StatusActive, StatusTrialing, StatusPastDue:
		return true
	default:
		return false
	}
}

// Subscription is the mirror's projection of a Stripe Subscription.
// All fields are app-DB-friendly; no nested stripe-go types leak out.
type Subscription struct {
	StripeID           string
	SubjectID          SubjectID
	StripeCustomerID   string
	Status             Status
	Items              []SubscriptionItem
	CurrentPeriodStart time.Time
	CurrentPeriodEnd   time.Time

	// CancelAtPeriodEnd is true if the customer has scheduled
	// cancellation for the end of the current billing period (the
	// common B2B "cancel" UX). The subscription remains Active until
	// CurrentPeriodEnd.
	CancelAtPeriodEnd bool

	CanceledAt *time.Time // set when cancellation is scheduled or applied
	EndedAt    *time.Time // set when the subscription truly terminated

	TrialStart *time.Time
	TrialEnd   *time.Time

	Metadata map[string]string

	// UpdatedAt is the wall-clock time this mirror row was last
	// written by the lib.
	UpdatedAt time.Time

	// StripeUpdatedAt is the watermark for out-of-order protection:
	//   - For webhook-applied rows: event.created of the most recent
	//     applied event (always monotonically increasing per Stripe).
	//   - For reconcile-applied rows: subscription.created — Stripe's
	//     v1 API doesn't expose a separate "modified" timestamp, so
	//     reconcile uses created as the best-available watermark.
	//     This means a reconcile of a long-lived sub appears "older"
	//     than its recent webhook events; the OOO check correctly
	//     keeps the webhook-derived state.
	//
	// Used as a watermark to reject out-of-order events (Stripe's
	// at-least-once delivery can re-order events under load).
	StripeUpdatedAt time.Time

	// LatestInvoice captures the dunning-relevant state of the most
	// recent invoice. Apps use this to render "Your payment failed —
	// next retry in 3 days" UX without doing a Stripe API round-trip.
	//
	// Populated by the subscription-mirror auto-handler when the
	// underlying event carries the latest_invoice.expansion (it
	// often does for `customer.subscription.updated` events
	// triggered by an invoice state change).
	LatestInvoice *LatestInvoiceState
}

// Schedule is the lib's minimal projection of a Stripe
// SubscriptionSchedule. Returned by Operations.CreateSchedule. Apps
// that need richer state can use conn.Stripe.V1SubscriptionSchedules
// directly — the lib doesn't model the full schedule tree because
// most apps only care about the id (to log it, store it, or call
// Release/Update later).
type Schedule struct {
	StripeID string
	// Status mirrors Stripe's schedule status: "not_started", "active",
	// "completed", "released", "canceled".
	Status string
}

// LatestInvoiceState is a small projection of the customer's most
// recent invoice — just what apps need for dunning UI.
type LatestInvoiceState struct {
	// Status mirrors Stripe's invoice.status ("draft", "open", "paid",
	// "void", "uncollectible").
	Status string

	// AttemptCount is how many times Stripe has tried to charge.
	// Stripe's Smart Retries do up to 4 attempts over ~3 weeks
	// before transitioning the subscription to "unpaid".
	AttemptCount int64

	// NextPaymentAttempt is the unix-time of Stripe's next retry,
	// nil when no retry is scheduled (either succeeded, given up,
	// or not in retry state).
	NextPaymentAttempt *time.Time
}

// InGracePeriod reports whether the subscription is currently in
// Stripe's payment-retry window — `Status==past_due` AND the latest
// invoice has retries remaining.
//
// Apps usually keep customer access during this period and surface
// "payment failed — please update your card" UI.
func (s Subscription) InGracePeriod() bool {
	return s.Status == StatusPastDue
}

// NextPaymentRetryAt returns when Stripe will next attempt to charge,
// or nil when not in a retry state. Convenience over reading
// LatestInvoice.NextPaymentAttempt directly.
func (s Subscription) NextPaymentRetryAt() *time.Time {
	if s.LatestInvoice == nil {
		return nil
	}
	return s.LatestInvoice.NextPaymentAttempt
}

// SmartRetryAttemptCount returns how many payment attempts have been
// made on the latest invoice. Stripe's Smart Retries default cap is
// 4 attempts.
func (s Subscription) SmartRetryAttemptCount() int {
	if s.LatestInvoice == nil {
		return 0
	}
	return int(s.LatestInvoice.AttemptCount)
}

// SubscriptionItem is one line on a subscription. The lib reverses
// the Stripe price ID back to the catalog logical key when possible;
// when the price isn't in the catalog (rare — added by hand in the
// dashboard), PriceKey is the raw price id prefixed with "stripe:".
type SubscriptionItem struct {
	StripeID string
	PriceKey string
	Quantity int64
}
