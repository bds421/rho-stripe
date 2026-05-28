package subscriptions

import (
	"context"
	"time"
)

// Backend is the Stripe-side surface for app-initiated subscription
// operations. The default implementation lives in stripeapi; tests
// inject fakes. Mirror updates flow back through the normal webhook
// dispatch path, NOT through this interface (except for ListSince,
// which the reconciliation flow uses to catch up after webhook gaps).
type Backend interface {
	CancelAtPeriodEnd(ctx context.Context, stripeSubID string, cancelAtPeriodEnd bool) error
	CancelNow(ctx context.Context, stripeSubID string, opts CancelNowOptions) error
	UpdateItems(ctx context.Context, stripeSubID string, items []ItemChange, opts UpdateOptions) error
	ApplyCoupon(ctx context.Context, stripeSubID string, couponID string) error
	RemoveCoupon(ctx context.Context, stripeSubID string) error

	// ListSince returns every subscription whose "created or updated
	// at" timestamp is >= since. Used by ReconcileFromStripe.
	ListSince(ctx context.Context, since time.Time) ([]ReconciledSubscription, error)

	// CreateInvoiced creates a subscription billed via invoices
	// (collection_method=send_invoice + days_until_due). Returns the
	// projected Subscription; the mirror's authoritative row will be
	// written by the resulting webhook event.
	CreateInvoiced(ctx context.Context, in InvoicedSubscriptionInput) (*Subscription, error)

	// CreateSubscriptionSchedule wraps Stripe's SubscriptionSchedule
	// API for multi-phase billing. Returns the projected Schedule; the
	// underlying subscription is created automatically on the start
	// date.
	CreateSubscriptionSchedule(ctx context.Context, in ScheduleBackendInput) (*Schedule, error)

	// GetSubscriptionItems fetches the subscription's items directly
	// from Stripe. Used by seat-management helpers (SetSeats /
	// AddSeats) as a fallback when the local mirror hasn't received
	// the subscription's events yet (typical for newly created subs
	// before the first webhook lands).
	//
	// Returns ErrSubscriptionNotFoundInStripe (wrapped) for a 404
	// from Stripe so callers can distinguish "doesn't exist anywhere"
	// from "mirror is stale."
	GetSubscriptionItems(ctx context.Context, stripeSubID string) ([]SubscriptionItem, error)

	// PreviewItemChange returns the proration breakdown for the given
	// item-replacement (without committing it). Apps use this to show
	// "upgrading now will charge €X today" before a Migrate call.
	PreviewItemChange(ctx context.Context, stripeSubID string, items []ItemChange) (MigratePreview, error)

	// Pause sets the subscription's pause_collection on Stripe.
	// Customer keeps access (mirror status stays "active" / "trialing");
	// invoices stop generating per the configured behavior.
	Pause(ctx context.Context, stripeSubID string, opts PauseOptions) error

	// Resume clears pause_collection. Stripe resumes the normal
	// billing cycle from now (or at ResumesAt if a future timestamp
	// was set when pausing).
	Resume(ctx context.Context, stripeSubID string) error
}

// PauseOptions configures Pause.
type PauseOptions struct {
	// Behavior is one of "keep_as_draft" (default — invoices created
	// as drafts the app reviews later), "mark_uncollectible" (invoices
	// generated but immediately marked uncollectible — useful when
	// pausing for retention reasons), or "void" (no invoice at all
	// during the pause). Empty = keep_as_draft.
	Behavior string

	// ResumesAt, when non-zero, schedules an automatic resume.
	// Leave zero to keep paused until explicit Resume.
	ResumesAt time.Time
}

// MigratePreview is the projection of Stripe's upcoming-invoice
// preview for a subscription change.
type MigratePreview struct {
	Currency           string    // ISO 4217, lowercase
	AmountDueNow       int64     // immediately due, smallest currency unit (can be negative if proration credits)
	AmountSubtotal     int64     // pre-tax total
	AmountTax          int64     // tax on AmountDueNow
	NextBillingTotal   int64     // what the next regular invoice will charge
	NextBillingAt      time.Time // when the next regular charge happens
	ProrationLineItems []MigratePreviewLine
}

// MigratePreviewLine is one line in the proration breakdown.
type MigratePreviewLine struct {
	Description string // human-readable from Stripe ("Unused time on Pro Plan", "Remaining time on Enterprise Plan")
	Amount      int64  // smallest currency unit; negative = credit, positive = charge
	PriceID     string // Stripe price id (empty for tax / proration credit lines)
}

// InvoicedSubscriptionInput is the Backend-level shape for creating
// a net-30 (or any days-until-due) subscription. The lib's higher-
// level Operations.CreateInvoiced resolves catalog keys to Stripe
// price ids and customer id from the CustomerRepo before invoking.
type InvoicedSubscriptionInput struct {
	StripeCustomerID string
	StripePriceID    string
	Quantity         int64
	DueIn            time.Duration
	AutoFinalize     bool
	Metadata         map[string]string
	TrialDays        int

	// AddInvoiceItems are one-time charges added to the first
	// invoice generated by this subscription. Typical use: setup
	// fee on an annual plan ("€500 onboarding + €99/month").
	AddInvoiceItems []AddInvoiceItem
}

// AddInvoiceItem describes a one-time charge added to a subscription's
// first invoice.
type AddInvoiceItem struct {
	// StripePriceID references an existing Price (created via catalog
	// sync). Exactly one of StripePriceID / InlineAmount required.
	StripePriceID string

	// InlineAmount + Currency + Name create a one-shot Price inline.
	// Useful for variable setup fees that aren't worth catalog-syncing
	// (one-off enterprise quotes).
	InlineAmount int64
	Currency     string
	Name         string

	// Quantity defaults to 1.
	Quantity int64
}

// ReconciledSubscription is the projection ListSince returns. Mirrors
// the fields ApplyEventToMirror's payload parser produces so the
// reconcile flow can reuse the same upsert path.
type ReconciledSubscription struct {
	StripeID           string
	StripeCustomerID   string
	Status             Status
	CancelAtPeriodEnd  bool
	CanceledAt         *time.Time
	EndedAt            *time.Time
	TrialStart         *time.Time
	TrialEnd           *time.Time
	Metadata           map[string]string
	Items              []SubscriptionItem
	CurrentPeriodStart time.Time
	CurrentPeriodEnd   time.Time
	UpdatedUnix        int64 // for the StripeUpdatedAt watermark
}

// CancelNowOptions configures immediate cancellation behavior.
type CancelNowOptions struct {
	// Prorate, when true, credits the customer for unused time on the
	// final invoice (as a `customer_balance` adjustment). When false,
	// no proration is calculated.
	Prorate bool

	// Reason is the optional `cancellation_details.reason` Stripe
	// records on the canceled subscription. Useful for analytics.
	// Examples: "cancellation_requested", "payment_failed".
	Reason string
}

// UpdateOptions configures item-update behavior (used by Migrate).
type UpdateOptions struct {
	// ProrationBehavior is "create_prorations" (default), "none", or
	// "always_invoice". See Stripe docs for the semantic.
	ProrationBehavior string

	// Cycle-anchor control (whether the billing cycle restarts on
	// update vs. stays on current schedule) is NOT exposed in phase 1.
	// Stripe defaults to "unchanged" — what B2B customers almost
	// always want (predictable billing dates). Apps that need explicit
	// anchor control call the raw client via `conn.Stripe.Subscriptions.Update`.
}

// ItemChange describes one mutation to a subscription's items array.
type ItemChange struct {
	// StripeItemID is the existing item id ("si_…") when modifying or
	// deleting. Leave empty when adding a brand-new item.
	StripeItemID string

	// PriceID is the Stripe price id ("price_…") to attach. Required
	// when adding or replacing; ignored when Deleted is true.
	PriceID string

	// Quantity, when non-zero, sets the line item quantity.
	Quantity int64

	// Deleted, when true, removes the item from the subscription.
	// StripeItemID must be set.
	Deleted bool
}
