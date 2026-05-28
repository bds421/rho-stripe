# "60 voice-minutes/month included" pattern

Subscription with a per-cycle quota of something consumable. The
library owns the ledger; your app owns the "is this minute allowed"
decision.

## Catalog declaration

```go
"pro": {
    Name:        "Voice Pro",
    TaxCategory: catalog.TaxCategorySaaSBusiness,
    RecurringGrant: &catalog.RecurringGrant{
        Bucket:             "voice_minutes",
        Amount:             600,
        ValidDaysFromGrant: 31, // unused minutes vanish at next cycle
    },
    Prices: map[string]catalog.Price{
        "monthly_eur": {Amount: 9900, Currency: "eur",
            Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
    },
},
```

The lib auto-grants the bucket on every successful `invoice.paid`
event (via the `ApplyRecurringGrantsFromInvoice` handler the
connector wires for you).

## App consumes minutes

Before serving each minute of transcription:

```go
err := conn.Credits.Deduct(ctx, credits.SubjectID(userOrg), "voice_minutes", 1, requestID)
switch {
case errors.Is(err, credits.ErrInsufficientCredit):
    // Out of quota for this month. Options:
    //   1. Return 402 → frontend shows upgrade modal
    //   2. Fall back to metered overage (see metered-billing.md)
    //   3. Hard-stop (return error to caller)
    return upsell()
case err != nil:
    return err
}
// Proceed with the transcription
```

`requestID` is your idempotency key — Deduct is safe to retry with the
same id. Useful when your worker crashes after Deduct but before
serving the minute.

## Showing balance to the user

```go
bal, err := conn.Credits.Balance(ctx, subject, "voice_minutes")
fmt.Printf("%d minutes remaining (expires %s)", bal.Total, bal.NextExpiry.Format("2006-01-02"))
```

## What the library does NOT do

- **Doesn't enforce limits on its own.** Apps choose when to call
  `Deduct`. The library only decrements + rejects when empty.
- **Doesn't refund credits on subscription cancel** by default. Apps
  that want to refund prorated credits call `Credits.RevokeGrant`
  in their `OnSubscriptionDeleted` handler.

## Multiple buckets

`voice_minutes` is one bucket; `api_calls` could be another. The
RecurringGrant takes a single Bucket; for two distinct allowances on
one subscription, declare two Products that the same checkout adds:

```go
"pro_voice":   { RecurringGrant: {Bucket: "voice_minutes", Amount: 600} },
"pro_api":     { RecurringGrant: {Bucket: "api_calls",      Amount: 50000} },
```

Or store both in one bucket (and let the app interpret 1 unit = 1
minute OR 1 call) if the units are interchangeable for billing.
