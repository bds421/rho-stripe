# GDPR data lifecycle (Export & Forget)

GDPR Article 15 (Subject Access Request) and Article 17 (Right to
Erasure) both require apps to act on *all* the data they hold for a
data subject. For a billing-integrated app, "all the data" spans:

- **Stripe-side** — customer record, subscriptions, invoices, charges,
  tax IDs, payment methods
- **Library-side** — subscription mirror, credit ledger, webhook event
  log
- **App-side** — your domain data (orders, support tickets, etc.)

`conn.Customers` bundles the first two and gives you a callback hook
for the third.

## Export (Article 15)

```go
export, err := conn.Customers.Export(ctx, subjectID)
if err != nil { ... }

// Marshal to JSON and hand to the customer (or a secure download URL).
json.NewEncoder(w).Encode(export)
```

`Export` returns a `customers.Export` struct holding:

- `SubjectID`, `StripeCustomerID`, `GeneratedAt`
- `Stripe.Customer` — Stripe customer projection (email, name, address,
  metadata, balance)
- `Stripe.Subscriptions` — every subscription (active + canceled)
- `Stripe.Invoices` — last 100 invoices with hosted URL + PDF URL
- `Stripe.Charges` — last 100 charges
- `Stripe.TaxIDs` — every tax registration
- `Stripe.PaymentMethods` — card-only (extend as needed)
- `Subscriptions` — local subscription-mirror state
- `Credits` — credit ledger: balances + grants + history
- `App` — whatever your `AppDataExporter` callback returns

Wire the app-side exporter at connector construction:

```go
cfg := connector.Config{
    // ...
    AppDataExporter: func(ctx context.Context, s customers.SubjectID) (map[string]any, error) {
        return map[string]any{
            "orders":         orderRepo.AllForSubject(s),
            "support_tickets": ticketRepo.AllForSubject(s),
            "preferences":     prefsRepo.GetForSubject(s),
        }, nil
    },
}
```

Exports hit Stripe live — they're not cached. Run them on-demand from
your SAR-handling endpoint.

## Forget (Article 17)

```go
report, err := conn.Customers.Forget(ctx, subjectID)
// report.StripeCustomerDeleted, report.SubscriptionsRevoked,
// report.CreditsForgotten, report.AppHookCalled, report.AppHookError
```

What happens, in order:

1. **Cancel active subscriptions** — Stripe rejects `Customer.delete`
   if any subscription is still open.
2. **Revoke credit grants** in the local ledger (so retained usage
   history can't be re-spent).
3. **Stripe `Customer.delete`** — anonymizes the customer record.
   **Stripe retains invoices and charges indefinitely** for
   financial-compliance reasons (SOX, PCI-DSS). This is *legally
   permitted* under GDPR Article 17(3)(b) ("compliance with a legal
   obligation").
4. **App-side erasure** via `AppDataForgetter` callback:

```go
cfg.AppDataForgetter = func(ctx context.Context, s customers.SubjectID) error {
    if err := orderRepo.DeleteForSubject(ctx, s); err != nil { return err }
    if err := prefsRepo.DeleteForSubject(ctx, s); err != nil { return err }
    return nil
}
```

If the app hook fails, Stripe + lib data are *still deleted* — the
report carries the hook error so the caller can retry just the
app-side step.

## Webhook event log retention

The webhooks package's event log retains delivered Stripe events
indefinitely by default. After a `Forget`, run
`conn.Webhooks.PruneOldEvents(ctx, time.Now().Add(-24*time.Hour))`
periodically to drop event payloads that may contain the forgotten
customer's data.

For *targeted* erasure of just the forgotten customer's events, query
your event-log table directly using the metadata `app_subject_id`
field (the lib stamps it on every Stripe object).

## Verification

`TestLive_GDPRExportAndForget` exercises the full cycle against a real
Stripe customer:

- Creates a Stripe Customer
- Exports → asserts customer surfaces in the bundle
- Forgets → asserts Stripe customer marked `Deleted=true`

## Compliance gotchas

- **Stripe never deletes invoices/charges**. If your DPA requires
  hard-delete, you cannot use Stripe. The lib does what's possible
  within Stripe's constraints.
- **`AppDataForgetter` must be idempotent**. A retry must not error
  if records are already gone.
- **Audit log the report**. `customers.ForgetReport` is structured
  for exactly this purpose — persist it as compliance evidence.
