# Dunning + grace periods

When a recurring payment fails, Stripe doesn't immediately cancel
the subscription. Instead it retries (Smart Retries, default 4
attempts over ~3 weeks). During this **grace period** the customer
keeps access; the app shows a "payment failed, update your card"
banner.

The library exposes this state via the subscription mirror + the
plans subsystem so apps don't write the dunning logic themselves.

## Lifecycle

1. **`invoice.paid` succeeds** → sub remains `active`. Nothing to do.
2. **`invoice.payment_failed`** → sub transitions to `past_due`.
   - `OnInvoicePaymentFailed` handler fires (apps wire custom UI/email here).
   - `Subscription.Status == past_due`, `IsAccessGranting() == true`.
   - `LatestInvoice.AttemptCount == 1`, `LatestInvoice.NextPaymentAttempt` is set.
3. **Stripe retries** (Smart Retries) → if any retry succeeds, sub returns to `active`.
   - On success: `invoice.paid` fires; sub goes `active`.
   - On retry attempt: `LatestInvoice.AttemptCount` increments; `NextPaymentAttempt` updates.
4. **All retries exhausted** → sub transitions to `unpaid` or `canceled`
   (depends on Stripe Dashboard → Subscription settings → "Subscription
   payment failure" policy).
   - `IsAccessGranting() == false` → access gate denies new requests.

## Reading dunning state

```go
sub, found, _ := conn.Subscriptions.Repo().GetByStripeID(ctx, "sub_…")
if !found || !sub.Status.IsAccessGranting() {
    return ErrNoAccess
}
if sub.InGracePeriod() {
    showDunningBanner(sub)
}
```

Or via `Plans`:

```go
snap, _ := conn.Plans.SnapshotFor(ctx, subject)
if snap.InGracePeriod() {
    nextAt := snap.NextPaymentRetryAt()
    // render "Payment failed; next retry: <time>"
}
```

## Per-field API

| Field / method | What it returns |
|---|---|
| `Subscription.InGracePeriod()` | true iff Status==past_due |
| `Subscription.NextPaymentRetryAt()` | `*time.Time` of next Stripe retry, nil otherwise |
| `Subscription.SmartRetryAttemptCount()` | how many attempts so far |
| `Subscription.LatestInvoice.Status` | "draft" / "open" / "paid" / "void" / "uncollectible" |
| `Snapshot.InGracePeriod()` | OR across all active subs |
| `Snapshot.NextPaymentRetryAt()` | earliest retry across all subs |

`Subscription.LatestInvoice` is populated when Stripe sends the
event with `latest_invoice` expanded (typical for
`customer.subscription.updated` triggered by an invoice transition).
When not expanded, `LatestInvoice` is nil and the helpers return
their zero values gracefully.

## Recommended app behaviour

| Status | Access? | UI |
|---|---|---|
| `active` | ✅ | normal |
| `trialing` | ✅ | "Trial ends in X days" |
| `past_due` | ✅ (grace period) | yellow banner: "Payment failed — please update card" |
| `unpaid` | ❌ | red banner: "Your subscription is paused — please update card to restore access" |
| `canceled` | ❌ | "Subscribe to access" |
| `incomplete` | ❌ | "Complete your subscription setup" |
| `incomplete_expired` | ❌ | same — but Stripe gave up on the setup |
| `paused` | ❌ | "Subscription is paused" |

The library's `IsAccessGranting()` follows this table.

## Configuring the dunning policy in Stripe

The lib doesn't override Stripe's retry schedule (it's set in
Dashboard → Settings → Subscriptions → "Smart Retries" + "Manage
failed payments"). Recommended defaults:

- **Smart Retries: ON** (4 attempts over 3 weeks)
- **Subscription payment failure: cancel** (after retries fail, cancel)
- **Send Stripe-native emails: ON** unless your app sends its own dunning emails
  (don't double-up).

## Auto-cancel on failed refund

Separate from dunning: when a REFUND fails (e.g. original card was
closed), apps often want to cancel the associated subscription. Wire:

```go
cfg := connector.Config{
    AutoCancelSubscriptionOnFailedRefund: true,
    PaymentIntentToSubscriptionResolver: func(ctx context.Context, piID string) (string, error) {
        // App-supplied: lookup your own PI→sub_id table.
        return db.LookupSubID(ctx, piID)
    },
}
```

Stripe doesn't carry the PI→subscription link on refund events, so
the resolver is required. Apps typically populate the lookup table
at `checkout.session.completed` time when both ids are available.

## Why the library doesn't auto-cancel on dunning failure

That decision (cancel vs leave-as-unpaid) is set in Stripe Dashboard,
not in the library. Apps configure it once at the account level; the
library mirrors whatever Stripe does. Override only if you want
non-default behavior — and then handle the transition via `OnSubscriptionUpdated`.
