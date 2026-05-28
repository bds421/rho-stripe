// Package metering implements usage-based billing per [adr-0004]:
// usage is tracked in the app's database on the hot path and pushed
// to Stripe in aggregated batches via a scheduled reconcile job.
// See docs/design/metering.md for the full design.
package metering

import (
	"time"

	"github.com/bds421/rho-stripe/subject"
)

// SubjectID identifies who's being metered.
//
// As of slice 40 this is a type alias of [subject.ID] so values flow
// freely between the connector packages without conversion.
type SubjectID = subject.ID

// MeterEvent is one billable observation: a customer used something
// metered. The hot path is just `repo.RecordUsage(evt)`; aggregation
// and Stripe push happens elsewhere.
type MeterEvent struct {
	SubjectID    SubjectID
	Metric     string // catalog meter key, e.g. "api_calls"
	Quantity   int64  // amount; commonly 1 for "one event", bigger for batched
	OccurredAt time.Time
	RequestID  string            // idempotency: same (subject, metric, request_id) records once
	Metadata   map[string]string // free-form; preserved on the row but not pushed to Stripe
}

// Period is a [Start, End) time window used by aggregate/query/prune.
type Period struct {
	Start time.Time
	End   time.Time
}

// UsageAggregate sums usage per (subject, metric) over a period.
// Returned by UsageRepo.AggregateByPeriod and pushed to Stripe via
// the reconcile path.
type UsageAggregate struct {
	SubjectID SubjectID
	Metric  string
	Total   int64
	Period  Period
}
