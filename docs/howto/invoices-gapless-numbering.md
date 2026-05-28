# Gapless app-side invoice numbering

Some jurisdictions (Austrian §11 UStG, German GoBD, Italian fiscal
receipts, etc.) require invoice numbers form a strictly increasing
gapless sequence within an accounting period. Stripe's default
auto-numbering doesn't guarantee gaplessness — drafts that fail
validation consume a number that never appears on a finalized invoice.

The lib provides a `NumberRepo` interface + Postgres implementation:

```go
import pgrepo "github.com/bds421/rho-stripe/repos/postgres"

conn, err := connector.New(ctx, connector.Config{
    InvoiceNumberRepo: pgrepo.NewInvoiceNumberRepo(db),
    InvoiceOrphanHook: func(_ context.Context, info invoices.OrphanInfo) error {
        // Persist the orphan to alert your finance team
        alert.Page("invoice-orphan reason=%s number=%s stripe=%s",
            info.Reason, info.Number, info.StripeInvoiceID)
        return nil
    },
})

inv, err := conn.Invoices.CreateDraft(ctx, invoices.CreateInput{
    Customer:     "cus_xyz",
    LineItems:    []invoices.CreateLineItem{{Amount: 5000, Currency: "eur"}},
    NumberPrefix: "AT-2026-",  // sequence: AT-2026-000001, AT-2026-000002, …
})
```

The flow:

1. **`Next` allocates BEFORE Stripe.** A row is persisted (`state=issued`)
   with the next counter value, returned as the formatted number.
2. **Stripe is called with `NumberOverride=<the number>`.**
3. **On Stripe success → `MarkUsed`.** The row's state becomes `used`
   with the Stripe invoice id recorded.
4. **On Stripe failure → `MarkVoided`.** The row's state becomes
   `voided` with the failure reason. The number is "consumed" but
   never appears on a real invoice — the audit trail shows it was
   intentionally skipped.

## Why this matters under failure

- **Crash between `Next` and Stripe call:** the next request asks for
  the next number, NOT the crashed one. Apps run periodic recovery via
  `OrphanReporter.ListIssuedOlderThan(ctx, time.Now().Add(-5*time.Minute))`
  to find rows stuck in `issued` and decide per-orphan whether to
  `MarkVoided` (Stripe never saw it) or `MarkUsed` (Stripe did create
  it; the failure was in returning the id).
- **`MarkVoided` itself fails:** the row stays in `issued` state. The
  `InvoiceOrphanHook` is called with reason `void_failed_after_stripe_error`
  so apps can alert their ops team. Without the hook, the orphan is
  silent — strongly recommend wiring it when `InvoiceNumberRepo` is set.
- **`MarkUsed` fails after Stripe success:** Stripe HAS the invoice,
  but our ledger doesn't know its number. Hook fires with reason
  `used_failed_after_stripe_success` and the Stripe invoice id so apps
  can reconcile manually.

## Custom formatter

Default formatter: `<prefix><6-digit-counter>` → `AT-2026-000001`.

For different formats:

```go
repo := pgrepo.NewInvoiceNumberRepo(db, pgrepo.WithFormatter(
    func(prefix string, counter int64) string {
        return fmt.Sprintf("%sINV-%04d", prefix, counter)
    },
))
// → "AT-2026-INV-0001"
```

## Multiple sequences

`NumberPrefix` selects the sequence. Common patterns:

- `"AT-2026-"` + `"DE-2026-"` — separate per-jurisdiction sequences (one for each Steuernummer).
- `"2026-"` — single yearly sequence (most common).
- `"2026-Q1-"` + `"2026-Q2-"` — quarterly (rarer, sometimes required by auditors).

Each prefix has its own counter; numbers can't collide across prefixes.

## Verifying gaplessness

```sql
WITH gaps AS (
    SELECT prefix, counter, 
           LAG(counter) OVER (PARTITION BY prefix ORDER BY counter) AS prev
    FROM stripe_connector_invoice_numbers
)
SELECT prefix, prev, counter
  FROM gaps
 WHERE counter - prev > 1;
```

Empty result = sequence is gapless. (`MarkVoided` rows still count —
they have a counter, they're just labeled "voided" with a reason.)
