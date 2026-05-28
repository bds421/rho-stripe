package metering

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"

	"github.com/bds421/rho-stripe/subject"
)

// ReconcileStats summarizes a ReconcileToStripe run.
type ReconcileStats struct {
	Aggregates int // (subject, metric) tuples found
	Pushed     int // entries actually sent to Stripe
}

// Removed in v0.1.0: the standalone `CustomerResolver` func type was
// structurally identical to subject.CustomerRepo.Get. ReconcileToStripe
// now takes a subject.CustomerRepo directly so apps can pass the same
// repo they wired into checkout / customers; no parallel adapter type.

// ReconcileToStripe aggregates usage events from UsageRepo for the
// given metric over [period.Start, period.End) and pushes them to
// Stripe via the Backend.
//
// Idempotency: each push event's Identifier is hash(namespace, metric,
// subject, period.Start) — re-running with the same window pushes the
// same identifiers and Stripe deduplicates. Safe to retry WITHIN
// Stripe's 24h Identifier-dedup window.
//
// CHECKPOINTING (apps must implement): this function does NOT
// persist "we've reconciled up to time T". A crash mid-push leaves
// partial state; the next call must replay the failed window OR all
// hours since the last successful run, or events are lost. Pattern:
//
//	// app maintains its own checkpoint (e.g., a DB row).
//	last := app.LastReconciledHour()  // returns time.Time
//	for hour := last.Add(time.Hour); hour.Before(time.Now()); hour = hour.Add(time.Hour) {
//	    stats, err := metering.ReconcileToStripe(ctx, ..., metering.Period{
//	        Start: hour, End: hour.Add(time.Hour),
//	    }, logger)
//	    if err != nil { return err }    // do NOT advance checkpoint
//	    app.AdvanceCheckpoint(hour)     // safe to skip forward
//	}
//
// Beyond 24h, Stripe's Identifier-dedup expires; re-running an old
// window double-counts. The checkpoint pattern above is the only safe
// way to recover from prolonged outages.
//
// Apps schedule this hourly (or per their chosen aggregation period).
func ReconcileToStripe(
	ctx context.Context,
	repo UsageRepo,
	backend Backend,
	customers subject.CustomerRepo,
	namespace, metric string,
	period Period,
	logger *slog.Logger,
) (ReconcileStats, error) {
	if repo == nil || backend == nil {
		return ReconcileStats{}, errors.New("metering: ReconcileToStripe requires both repo and backend")
	}
	if customers == nil {
		return ReconcileStats{}, errors.New("metering: ReconcileToStripe requires a subject.CustomerRepo (subject → stripe customer)")
	}
	if logger == nil {
		logger = slog.Default()
	}

	aggs, err := repo.AggregateByPeriod(ctx, metric, period)
	if err != nil {
		return ReconcileStats{}, fmt.Errorf("metering: aggregate: %w", err)
	}
	stats := ReconcileStats{Aggregates: len(aggs)}
	if len(aggs) == 0 {
		return stats, nil
	}

	events := make([]PushEvent, 0, len(aggs))
	for _, a := range aggs {
		custID, ok, err := customers.Get(ctx, a.SubjectID)
		if err != nil {
			return stats, fmt.Errorf("metering: customer resolve: %w", err)
		}
		if !ok {
			logger.WarnContext(ctx, "metering: subject has no Stripe Customer; skipping push",
				"subject", a.SubjectID, "metric", metric)
			continue
		}
		events = append(events, PushEvent{
			EventName:      namespace + "." + metric,
			StripeCustomer: string(custID),
			Value:          a.Total,
			Identifier:     reconcileIdentifier(namespace, metric, a.SubjectID, period.Start),
			TimestampUnix:  period.End.Unix(),
		})
	}

	if len(events) == 0 {
		return stats, nil
	}
	if err := backend.PushBatch(ctx, events); err != nil {
		return stats, fmt.Errorf("metering: push: %w", err)
	}
	stats.Pushed = len(events)
	return stats, nil
}

// RecordDirect pushes one meter event to Stripe immediately, bypassing
// the local UsageRepo. Use for genuinely low-volume metrics (per-invoice
// fees, per-month audit events) where aggregation is overkill. Same
// idempotency contract as the reconcile path (Identifier derived from
// inputs).
func RecordDirect(
	ctx context.Context,
	backend Backend,
	customers subject.CustomerRepo,
	namespace, metric string,
	subj SubjectID,
	value int64,
	requestID string,
) error {
	if backend == nil {
		return errors.New("metering: RecordDirect requires a backend")
	}
	if customers == nil {
		return errors.New("metering: RecordDirect requires a subject.CustomerRepo")
	}
	custID, ok, err := customers.Get(ctx, subj)
	if err != nil {
		return fmt.Errorf("metering: RecordDirect: customer resolve: %w", err)
	}
	if !ok {
		return fmt.Errorf("metering: RecordDirect: no Stripe Customer for subject %q", subj)
	}
	identifier := requestID
	if identifier == "" {
		// fall back to hash if caller didn't supply one
		identifier = reconcileIdentifier(namespace, metric, subj, time0)
	}
	return backend.PushBatch(ctx, []PushEvent{{
		EventName:      namespace + "." + metric,
		StripeCustomer: string(custID),
		Value:          value,
		Identifier:     identifier,
	}})
}

// time0 is a sentinel for "no period start" in RecordDirect hashing.
var time0 = mustParseTime0()

func mustParseTime0() (t struct{}) { return }

// reconcileIdentifier produces the deterministic Stripe-dedup string
// for an aggregated (namespace, metric, subject, period) tuple.
func reconcileIdentifier(namespace, metric string, subject SubjectID, periodStart any) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%v", namespace, metric, subject, periodStart)
	return hex.EncodeToString(h.Sum(nil))
}
