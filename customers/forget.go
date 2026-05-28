package customers

import (
	"context"
	"errors"
	"fmt"

	"github.com/bds421/rho-stripe/credits"
	stripe "github.com/stripe/stripe-go/v82"
)

// ForgetReport summarises what the Forget operation did, so callers
// can log it for compliance evidence.
type ForgetReport struct {
	SubjectID             SubjectID
	StripeCustomerID      string
	StripeCustomerDeleted bool
	SubscriptionsRevoked  int
	CreditsForgotten      int
	AppHookCalled         bool
	AppHookError          error
}

// Forget is the GDPR "Right to Erasure" (Article 17) operation.
//
// What it does, in order:
//
//  1. Cancels every active subscription for the customer (Stripe will
//     not allow Customer.delete while active subs exist).
//  2. Revokes every active credit grant for the subject in the local
//     ledger (so usage history retained for audit cannot be re-spent).
//  3. Calls stripe.Customers.Delete — Stripe anonymizes the customer
//     record. NOTE: Stripe retains transactional records (invoices,
//     charges) indefinitely for financial-compliance reasons (SOX,
//     PCI-DSS). This is *legally permitted* under GDPR (Article 17(3)b
//     — compliance with legal obligation).
//  4. Calls the app-supplied AppDataForgetter callback. The app deletes
//     or anonymizes its own records here. If this fails, the lib still
//     considers the Stripe-side deletion done — callers should log the
//     hook error and retry.
//
// Returns a ForgetReport documenting what happened. Even on partial
// failure, callers get a structured report they can persist for audit.
func (o *Operations) Forget(ctx context.Context, s SubjectID) (*ForgetReport, error) {
	stripeID, err := o.resolveStripeID(ctx, s)
	if err != nil {
		return nil, err
	}
	report := &ForgetReport{
		SubjectID:        s,
		StripeCustomerID: string(stripeID),
	}

	// Cancel active subscriptions before deletion — Stripe rejects
	// Customer.delete otherwise. Use the lib's subscription mirror
	// when available (cheaper than a Stripe list call).
	if err := o.cancelActiveSubscriptions(ctx, string(stripeID), report); err != nil {
		return report, fmt.Errorf("customers.Forget: cancel subs: %w", err)
	}

	// Revoke credit grants in the local ledger.
	if o.cfg.CreditRepo != nil {
		grants, err := o.cfg.CreditRepo.ListBySubject(ctx, credits.SubjectID(s))
		if err != nil {
			return report, fmt.Errorf("customers.Forget: list grants: %w", err)
		}
		for _, g := range grants {
			if err := o.cfg.CreditRepo.RevokeGrant(ctx, g.ID, "gdpr_forget"); err != nil {
				return report, fmt.Errorf("customers.Forget: revoke grant %s: %w", g.ID, err)
			}
			report.CreditsForgotten++
		}
	}

	// Stripe Customer.delete — anonymizes record; preserves invoices
	// for tax / legal retention.
	if _, err := o.cfg.StripeClient.V1Customers.Delete(ctx, string(stripeID), nil); err != nil {
		return report, fmt.Errorf("customers.Forget: Customers.Delete(%s): %w", stripeID, err)
	}
	report.StripeCustomerDeleted = true

	// App-side erasure. Soft-fail: lib-side is done either way; caller
	// gets the error in the report and decides what to do.
	if o.cfg.AppDataForgetter != nil {
		report.AppHookCalled = true
		if err := o.cfg.AppDataForgetter(ctx, s); err != nil {
			report.AppHookError = err
			return report, fmt.Errorf("customers.Forget: app-side hook: %w", err)
		}
	}

	return report, nil
}

// cancelActiveSubscriptions cancels every subscription the customer
// still has open. We delegate to Stripe's Cancel endpoint (not the
// lib's Operations.CancelNow) to avoid coupling this package to
// subscriptions.Operations — Forget should work even if the consumer
// hasn't wired the subscriptions module.
func (o *Operations) cancelActiveSubscriptions(ctx context.Context, customerID string, report *ForgetReport) error {
	listParams := &stripe.SubscriptionListParams{
		Customer: stripe.String(customerID),
	}
	listParams.Filters.AddFilter("status", "", "all")
	listParams.Filters.AddFilter("limit", "", "100")
	var ids []string
	for s, err := range o.cfg.StripeClient.V1Subscriptions.List(ctx, listParams) {
		if err != nil {
			return err
		}
		if isTerminalStatus(string(s.Status)) {
			continue
		}
		ids = append(ids, s.ID)
	}
	for _, id := range ids {
		// Prefer the lib's Operations path when wired — it stamps
		// namespace, applies idempotency, and lets the connector's
		// auto-cancel-on-failed-refund flow stay consistent.
		if o.cfg.SubscriptionCanceler != nil {
			if err := o.cfg.SubscriptionCanceler.CancelNowByStripeID(ctx, id); err != nil {
				return fmt.Errorf("SubscriptionCanceler(%s): %w", id, err)
			}
		} else {
			if _, err := o.cfg.StripeClient.V1Subscriptions.Cancel(ctx, id, nil); err != nil {
				return fmt.Errorf("Subscriptions.Cancel(%s): %w", id, err)
			}
		}
		report.SubscriptionsRevoked++
	}
	return nil
}

func isTerminalStatus(s string) bool {
	switch s {
	case "canceled", "incomplete_expired":
		return true
	}
	return false
}

// ErrNoMapping is sentinel for "no Stripe customer for this subject" —
// returned by Export / Forget when the CustomerRepo has no mapping.
// Callers fulfilling GDPR requests should treat this as "we hold no
// data on this subject" and respond accordingly.
var ErrNoMapping = errors.New("customers: no Stripe customer for subject")

// ForgetDisclosure is the plain-language data-retention statement
// apps should include in the response to a GDPR Article-17 request.
// Stripe retains financial records (invoices, charges, refunds)
// indefinitely under legal-obligation grounds; apps fulfilling
// erasure requests must surface this to the data subject.
//
// Use as-is or adapt the wording. Format: plain text, ~3 sentences.
const ForgetDisclosure = `Per Article 17(3)(b) of GDPR, payment-related records ` +
	`(invoices, charges, refunds) are retained by our payment processor (Stripe) ` +
	`indefinitely under legal-obligation grounds (SOX, PCI-DSS, tax-law retention). ` +
	`Your customer record has been anonymized; transactional records remain ` +
	`accessible only to Stripe and our finance / tax-compliance functions, ` +
	`not for marketing or product use. You may request a copy of retained ` +
	`records via the GDPR Article-15 access right at any time.`
