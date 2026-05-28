# Subscription mirroring

Stripe subscription state is mirrored to your DB so hot-path access
checks ("is this org on the Pro plan?") never hit Stripe.

## Setup

```go
cfg := connector.Config{
    Subscriptions: pgrepo.NewSubscriptionRepo(db),
    // ...
}
```

The connector auto-wires `subscriptions.ApplyEventToMirror` BEFORE
your `OnSubscriptionCreated`/`Updated`/`Canceled` handlers, so by
the time your handler runs the mirror is fresh.

## Access checks

```go
sub, found, err := conn.Subscriptions.Repo().GetByStripeID(ctx, "sub_…")
if err != nil { return err }
if !found || !sub.Status.IsAccessGranting() {
    return ErrUpgradeRequired
}
```

`IsAccessGranting()` is `true` for: `active`, `trialing`, `past_due`
(grace period). Apps that want stricter checks should branch on
`sub.Status` directly.

## By subject (typical)

```go
subs, _ := conn.Subscriptions.Repo().ListBySubject(ctx, "org_acme")
for _, s := range subs {
    if s.Status.IsAccessGranting() && s.HasPrice("pro_plan.monthly_eur") {
        // user has the Pro plan
    }
}
```

`ListByPriceKey` and `HasActivePrice` are convenience helpers on
`conn.Subscriptions.Operations`.

## Eventual consistency window

The mirror updates when Stripe's webhook arrives — typically <1
second. If your app does "do mutation → immediately read mirror"
you may see stale state.

Two patterns:

1. **Trust the input** for read-after-write in the user's session:
   "You upgraded to Pro" + render the new state from what you just
   submitted, not from the mirror.
2. **Poll briefly** for high-accuracy reads:
   ```go
   for i := 0; i < 10; i++ {
       sub, _, _ := repo.GetByStripeID(ctx, id)
       if sub.StripeUpdatedAt.After(beforeMutation) { break }
       time.Sleep(100 * time.Millisecond)
   }
   ```

## Out-of-order event protection

The lib's `StripeUpdatedAt` watermark rejects events older than what's
already in the mirror (Stripe's at-least-once delivery can re-order
under load). You don't need to write this guard yourself.

## Catching up after webhook outages

If your webhook endpoint was down for an hour:

```go
err := conn.Subscriptions.ReconcileFromStripe(ctx, time.Now().Add(-2*time.Hour), nil)
```

Calls `Backend.ListSince(t)` and upserts every subscription Stripe
shows as created/updated since `t`. Mirror catches up to current
state.
