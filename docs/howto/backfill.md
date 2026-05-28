# Backfill existing customers

You're adopting rho-stripe but you already have customers in
Stripe — created before you used the lib, or migrated from a previous
billing system. Those customers lack the `app_namespace` metadata
stamp the lib relies on for webhook dispatch and won't show up in
hot-path queries.

`conn.Customers.ImportCustomer` fixes this without duplicating data:

```go
result, err := conn.Customers.ImportCustomer(ctx, customers.ImportCustomerInput{
    SubjectID:             "subject-42",       // your app's stable identity
    StripeCustomerID:      "cus_existing",     // the existing Stripe id
    Namespace:             "myapp",            // must match conn config
    BackfillSubscriptions: true,
})
// result.NamespaceStamped, result.SubscriptionsBackfilled, ...
```

## What it does, in order

1. **Read the Stripe customer's current metadata.**
2. **Decide whether to stamp `app_namespace`:**
   - If unset → stamp it.
   - If set to *this* namespace → no-op.
   - If set to a *different* namespace + `AdoptNamespace=false` →
     return `ErrNamespaceConflict` (caller decides).
   - If set to a *different* namespace + `AdoptNamespace=true` →
     overwrite.
3. **Upsert the local `CustomerRepo`** with the
   `(subject, stripeCustomerID)` mapping.
4. **(Optional) Walk every subscription on the customer**, stamp it
   with the namespace (so webhook dispatch routes its future events),
   and upsert it into the local subscription mirror.

## Bulk migration script

```go
for _, mig := range migrations { // your iterable of (subjectID, stripeCustomerID)
    res, err := conn.Customers.ImportCustomer(ctx, customers.ImportCustomerInput{
        SubjectID:             mig.SubjectID,
        StripeCustomerID:      mig.StripeID,
        Namespace:             "myapp",
        BackfillSubscriptions: true,
    })
    if errors.Is(err, customers.ErrNamespaceConflict) {
        log.Warn("namespace conflict", "subject", mig.SubjectID, "previous", res.PreviousNamespace)
        continue
    }
    if err != nil {
        log.Error("import", "subject", mig.SubjectID, "err", err)
        continue
    }
    log.Info("imported", "subject", mig.SubjectID,
        "subs_backfilled", res.SubscriptionsBackfilled)
}
```

## Idempotency

Every Stripe write inside `ImportCustomer` uses an Idempotency-Key
derived from `(stripeCustomerID, namespace)`. You can safely re-run
the script — already-stamped customers cost one read API call and
zero writes.

## What happens if the script crashes halfway?

The connector treats the namespace stamp as the durable marker. Re-run
the script:

- Customers already stamped this namespace → skipped.
- Customers stamped, but local repo missing the mapping → re-Upsert.
- Customers stamped, subs unbackfilled → re-walked (idempotent).

There's no need for a manual recovery step.

## Why stamp subscriptions too?

The webhook dispatcher filters incoming events by
`event.data.object.metadata.app_namespace`. Subscriptions created
before backfill have no stamp; their future `invoice.paid` /
`customer.subscription.updated` events would be silently dropped
without the subscription-side stamp.

This is a footgun by design from Stripe's metadata model — backfill
removes it from your operational surface.
