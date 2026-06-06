package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// ReconcileStats summarizes what ReconcileFromStripe did.
type ReconcileStats struct {
	Listed   int // how many subscriptions Stripe returned
	Upserted int // how many made it past the StripeUpdatedAt watermark
}

// ErrSubjectReconcileUnsupported is returned by ReconcileSubject when the
// configured Backend does not implement CustomerSubscriptionLister (the
// optional capability that scopes the Stripe list to one customer). Callers
// can detect it (errors.Is) and fall back to the global ReconcileFromStripe.
var ErrSubjectReconcileUnsupported = errors.New("subscriptions: backend does not support subject-scoped reconcile")

// CustomerSubscriptionLister is an OPTIONAL Backend capability: list every
// subscription belonging to a single Stripe customer (status=all). Backends
// that implement it enable ReconcileSubject — an O(one customer) reconcile —
// instead of the O(all-subs-in-window) global ListSince sweep. The stripe-api
// backend implements it; test fakes need not. It is a separate interface (not
// part of Backend) precisely so adding it is not a breaking change for existing
// Backend implementers.
type CustomerSubscriptionLister interface {
	ListByCustomer(ctx context.Context, stripeCustomerID string) ([]ReconciledSubscription, error)
}

// ReconcileFromStripe lists Stripe subscriptions updated since `since`
// and upserts each via the mirror. Belt-and-suspenders against
// webhook delivery gaps: if the endpoint was down for an hour and
// Stripe gave up retrying some events, this catches them up.
//
// The repo's stale-event watermark means re-running with overlapping
// time windows is safe — already-current rows are silently skipped.
//
// Apps schedule this (e.g. nightly) as a safety net. Run inside a
// reasonable timeout: for huge subscription bases (>1000s) the call
// is paginated by stripe-go but still takes proportional time.
func (o *Operations) ReconcileFromStripe(ctx context.Context, since time.Time, logger *slog.Logger) (ReconcileStats, error) {
	subs, err := o.backend.ListSince(ctx, since)
	if err != nil {
		return ReconcileStats{}, fmt.Errorf("subscriptions.ReconcileFromStripe: list: %w", err)
	}
	return o.upsertReconciled(ctx, subs, "ReconcileFromStripe", logger), nil
}

// ReconcileSubject reconciles ONLY the subscriptions belonging to a single
// Stripe customer — the surgical counterpart to ReconcileFromStripe's global
// sweep. Use it on the synchronous request path (e.g. the org-delete cancel
// cascade) where reconciling every subject's recently-touched subscription
// would be wasteful: it lists O(one customer) instead of O(all-subs-in-window).
//
// Requires the Backend to implement CustomerSubscriptionLister; otherwise it
// returns ErrSubjectReconcileUnsupported so the caller can fall back to
// ReconcileFromStripe. An empty stripeCustomerID is a no-op (a subject with no
// Stripe customer can have no subscriptions): returns zero stats, nil error.
func (o *Operations) ReconcileSubject(ctx context.Context, stripeCustomerID string, logger *slog.Logger) (ReconcileStats, error) {
	if stripeCustomerID == "" {
		return ReconcileStats{}, nil
	}
	lister, ok := o.backend.(CustomerSubscriptionLister)
	if !ok {
		return ReconcileStats{}, ErrSubjectReconcileUnsupported
	}
	subs, err := lister.ListByCustomer(ctx, stripeCustomerID)
	if err != nil {
		return ReconcileStats{}, fmt.Errorf("subscriptions.ReconcileSubject(%s): list: %w", stripeCustomerID, err)
	}
	return o.upsertReconciled(ctx, subs, "ReconcileSubject", logger), nil
}

// upsertReconciled writes each Stripe-projected subscription into the mirror,
// skipping (and logging) individual upsert failures. The repo's stale-event
// watermark makes overlapping reconciles safe. Shared by ReconcileFromStripe
// and ReconcileSubject so both go through one upsert path.
func (o *Operations) upsertReconciled(ctx context.Context, subs []ReconciledSubscription, op string, logger *slog.Logger) ReconcileStats {
	if logger == nil {
		logger = slog.Default()
	}
	stats := ReconcileStats{Listed: len(subs)}
	for _, rs := range subs {
		mirror := &Subscription{
			StripeID:           rs.StripeID,
			SubjectID:          SubjectID(rs.Metadata["subject_id"]),
			StripeCustomerID:   rs.StripeCustomerID,
			Status:             rs.Status,
			Items:              rs.Items,
			CurrentPeriodStart: rs.CurrentPeriodStart,
			CurrentPeriodEnd:   rs.CurrentPeriodEnd,
			CancelAtPeriodEnd:  rs.CancelAtPeriodEnd,
			CanceledAt:         rs.CanceledAt,
			EndedAt:            rs.EndedAt,
			TrialStart:         rs.TrialStart,
			TrialEnd:           rs.TrialEnd,
			Metadata:           rs.Metadata,
			UpdatedAt:          o.now(),
			StripeUpdatedAt:    time.Unix(rs.UpdatedUnix, 0),
		}
		if err := o.repo.Upsert(ctx, mirror); err != nil {
			logger.WarnContext(ctx, "subscriptions."+op+": upsert failed",
				"stripe_id", rs.StripeID, "err", err)
			continue
		}
		stats.Upserted++
	}
	return stats
}
