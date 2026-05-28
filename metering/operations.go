package metering

import (
	"context"
	"errors"
	"time"
)

// Operations is the public surface apps use for metering. Construct
// via New; the connector facade builds one when Config.Usage is set.
type Operations struct {
	repo UsageRepo
}

// New wraps a UsageRepo in an Operations facade.
func New(repo UsageRepo) *Operations {
	if repo == nil {
		panic("metering.New: repo is required")
	}
	return &Operations{repo: repo}
}

// Record writes a meter event to the app's UsageRepo on the hot path
// (per ADR-0004 — fast local insert, no Stripe call). Idempotent on
// (subject, metric, request_id).
func (o *Operations) Record(ctx context.Context, evt MeterEvent) error {
	if evt.SubjectID == "" {
		return errors.New("metering: MeterEvent.SubjectID is required")
	}
	if evt.Metric == "" {
		return errors.New("metering: MeterEvent.Metric is required")
	}
	if evt.Quantity <= 0 {
		return errors.New("metering: MeterEvent.Quantity must be positive")
	}
	if evt.RequestID == "" {
		return errors.New("metering: MeterEvent.RequestID is required (idempotency)")
	}
	return o.repo.RecordUsage(ctx, evt)
}

// QueryByPeriod returns the subject's metric total over [start, end).
// Used for in-app dashboards and quota enforcement on the hot path.
func (o *Operations) QueryByPeriod(ctx context.Context, subject SubjectID, metric string, start, end time.Time) (int64, error) {
	return o.repo.QueryByPeriod(ctx, subject, metric, start, end)
}

// PruneOldEvents deletes events older than `before`. Apps schedule
// this (e.g. monthly) for retention.
func (o *Operations) PruneOldEvents(ctx context.Context, before time.Time) (int, error) {
	return o.repo.PruneOlderThan(ctx, before)
}

// Repo exposes the raw repo for direct access (aggregate queries,
// custom analytics, etc.).
func (o *Operations) Repo() UsageRepo { return o.repo }
