# Subscription upgrades (Migrate + PreviewMigrate)

Pattern: customer on Pro Monthly wants to switch to Pro Yearly. Two
steps: preview the proration so they see what they'll be charged,
then migrate.

## Preview

```go
preview, err := conn.Subscriptions.PreviewMigrate(ctx, subscriptions.MigrateInput{
    StripeSubID:  sub.StripeID,
    FromPriceKey: "pro.monthly_eur",
    ToPriceKey:   "pro.yearly_eur",
    Prorate:      true,
})
if err != nil {
    return err
}

// Show in the UI:
//   "You'll be charged €X today, then €Y on Z."
fmt.Printf("Due now: %d %s\n", preview.AmountDueNow, preview.Currency)
for _, line := range preview.ProrationLineItems {
    fmt.Printf("  %s: %d\n", line.Description, line.Amount)
}
```

`AmountDueNow` can be negative — that's a proration credit (the
customer is "owed" credit when downgrading or moving from a higher-
priced cycle).

## Commit

After the user confirms:

```go
err := conn.Subscriptions.Migrate(ctx, subscriptions.MigrateInput{
    StripeSubID:  sub.StripeID,
    FromPriceKey: "pro.monthly_eur",
    ToPriceKey:   "pro.yearly_eur",
    Prorate:      true,
})
```

The mirror updates when the resulting `customer.subscription.updated`
webhook lands. Apps that need immediate read-after-write consistency
should re-fetch via `conn.Subscriptions.Repo().GetByStripeID(ctx, id)`
after a short delay (Stripe usually delivers in <1s).

## When the customer goes Pro → Free

The pattern is identical but you migrate to your free-tier Price (the
€0 recurring price). The customer keeps access until the cycle end,
then transitions cleanly — Stripe handles the proration as a credit
on the next (free) invoice, which is effectively no-op.

## Important caveats

- **Preview is point-in-time.** By the time the user clicks Confirm
  the period boundary may have moved and the amount may shift by a
  cent or two. Acceptable for UX; not acceptable for billing
  reconciliation (use the actual `invoice.created` event for that).
- **The mirror's source-of-truth is webhooks.** Migrate doesn't
  update the mirror directly; that happens when the resulting event
  arrives.
