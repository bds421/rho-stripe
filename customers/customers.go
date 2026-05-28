// Package customers groups customer-as-an-aggregate operations that
// don't fit the per-Stripe-resource sub-packages: GDPR data export /
// forget (Stripe + lib + app data), Tax ID management (B2B VAT), and
// backfill for migrating existing Stripe customers into a namespace.
//
// All operations take a SubjectID (the app's stable identity for the
// customer); the package resolves it to a Stripe customer via the
// CustomerRepo and dispatches to the relevant Stripe APIs + lib
// mirrors. App-owned data (orders, support tickets, audit logs) is
// surfaced via caller-supplied callbacks so the lib never has to know
// the schema of every adopting app.
//
// Boundary vs the `checkout` package: checkout owns the per-session
// CustomerRepo interface and the Stripe-customer-creation path that
// happens inline with Checkout-session creation. customers owns the
// *cross-cutting aggregate* — operations that touch a customer's
// entire lifecycle (export, forget, import, tax IDs) rather than
// a single checkout flow. The split is deliberate so a B2C app
// importing only `checkout` doesn't drag in GDPR machinery.
package customers

import (
	"context"
	"fmt"

	"github.com/bds421/rho-stripe/checkout"
	"github.com/bds421/rho-stripe/credits"
	"github.com/bds421/rho-stripe/subject"
	"github.com/bds421/rho-stripe/subscriptions"
	stripe "github.com/stripe/stripe-go/v82"
)

// SubjectID is re-exported from subject for convenience so callers
// don't need to import both packages.
type SubjectID = subject.ID

// Config wires the dependencies the Operations struct needs.
type Config struct {
	// StripeClient is the configured stripe-go client. Required.
	StripeClient *stripe.Client

	// CustomerRepo resolves SubjectID ↔ StripeCustomerID. Required.
	CustomerRepo checkout.CustomerRepo

	// SubscriptionRepo is the local subscription mirror. Optional —
	// when set, Export includes the mirrored state and Forget deletes
	// from it. When nil, only Stripe-side data is exported.
	SubscriptionRepo subscriptions.SubscriptionRepo

	// CreditRepo is the local credits ledger. Optional — when set,
	// Export includes credit grants/usage and Forget deletes them.
	CreditRepo credits.CreditRepo

	// AppDataExporter is called during Export to gather app-owned data
	// for the customer (orders, support tickets, etc.). The returned
	// map is merged into the export under the "app" key. Optional.
	AppDataExporter func(ctx context.Context, s SubjectID) (map[string]any, error)

	// AppDataForgetter is called during Forget AFTER Stripe + lib data
	// have been deleted. The app deletes (or anonymizes) its own
	// records here. Optional but strongly recommended for GDPR
	// compliance — without it, app-owned data persists.
	AppDataForgetter func(ctx context.Context, s SubjectID) error

	// SubscriptionCanceler optionally routes Forget's subscription
	// cancellation through the lib's subscriptions.Operations
	// (preserving idempotency-key, namespace stamp, lifecycle hooks).
	// When nil, Forget uses the raw Stripe client directly — fine for
	// pure GDPR erasure but skips lib-level invariants. Connector
	// auto-wires this when both Customers and Subscriptions configs
	// are set.
	SubscriptionCanceler SubscriptionCanceler
}

// SubscriptionCanceler is the narrow interface customers.Forget calls
// during cleanup. Satisfied by subscriptions.Operations.CancelNow but
// kept here to avoid an import cycle.
type SubscriptionCanceler interface {
	CancelNowByStripeID(ctx context.Context, stripeSubID string) error
}

// Operations is the entry point for customer-aggregate operations.
// Construct via New, then call from the connector facade as
// `conn.Customers.Export(...)` / `Forget(...)` / `AddTaxID(...)` etc.
type Operations struct {
	cfg Config
}

// New validates required deps and returns an Operations. Panics on
// missing required deps (matches lib-wide convention).
func New(cfg Config) *Operations {
	if cfg.StripeClient == nil {
		panic("customers.New: StripeClient is required")
	}
	if cfg.CustomerRepo == nil {
		panic("customers.New: CustomerRepo is required")
	}
	return &Operations{cfg: cfg}
}

// resolveStripeID looks up the StripeCustomerID for a subject, returning
// a descriptive error if no mapping exists.
func (o *Operations) resolveStripeID(ctx context.Context, s SubjectID) (checkout.StripeCustomerID, error) {
	id, ok, err := o.cfg.CustomerRepo.Get(ctx, subject.ID(s))
	if err != nil {
		return "", fmt.Errorf("customers: resolve %s: %w", s, err)
	}
	if !ok {
		return "", fmt.Errorf("customers: no Stripe customer for subject %s", s)
	}
	return id, nil
}
