package metering

import (
	"context"
	"sort"
	"sync"
	"time"
)

// UsageRepo is the storage interface for metering events. Apps
// implement against their own DB (the lib never touches storage
// directly per ADR-0002). MemoryRepo is for tests and demos.
type UsageRepo interface {
	// RecordUsage inserts a meter event. Idempotent on
	// (subject, metric, request_id): duplicate calls with the same
	// triple no-op.
	RecordUsage(ctx context.Context, evt MeterEvent) error

	// AggregateByPeriod sums Quantity per (subject, metric) for events
	// whose OccurredAt falls within [period.Start, period.End). Used
	// by the reconcile path to compute per-subject totals to push to
	// Stripe.
	AggregateByPeriod(ctx context.Context, metric string, period Period) ([]UsageAggregate, error)

	// QueryByPeriod returns the total for a single subject + metric
	// over [start, end). Used by in-app dashboards and quota checks.
	QueryByPeriod(ctx context.Context, subject SubjectID, metric string, start, end time.Time) (int64, error)

	// PruneOlderThan deletes events older than `before`. Apps schedule
	// this for retention. Returns the count deleted.
	PruneOlderThan(ctx context.Context, before time.Time) (int, error)
}

// NewMemoryRepo returns an in-memory UsageRepo for tests + demos.
func NewMemoryRepo() UsageRepo {
	return &memoryRepo{
		events: map[string]bool{}, // idempotency key set
	}
}

type memoryRepo struct {
	mu     sync.Mutex
	rows   []MeterEvent
	events map[string]bool
}

func (r *memoryRepo) RecordUsage(_ context.Context, evt MeterEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := string(evt.SubjectID) + "|" + evt.Metric + "|" + evt.RequestID
	if evt.RequestID != "" && r.events[key] {
		return nil // dup
	}
	if evt.OccurredAt.IsZero() {
		evt.OccurredAt = time.Now()
	}
	r.rows = append(r.rows, evt)
	if evt.RequestID != "" {
		r.events[key] = true
	}
	return nil
}

func (r *memoryRepo) AggregateByPeriod(_ context.Context, metric string, period Period) ([]UsageAggregate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	totals := map[SubjectID]int64{}
	for _, e := range r.rows {
		if e.Metric != metric {
			continue
		}
		if e.OccurredAt.Before(period.Start) || !e.OccurredAt.Before(period.End) {
			continue
		}
		totals[e.SubjectID] += e.Quantity
	}
	out := make([]UsageAggregate, 0, len(totals))
	for s, t := range totals {
		out = append(out, UsageAggregate{SubjectID: s, Metric: metric, Total: t, Period: period})
	}
	// Deterministic order for testability.
	sort.Slice(out, func(i, j int) bool { return out[i].SubjectID < out[j].SubjectID })
	return out, nil
}

func (r *memoryRepo) QueryByPeriod(_ context.Context, subject SubjectID, metric string, start, end time.Time) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var total int64
	for _, e := range r.rows {
		if e.SubjectID != subject || e.Metric != metric {
			continue
		}
		if e.OccurredAt.Before(start) || !e.OccurredAt.Before(end) {
			continue
		}
		total += e.Quantity
	}
	return total, nil
}

func (r *memoryRepo) PruneOlderThan(_ context.Context, before time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.rows[:0]
	deleted := 0
	for _, e := range r.rows {
		if e.OccurredAt.Before(before) {
			deleted++
			continue
		}
		kept = append(kept, e)
	}
	r.rows = kept
	return deleted, nil
}
