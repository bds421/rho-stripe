# Free trials

"14 days free, then €29/month."

## Checkout (most common)

```go
sess, err := conn.Checkout.CreateSession(ctx, checkout.Input{
    SubjectID:    "org_acme",
    LineItems:  []checkout.LineItem{{PriceKey: "pro.monthly_eur"}},
    SuccessURL: "https://app.example.com/welcome",
    CancelURL:  "https://app.example.com/pricing",
    TrialDays:  14,
})
```

After 14 days Stripe auto-charges the captured card and transitions
the subscription from `trialing` → `active`. The mirror updates via
`customer.subscription.updated`.

## "No credit card required" trials

```go
noCard := false
sess, err := conn.Checkout.CreateSession(ctx, checkout.Input{
    // ... as above
    TrialDays: 14,
    Defaults: checkout.SessionDefaults{
        RequirePaymentMethodForTrial: &noCard,
    },
})
```

Stripe collects the card BEFORE the first charge (3 days before trial
end) instead of upfront. Trade-off: higher conversion at signup, more
"failed payment" emails to handle.

## Heads-up before charging

Wire `OnSubscriptionTrialWillEnd` — Stripe sends this 3 days before
the trial-end charge so you can email "your trial ends in 3 days":

```go
Handlers: webhooks.Handlers{
    OnSubscriptionTrialWillEnd: func(ctx context.Context, evt webhooks.Event) error {
        return mailer.SendTrialEndingSoon(ctx, evt)
    },
},
```

## Direct (invoiced) subscriptions

For sales-led B2B (Pro tier, send-invoice instead of card):

```go
sub, err := conn.Subscriptions.CreateInvoiced(ctx, subscriptions.InvoicedInput{
    StripeCustomerID: cus_id,
    PriceKey:         "enterprise.annual_eur",
    DueIn:            30 * 24 * time.Hour,
    TrialDays:        30,
})
```

## Access checks during trial

`subscriptions.Status.IsAccessGranting()` already returns `true` for
`trialing`. App code doesn't branch:

```go
sub, _, _ := conn.Subscriptions.Repo().GetByStripeID(ctx, id)
if sub.Status.IsAccessGranting() {
    // Customer has access — trial or active, same code path.
}
```

## Trial → cancel-before-charge UX

Customer wants to cancel during the trial. Use `CancelAtPeriodEnd`:

```go
err := conn.Subscriptions.CancelAtPeriodEnd(ctx, subID)
// Customer keeps access until trial ends, then sub silently cancels —
// no charge.
```

## Caveats

- **Hard cap: 730 days.** Stripe rejects longer trials. The lib
  validates and returns `checkout.ErrTrialDaysOutOfRange` for
  invalid values.
- **Trials are subscription-only.** Setting `TrialDays` on a one-time
  (payment-mode) session returns `ErrTrialOnPayment`.
- **Trial mid-cycle isn't supported.** If a customer is already on a
  paid subscription you can't "give them another trial" — start a
  new sub with a trial instead.
