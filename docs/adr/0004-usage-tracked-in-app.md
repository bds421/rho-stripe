# ADR-0004: Usage tracking owned by the app; Stripe receives periodic aggregates

- **Status:** Accepted
- **Date:** 2026-05-26
- **Deciders:** Markus

## Context

Usage-based billing (metered pricing) is one of the four billing models in scope ([[project-charge-model]] memory). Naively, every billable action (an API call, an AI completion, a bandwidth unit) could be sent directly to Stripe as a meter event. In practice, this fails for any non-trivial volume:

- Stripe's `billing.meter_event.create` is rate-limited and has perceptible latency. A hot-path API call now blocks on a network call to Stripe.
- Stripe is not a usage analytics database. "How many calls did org X make in the last 10 minutes" (for in-app quota displays, anomaly detection, abuse investigation) is not a Stripe-queryable shape with low latency.
- Stripe Sigma queries cost money and run on Stripe's clock.
- Stripe outages would lose hot-path usage events, requiring a local buffer anyway.

Mature usage-billing systems (Datadog, Snowflake, OpenAI, Vercel, Cloudflare) all converge on the same pattern: own the usage data, push aggregates to the billing system periodically.

## Decision

**Usage tracking lives in the integrating app's database. Stripe receives aggregated counters on a periodic schedule (default hourly), not per-event.**

### Hot path (per request, sub-millisecond)

```go
err := conn.Metering.Record(ctx, MeterEvent{
    SubjectID:   subjectID,
    Metric:    "api_call",
    Quantity:  1,
    RequestID: reqID,        // idempotency
    OccurredAt: time.Now(),
})
```

Writes to `UsageRepo` in the app's DB. Synchronous, no network call to Stripe. Powers in-app dashboards, quotas, anomaly detection. Idempotent on `RequestID` — duplicate calls within the same logical request don't double-count.

### Cold path (scheduled, default hourly)

```go
err := conn.Metering.ReconcileToStripe(ctx, Period{
    Start: hourStart, End: hourEnd,
})
```

Reads aggregates from `UsageRepo` for the period, pushes one `meter_event` per (subject, metric) tuple to Stripe with an idempotency key derived from `(subject, metric, period_start)`. Safe to re-run any number of times; Stripe deduplicates on the idempotency key.

Apps schedule `ReconcileToStripe` via their preferred mechanism (cron, queue worker, k8s CronJob). The lib does NOT run a background scheduler — that's an app concern.

### Escape hatch: `RecordDirect` for low-volume metrics

For metrics with intrinsically low volume (e.g. per-invoice fees, per-account audit checks, per-month billable events), the lib also exposes:

```go
err := conn.Metering.RecordDirect(ctx, MeterEvent{...})
```

Which pushes one `meter_event` to Stripe immediately, bypassing the local buffer. Same interface shape; integrator picks per metric based on volume. Documented trade-off: `Record` for any metric that could exceed ~10 events/sec; `RecordDirect` for genuinely sparse cases.

### Aggregation period

Default: hourly. Configurable per metric via the catalog's metering declaration. Reasoning: small enough that Stripe-side spend alerts fire usefully on overage; large enough that 1M API calls don't become 1M Stripe writes (≈ 1M/hour vs ≈ 8.7K/year of meter events).

## Options considered

### Direct push per event to Stripe — rejected

Adds Stripe latency to every billable request. Fails under outages. Burns Stripe rate-limit budget. Forces apps to add a local buffer anyway for reliability — at which point the buffer might as well be the source of truth.

### Push per request but with local buffer for retry — rejected

A retry-on-fail wrapper around direct push still incurs Stripe latency on the happy path and adds complex retry state. Doesn't solve the in-app analytics problem.

### Aggregate hourly + push (chosen) — accepted

Decouples hot-path performance from Stripe availability. Provides queryable usage history in the app DB for free. Matches industry convention. The trade-off — slightly delayed Stripe-side spend visibility — is mitigated by the hourly cadence (Stripe alerts still fire within an hour of overage).

### Aggregate daily — rejected

Better cost amortization but worse spend-alert timing. An app could accumulate 24 hours of overage before Stripe sees the spike. Hourly hits the right balance for B2B SaaS workloads.

## Consequences

### Positive

- Hot-path latency is bounded by `UsageRepo` write speed (typically <5ms for a Postgres insert), not Stripe API latency.
- In-app dashboards / quotas query the same data Stripe will eventually see — single source of truth.
- Stripe outages don't drop billable events; reconciliation catches up on the next scheduled run.
- Costs are predictable: O(metrics × subjects × hours) Stripe meter writes per day, not O(events).
- Anomaly detection, abuse investigation, refund support — all driven by the app's own queryable history.
- The `RecordDirect` escape hatch covers the small set of cases where aggregation is overkill.

### Negative / accepted risks

- **Apps must schedule `ReconcileToStripe`.** Missed schedules cause Stripe-side state to lag, which can delay invoice generation for usage-billed subscriptions. Mitigation: the lib exposes `conn.Metering.LastReconciledAt(metric)` for monitoring; apps can alert on staleness.
- **`UsageRepo` becomes a high-write table.** Apps must design indices and retention appropriately. Mitigation: reference Postgres impl includes a partitioned-by-day schema option for high-volume cases; documented in the relevant package GoDoc.
- **Aggregation logic adds complexity vs raw event push.** Acceptable; isolated in the `metering` package and well-tested.

### Lib API implications

- `connector/metering` exposes `Record`, `RecordDirect`, `ReconcileToStripe`, `LastReconciledAt`, and `Query` (for in-app usage queries).
- `MeterEvent` and `Period` are exported types; `UsageRepo` interface defines storage contract.
- Catalog declarations for metered prices include the metric name and aggregation period; sync ensures the matching Stripe Meter object exists.

## Out of scope (defer to other docs)

- Exact `UsageRepo` schema for the reference Postgres impl, including partitioning strategy → the relevant package GoDoc.
- How `ReconcileToStripe` handles partial failure (e.g. some subjects succeed, some fail) → the relevant package GoDoc.
- Stripe Meter setup via the catalog (declaring meters, mapping aggregations) → the relevant package GoDoc.
- Interaction between credits ([[adr-0005]]) and metering for the same product (mutually exclusive) → the relevant package GoDoc.
