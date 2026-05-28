package stripeapi

import (
	"strings"
	"testing"

	stripe "github.com/stripe/stripe-go/v82"
)

// TestStripeAPIVersionPinned guards against accidental stripe-go
// upgrades that bump the Stripe API version without the lib being
// audited for the breaking payload-shape changes a version bump
// brings.
//
// When Stripe publishes a new API version (~quarterly), and a stripe-go
// upgrade picks it up, this test fails. Resolution:
//
//  1. Read the Stripe changelog for the new version.
//  2. Audit every parseSubscription / parseInvoice / parseEvent
//     site in the lib for fields that may have changed shape.
//  3. Update this test's expected version constant + STATUS.md
//     with a note documenting the audit.
//
// Tested versions: as of slice 55, 2025-08-27.basil (paired with
// stripe-go v82.5.1).
func TestStripeAPIVersionPinned(t *testing.T) {
	const expected = "2025-08-27.basil"
	if stripe.APIVersion != expected {
		t.Fatalf(`Stripe APIVersion changed: stripe-go=%q expected=%q

A stripe-go upgrade has bumped the API version. Before un-pinning
this test:
  1. Audit subscriptions/mirror.go parseSubscription against the
     new event payload shape (Stripe changelog).
  2. Audit stripeapi/invoice_backend.go projectInvoice.
  3. Audit webhooks/handler.go classifyNamespace (metadata extraction).
  4. Update docs/STATUS.md with the audit result.
  5. Then bump 'expected' here to the new version.`,
			stripe.APIVersion, expected)
	}
	// Sanity: version starts with year-month — catches typos in expected.
	if !strings.HasPrefix(expected, "20") || len(expected) < 10 {
		t.Fatalf("expected version %q has wrong shape", expected)
	}
}
