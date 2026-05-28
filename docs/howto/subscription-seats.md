# Per-seat subscriptions

Per-user/per-seat plans: `€15/seat/month, 5 seats today, 7 tomorrow`.

## Set headcount

```go
err := conn.Subscriptions.SetSeats(ctx, "org_acme", "sub_…", "team.per_seat_eur", 7, true /* prorate */)
```

`SetSeats` finds the matching seat item on the subscription and
adjusts its quantity. Prorate=true charges/credits the difference
for the remainder of the cycle (Stripe default).

## Delta-based changes

For "user joined → +1 seat":

```go
err := conn.Subscriptions.AddSeats(ctx, "org_acme", "sub_…", "team.per_seat_eur", 1, true)
```

For "user left → -1 seat", pass a negative delta. Going to 0 is
rejected — use `CancelNow` to terminate the subscription.

## Read current count

```go
n, err := conn.Subscriptions.SeatCount(ctx, "org_acme", "sub_…", "team.per_seat_eur")
```

## Mirror-miss fallback

`SetSeats` first checks the mirror. If the subscription was just
created and the webhook hasn't landed yet, the lib falls back to
calling Stripe directly (`GetSubscriptionItems`) — no spurious
"subscription not found" errors during the eventual-consistency
window.

The fallback skips for cross-tenant errors: if the mirror knows the
sub belongs to a different subject, the lib refuses with
`ErrSubscriptionNotForSubject` (no Stripe call).

## Catalog declaration

```go
"team": {
    Name:        "Team plan",
    TaxCategory: catalog.TaxCategorySaaSBusiness,
    Prices: map[string]catalog.Price{
        "per_seat_eur": {Amount: 1500, Currency: "eur",
            Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
    },
},
```

App passes `LineItem{PriceKey: "team.per_seat_eur", Quantity: 5}` to
the initial Checkout, then uses `SetSeats`/`AddSeats` for changes.
