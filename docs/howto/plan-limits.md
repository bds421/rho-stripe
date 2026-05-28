# Plan limits + feature flags

Every SaaS app re-implements "what can this user do based on their
plan?" Apps usually end up with scattered code:
- A `is_pro_plan(user)` function in 5 places
- Hardcoded "Free tier = 1 seat, Pro = 10, Enterprise = unlimited"
- A separate "is the customer's payment late?" check
- A separate "how many credits do they have left this month?" lookup

`conn.Plans` consolidates all of that.

## The convention

Declare your limits + features in catalog Product metadata, using
two reserved prefixes:

```go
"pro": {
    Name:        "Pro Plan",
    TaxCategory: catalog.TaxCategorySaaSBusiness,
    Metadata: map[string]string{
        // Numeric caps — apps read with snap.IntLimit(name)
        "limit.max_seats":          "50",
        "limit.max_api_calls_mo":   "500000",
        "limit.included_storage_gb":"500",

        // Boolean feature flags — apps read with snap.HasFeature(name)
        "feature.sso":              "true",
        "feature.audit_log":        "true",
        "feature.dedicated_csm":    "false",
    },
    // ...
}
```

Use the literal string `"unlimited"` for caps that have no limit:

```go
"enterprise": {
    Metadata: map[string]string{
        "limit.max_seats":         "unlimited",
        "limit.max_api_calls_mo":  "unlimited",
    },
},
```

`snap.IntLimit("max_seats")` returns `(math.MaxInt64, true)` for
unlimited — apps can write `currentSeats >= limit` uniformly without
a special branch.

## Reading the snapshot

```go
snap, err := conn.Plans.SnapshotFor(ctx, "org_acme")
if err != nil { return err }

// Access gate: is the customer's subscription in an access-granting state?
if !snap.IsActive() {
    return ErrSubscriptionInactive
}

// Feature gate: is SSO enabled on their plan?
if !snap.HasFeature("sso") {
    return ErrUpgradeRequiredForSSO
}

// Numeric limit: can they add another seat?
maxSeats, hasLimit := snap.IntLimit("max_seats")
if hasLimit && currentSeatCount >= maxSeats {
    return ErrSeatLimitReached
}

// Credit-style limit: do they have API calls left this cycle?
remaining := snap.CreditsRemaining("api_calls")
if remaining < 1 {
    return ErrQuotaExceeded
}
```

## What about included resets per cycle?

Use `RecurringGrant` in the catalog — the library auto-resets the
credits each billing cycle via the `invoice.paid` webhook:

```go
"pro": {
    Name:        "Pro Plan",
    TaxCategory: catalog.TaxCategorySaaSBusiness,
    RecurringGrant: &catalog.RecurringGrant{
        Bucket:             "api_calls",
        Amount:             500000,
        ValidDaysFromGrant: 31, // expire at next cycle so unused don't stack
    },
    Metadata: map[string]string{
        "limit.max_api_calls_mo": "500000", // documentation; the grant does the work
    },
    // ...
}
```

Apps then deduct per use:

```go
err := conn.Credits.Deduct(ctx, subject, "api_calls", 1, requestID)
if errors.Is(err, credits.ErrInsufficientCredit) {
    // The customer exhausted this cycle's quota — block or charge overage
}
```

The reset happens automatically when Stripe fires `invoice.paid` at
the start of the next cycle; the library applies the recurring grant.

## Aggregating across multiple subscriptions

B2B customers often have base + addon subscriptions. `Snapshot`
aggregates:

| Field | Aggregation |
|---|---|
| `IntLimit(name)` | MAX across all active plans (highest tier wins) |
| `HasFeature(name)` | OR (any plan enables it → access granted) |
| `CreditsRemaining(bucket)` | SUM of grants in that bucket |
| `IsActive()` | OR (any access-granting sub → access) |
| `InGracePeriod()` | OR (any sub past_due → show banner) |

Example: customer has `pro` ($99/mo) + `addon_sso` ($20/mo). Combined
snapshot has SSO enabled (from addon) AND max_seats from Pro.

## Payment-delayed (`past_due`) handling

"How does the app handle delayed payments without the sub being
cancelled?" — this is exactly what `InGracePeriod` is for:

```go
if snap.InGracePeriod() {
    banner := "Your payment failed — we'll retry; please update your card"
    if at := snap.NextPaymentRetryAt(); at != nil {
        banner += " Next retry: " + at.Format("Jan 2, 15:04")
    }
    showBanner(banner)
}
```

Stripe's Smart Retries do up to 4 attempts over ~3 weeks before
giving up. During that window:

- `Status == past_due` → `IsAccessGranting()` returns true → customer keeps access
- `InGracePeriod()` returns true → app shows the banner
- `NextPaymentRetryAt()` tells the app when Stripe will try next
- `SmartRetryAttemptCount()` tells the app how many attempts so far

When Stripe finally gives up, the subscription transitions to `unpaid`
or `canceled` — at that point `IsAccessGranting()` returns false and
your access gate denies new requests automatically.

## What the app still needs to handle

The library deliberately doesn't:

- **Send dunning emails** — Stripe Dashboard's "Smart Emails" do this;
  apps that want custom messaging wire `OnInvoicePaymentFailed`.
- **Decide WHAT to do at limit** — `IntLimit` says the cap; the app
  picks the action (block, queue, charge overage, upsell modal).
- **Render the upgrade UI** — frontend concern.
- **Choose dunning policy** — Stripe Smart Retries handle the schedule;
  apps that want non-default retry behavior configure it in Stripe
  Dashboard or per-subscription via `default_payment_method` overrides.

## Putting it together: typical access middleware

```go
func RequireActivePlan(snap plans.Snapshot, feature string) error {
    if !snap.IsActive() {
        return ErrNotSubscribed
    }
    if feature != "" && !snap.HasFeature(feature) {
        return ErrFeatureNotInPlan
    }
    return nil
}

func GateAPICall(snap plans.Snapshot, subject SubjectID) error {
    if err := RequireActivePlan(snap, ""); err != nil { return err }
    remaining := snap.CreditsRemaining("api_calls")
    if remaining < 1 {
        return ErrQuotaExceeded
    }
    return nil
}
```

That's ~10 lines that would otherwise be re-implemented in every app.
