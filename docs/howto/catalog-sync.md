# Sync the catalog to Stripe

The lib is declarative: you describe products/prices/coupons in Go,
and `sync --apply` makes Stripe match your declaration.

## 1. Declare

```go
var spec = catalog.MustSpec(catalog.Spec{
    Namespace: "myapp",
    Products: map[string]catalog.Product{
        "pro": {
            Name:        "Pro",
            TaxCategory: catalog.TaxCategorySaaSBusiness,
            Prices: map[string]catalog.Price{
                "monthly_eur": {Amount: 4900, Currency: "eur",
                    Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
                "yearly_eur":  {Amount: 49000, Currency: "eur",
                    Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalYear},
            },
        },
    },
})

func main() { cli.Main(spec) }
```

## 2. Preview

```bash
go run ./cmd/sync diff
```

Outputs every Create/Update/Replace/Archive the next `--apply` would do.

## 3. Apply

```bash
go run ./cmd/sync sync --apply
```

Idempotent: a second `--apply` with no spec changes prints `applied.`
with zero Stripe writes.

## 4. Detect drift

Someone edited a product in the dashboard? Run:

```bash
go run ./cmd/sync drift-check        # exits 1 if drift found
go run ./cmd/sync drift-check --apply  # auto-remediate (re-checks first)
```

Or wire continuous detection into your webhook server:

```go
DriftDetector: &connector.DriftDetectorConfig{
    Interval: 15 * time.Minute,
    OnReport: func(r catalog.DriftReport) {
        if r.HasDrift {
            slack.Notify("stripe drift", r.FormatHuman())
        }
    },
},
```

## Notes

- **Lookup keys are stable** — `monthly_eur` is the contract. Renaming it generates a new Stripe Price (REPLACE op). The old one is archived; subscriptions stay on it until they renew, then Migrate them.
- **`TransferLookupKey: true`** on a Price moves the lookup_key from the archived Price to the new one — same-key REPLACE is atomic from the app's perspective.
- **Drift items have categories**: `OpCreate`, `OpUpdate`, `OpReplace`, `OpArchive`, `OpDrift` (read-only flag for fields the lib can't reconcile), `OpUnarchive`.
