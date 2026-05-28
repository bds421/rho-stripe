# Prepaid credit packs + auto-refill

"Buy 1000 credits for €10, use them per API call, auto-refill when low."

## Catalog

Each pack is a one-time-priced Product with a `CreditGrant`. The grant
fires on `checkout.session.completed` — the library auto-wires this.

```go
"pack_1000": {
    Name:        "Credit Pack — 1,000 credits",
    TaxCategory: catalog.TaxCategorySaaSPersonal,
    CreditGrant: &catalog.CreditGrant{
        Bucket:    "api_credits",
        Amount:    1000,
        ValidDays: 0, // never expires (typical for prepaid packs)
    },
    Prices: map[string]catalog.Price{
        "default": {Amount: 1000, Currency: "eur", Type: catalog.PriceTypeOneTime},
    },
},
```

Full example: [`examples/credit_topup`](../../examples/credit_topup).

## Consume

```go
err := conn.Credits.Deduct(ctx, "user_alice", "api_credits", 1, requestID)
if errors.Is(err, credits.ErrInsufficientCredit) {
    return askToTopUp()
}
```

## Auto-refill: app-driven

Two patterns; pick one based on your UX.

### Pattern A: Subscription that grants per cycle

A recurring subscription with `RecurringGrant`:

```go
"auto_refill_monthly": {
    Name:        "Auto-refill 10K credits/month",
    TaxCategory: catalog.TaxCategorySaaSPersonal,
    RecurringGrant: &catalog.RecurringGrant{
        Bucket:             "api_credits",
        Amount:             10000,
        ValidDaysFromGrant: 30, // expire at next cycle so unused don't stack
    },
    Prices: map[string]catalog.Price{
        "monthly_eur": {Amount: 7900, Currency: "eur",
            Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
    },
},
```

The lib auto-applies the grant on every `invoice.paid`. No app code
needed beyond the catalog declaration.

### Pattern B: Threshold-triggered ad-hoc Checkout

App monitors the balance; when it drops below threshold, programmatically
create a Checkout session for a top-up pack:

```go
bal, _ := conn.Credits.Balance(ctx, subject, "api_credits")
if bal.Total < 100 && customerHasOptedIntoAutoRefill(subject) {
    sess, _ := conn.Checkout.CreateSession(ctx, checkout.Input{
        SubjectID:   subject,
        LineItems: []checkout.LineItem{{PriceKey: "pack_1000.default"}},
        // ... apps can pre-fill payment method via Stripe's payment_method
        // saved-on-customer; lib doesn't yet wrap that — use raw Stripe API.
    })
    mailer.SendTopUpEmail(subject, sess.URL)
}
```

## Refund handling

Apps that grant credits on payment should also revoke them on refund.
Set `Config.AutoRevokeCreditsOnRefund: true` to wire this automatically:

```go
cfg := connector.Config{
    AutoRevokeCreditsOnRefund: true,
    // ...
}
```

The lib finds grants whose `SourceRef` matches the refunded
PaymentIntent or charge and revokes them. Idempotent across
re-delivered refund events.

## See also

- [credits-included-quota.md](credits-included-quota.md) — "60 minutes/month included" pattern
- [credits-load.md](credits-load.md) — concurrency + retry guidance
