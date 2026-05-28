# Catalog drift detection

Someone edits a Product in the Stripe Dashboard. Your spec
no longer matches. `drift-check` catches this.

## One-shot (manual / CI gate)

```bash
rho-stripe drift-check
# exit 0 = no drift; exit 1 = drift detected; prints the report.

rho-stripe drift-check --no-fail   # always exit 0; useful for non-blocking alerts
rho-stripe drift-check --apply     # auto-remediate (re-checks then applies)
```

CI integration: a `drift-check` step in your nightly cron fails the
pipeline on dashboard tampering.

## Programmatic

```go
report, err := catalog.CheckDrift(ctx, backend, spec)
if report.HasDrift {
    fmt.Println(report.FormatHuman())
    // Or pipe each item to a JSON serializer for Slack:
    for _, item := range report.Items {
        slack.Send(fmt.Sprintf("[%s] %s %s", item.Op, item.Kind, item.Key))
    }
}
```

## Continuous (production)

Wire the drift detector via Connector.Config so a background
goroutine pings Stripe every 15 minutes:

```go
DriftDetector: &connector.DriftDetectorConfig{
    Interval: 15 * time.Minute,
    OnReport: func(r catalog.DriftReport) {
        if r.HasDrift {
            pagerduty.Trigger("stripe catalog drift", r.FormatHuman())
        }
    },
},
```

Run on exactly ONE replica (otherwise N replicas all ping Stripe).
Gate by env var:

```go
if os.Getenv("DRIFT_DETECTOR_LEADER") == "true" {
    cfg.DriftDetector = ...
}
```

## Reading the report

```go
report.Items // []PlanItem — each is a Create / Update / Replace / Archive / Drift / Unarchive
report.Namespace // your spec's namespace
report.CheckedAt // when the check ran
report.HasDrift  // true iff Items is non-empty
```

`PlanItem.Op` values:

| Op | Meaning |
|---|---|
| `OpCreate` | Spec has it; Stripe doesn't. Apply will CREATE. |
| `OpUpdate` | Spec + Stripe both have it; fields differ. Apply will UPDATE. |
| `OpReplace` | Stripe has a price with a non-updatable field changed (currency, type). Apply will archive old + create new. |
| `OpArchive` | Stripe has it; spec doesn't. Apply will archive. |
| `OpUnarchive` | Spec has it; Stripe has it archived. Apply will unarchive. |
| `OpDrift` | INFORMATIONAL — fields differ but Stripe API doesn't allow updating. No action; documents the divergence. |

Apps that want CI gating should filter out `OpDrift` items (those are
expected for tax codes, currency on existing prices, etc.).

## Auto-remediation

`DriftReport.Apply(ctx, backend)` runs the items through the same
sync logic as `--apply`:

```go
if report.HasDrift {
    fresh, _ := catalog.CheckDrift(ctx, backend, spec)  // re-check first
    if err := fresh.Apply(ctx, backend); err != nil { ... }
}
```

Re-check before applying because the report is a frozen snapshot.

## What it doesn't catch

- Stripe-side soft data: customer counts, MRR, etc. The drift check
  is about the **catalog** (products, prices, coupons, meters).
- Stripe Tax registrations / tax-rate changes. Those live in a
  separate Stripe configuration the lib doesn't sync.
- Webhook endpoint configuration. Drift here would also affect events
  not arriving, which has its own monitoring.
