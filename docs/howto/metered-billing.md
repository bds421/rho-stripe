# Usage-based / metered billing

Charge per API call, per minute of compute, per GB transferred.

## ADR-0004 architecture

Per-event hot-path traffic is recorded in YOUR database, not Stripe.
A scheduled reconcile job aggregates and pushes to Stripe Meters
hourly (or whatever interval you configure). Stripe is the **biller**,
not the **counter**.

Why: Stripe's BillingMeters API has rate limits + latency that don't
fit "record one event per API call" usage. Your DB does.

## Catalog declaration

```go
spec := catalog.MustSpec(catalog.Spec{
    Namespace: "myapp",
    Products: map[string]catalog.Product{
        "pro": {
            Name:        "Pro Plan",
            TaxCategory: catalog.TaxCategorySaaSBusiness,
            Prices: map[string]catalog.Price{
                "monthly_eur": {Amount: 4900, Currency: "eur",
                    Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
                "api_overage_eur": {Amount: 1, Currency: "eur",
                    Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth,
                    MeterRef: "api_calls_meter"},
            },
        },
    },
    Meters: map[string]catalog.Meter{
        "api_calls_meter": {
            DisplayName: "API calls (€0.001 each over 100K free)",
            EventName:   "api_call",  // namespaced at sync time → "myapp.api_call"
            AggregateBy: "sum",
        },
    },
})
```

`sync --apply` creates BOTH the Product/Price AND the Stripe Meter.

## Hot-path recording

```go
err := conn.Metering.Record(ctx, metering.MeterEvent{
    SubjectID:   "org_acme",
    Metric:    "api_call",  // matches catalog Meter EventName
    Quantity:  1,
    OccurredAt: time.Now(),
    RequestID:  uuid.NewString(),  // idempotency
})
```

`Record` is fast — just an INSERT on your DB. No Stripe call.

## Periodic reconcile to Stripe

```go
// Run hourly (cron, lambda, k8s CronJob, …)
n, err := conn.Metering.ReconcileToStripe(ctx, time.Now().Add(-1*time.Hour), time.Now())
log.Printf("pushed %d aggregates to Stripe", n)
```

`ReconcileToStripe`:
- Groups events by (subject, metric) within the period
- Pushes ONE aggregated `BillingMeterEvent` per group with a
  deterministic Identifier (so Stripe dedupes if the reconcile re-runs)
- Resolves Subject → Stripe Customer via your `CustomerRepo`

## Catching up after an outage

Reconcile didn't run for 4 hours? `CatchUp` walks back-in-time
and reconciles each hourly window:

```go
err := conn.Metering.CatchUp(ctx, time.Now().Add(-6*time.Hour))
```

## Querying for in-app dashboards

```go
total, _ := conn.Metering.QueryByPeriod(ctx, "org_acme", "api_call",
    monthStart, monthEnd)
```

Reads from your DB; no Stripe round-trip.

## See also

- [`examples/per_seat_metered`](../../examples/per_seat_metered) — full per-seat + metered example
- [`examples/included_quota`](../../examples/included_quota) — included-amount + overage metering pattern
