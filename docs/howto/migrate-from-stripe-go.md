# Migrating from raw stripe-go

This guide is for teams who already use stripe-go directly and are
considering adopting rho-stripe. It's not a one-shot rewrite —
adopt the lib incrementally.

## What you keep

rho-stripe wraps stripe-go; it doesn't replace it. Your
existing stripe-go code keeps working alongside `conn.Stripe`
(the underlying `*client.API` is exposed):

```go
// New library wraps:
conn.Checkout.CreateSession(ctx, …)

// Underlying stripe-go still accessible:
conn.Stripe.Charges.New(…)
```

So you can migrate one subsystem at a time.

## Where the lib pays off most (start here)

### 1. Catalog management — biggest immediate win

**Before:**

```go
// Hardcoded price ID — drift between dashboard + code is painful
const ProMonthlyPriceID = "price_1Nxxxxx"

// Or pulled from env vars at startup:
priceID := os.Getenv("STRIPE_PRO_MONTHLY_PRICE_ID")
```

**After:**

```go
spec := catalog.MustSpec(catalog.Spec{
    Namespace: "myapp",
    Products: map[string]catalog.Product{
        "pro": {
            Name: "Pro", TaxCategory: catalog.TaxCategorySaaSBusiness,
            Prices: map[string]catalog.Price{
                "monthly_eur": {Amount: 4900, Currency: "eur",
                    Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
            },
        },
    },
})
// `sync --apply` reconciles your spec with Stripe; apps reference
// "pro.monthly_eur" not "price_…"
```

Migration step: declare your existing products in a `Spec`, run
`drift-check` to confirm no diff against current Stripe state, then
update calling code to use the catalog-relative key.

### 2. Webhooks — second biggest win

**Before:**

```go
http.HandleFunc("/webhook", func(w http.ResponseWriter, r *http.Request) {
    body, _ := io.ReadAll(r.Body)
    sig := r.Header.Get("Stripe-Signature")
    event, err := webhook.ConstructEvent(body, sig, secret)
    if err != nil { http.Error(w, err.Error(), 400); return }

    // Manual dedup
    if alreadyProcessed(event.ID) { w.WriteHeader(200); return }

    // Manual dispatch
    switch event.Type {
    case "checkout.session.completed":
        // ...
    case "customer.subscription.updated":
        // ...
    }

    markProcessed(event.ID)
    w.WriteHeader(200)
})
```

**After:**

```go
conn, _ := connector.New(ctx, connector.Config{
    SigningSecret: secret,
    Events:        pgstore.New(db),  // shared dedup store
    Handlers: webhooks.Handlers{
        OnCheckoutCompleted: func(ctx context.Context, evt webhooks.Event) error {
            // your business logic
        },
        OnSubscriptionUpdated: func(ctx context.Context, evt webhooks.Event) error {
            // mirror is already updated by the time you see this
        },
    },
})
http.HandleFunc("/webhook", conn.Webhooks.Handle)
```

You get for free: signature verification with rotation support, dedup,
namespace filtering, mirror auto-updates, async dispatch via config.

### 3. Subscription state checks

**Before:**

```go
sub, _ := stripe.Subscriptions.Get(subID, nil)
if sub.Status == "active" || sub.Status == "trialing" { … }
```

**After:**

```go
sub, found, _ := conn.Subscriptions.Repo().GetByStripeID(ctx, subID)
if found && sub.Status.IsAccessGranting() { … }
```

The mirror lives in your DB — no Stripe round-trip on hot paths.

## Where the lib doesn't replace stripe-go

Things you keep doing directly with stripe-go (via `conn.Stripe`):

- Anything Connect-related (the lib explicitly doesn't wrap Connect)
- Stripe Identity flows (out of scope)
- Stripe Issuing (out of scope)
- Anything you've already built that works fine — no reason to migrate just for the sake of it

## Suggested migration order

For a typical SaaS:

1. **Add the lib alongside existing code.** `conn.Stripe` is the
   same `*client.API` you already use; nothing breaks.
2. **Migrate the catalog first.** Declare your existing products in
   a `Spec`, run `drift-check`, ship a sync.
3. **Migrate the webhook handler.** Cleanest big win. Use
   `connector.New` to wire dedup, signature verification, dispatch.
4. **Migrate subscription state checks** to the mirror.
5. **Migrate ad-hoc helpers** (refunds, customer-balance, etc.) as
   you touch them.

Don't try to migrate everything in one PR. The lib is designed for
incremental adoption.

## Common gotchas during migration

- **The webhook handler reads `r.Body`** — make sure no
  body-buffering middleware runs before it. Common with gin/echo
  default middleware stacks. See [troubleshooting.md](troubleshooting.md).
- **Your existing webhooks may not have `app_namespace` metadata** —
  legacy events fall through to `OnOtherEvent` until you backfill.
  Run a one-shot script that pulls historical objects and stamps
  metadata; or accept that pre-migration events bypass the namespace
  filter.
- **Pricing changes ARE Replace operations** — Stripe Prices are
  immutable. Changing Amount in the spec → archive old + create new.
  Test on a sandbox catalog first.

## Things you might miss from raw stripe-go

| Raw stripe-go | rho-stripe |
|---|---|
| Direct access to every Stripe field | `conn.Stripe` escape hatch + projection types for common fields |
| Sync HTTP calls everywhere | Same; the wrapper doesn't add async unless you opt in |
| You own the dispatch logic | Library owns it (typed handlers) — less flexibility, more correctness |
| You wire your own dedup | Required by config; library does it |
| You handle retries | Library auto-retries on 5xx; per-event policies for give-up |

If "I want to handle this myself" is a recurring feeling, that's
fine — the escape hatch is `conn.Stripe.<anything>`. The lib doesn't
force its abstractions on operations you'd rather drive directly.

## Tracking your migration

Use `conn.Plans.SnapshotFor` as your "what can this user do?"
helper from the start. Apps that rely on it from day one don't
end up re-implementing the same logic 5 times across the codebase
— which is the actual long-term win of adopting the lib.
