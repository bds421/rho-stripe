# Multi-phase billing (SubscriptionSchedule)

"3 months at 50% off, then full price thereafter." Stripe's
`SubscriptionSchedule` API; the lib exposes a typed wrapper.

```go
sched, err := conn.Subscriptions.CreateSchedule(ctx, subscriptions.ScheduleInput{
    StripeCustomerID: "cus_…",
    Phases: []subscriptions.SchedulePhase{
        {PriceKey: "pro.monthly_eur", Iterations: 3, CouponKey: "SAVE50"},  // 3 months at 50% off
        {PriceKey: "pro.monthly_eur"},                                       // open-ended tail at full price
    },
    EndBehavior: "release",  // convert to normal sub when phases end (default)
})
// sched is *subscriptions.Schedule; sched.StripeID is the schedule id.
```

## Iterations

`Iterations` counts the price's **natural billing periods**, not months.
- Monthly price + `Iterations: 3` = 3 months
- Yearly price + `Iterations: 3` = 3 years
- Iterations = 0 → run to end of schedule (the final phase typically does this)

## EndBehavior

- `"release"` (default): when the last phase ends, Stripe converts the
  schedule into a plain ongoing subscription. The customer keeps
  paying full price indefinitely (until they cancel).
- `"cancel"`: when the last phase ends, the subscription cancels.
  Used for fixed-term contracts ("12 months, then expires").

## Per-phase coupons

Each `SchedulePhase` can carry its own `CouponKey` (catalog-relative
— the lib namespaces it). This is the cleanest way to express
"introductory pricing":

```go
Phases: []subscriptions.SchedulePhase{
    {PriceKey: "enterprise.annual_eur", Iterations: 1, CouponKey: "FIRST_YEAR_25"},
    {PriceKey: "enterprise.annual_eur"},  // year 2+ at full price
}
```

## StartAt

Default = now. Pass a future timestamp to schedule a start (typical
for "free for 6 months, paid plan starts on 2027-01-01" deals).

```go
ScheduleInput{
    StartAt: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
    Phases:  []…,
}
```

When `StartAt` is zero, the lib sends `start_date=now` (Stripe rejects
schedules with no start date).

## Mirror behavior

The schedule itself is a Stripe object; the lib doesn't mirror schedule
state. The underlying *subscription* (created by Stripe when the
schedule activates) is mirrored normally via the
`customer.subscription.*` event flow.
