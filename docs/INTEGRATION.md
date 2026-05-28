# Integration Guide

How to integrate `rho-stripe` into a Go app. Read this once when adopting the lib; refer back when wiring new features.

For *why* the lib is shaped this way, see [ARCHITECTURE](ARCHITECTURE.md) and [adr/](adr/).
For *task-oriented usage*, see [howto/](howto/).

## Prerequisites

- A Stripe account (test mode is fine to start).
- Go 1.22+.
- A database for the app's repo implementations (Postgres recommended for phase 0; SQLite or others when adapters exist).
- For Stripe Tax: enabled on the Stripe account (Settings → Tax). Free trial available.

## Phase 0 setup (the minimum viable integration)

### Step 1: Add the dependency

```bash
go get github.com/bds421/rho-stripe@latest
go get github.com/bds421/rho-stripe/repos/postgres@latest   # if using Postgres
```

### Step 2: Declare the catalog

In a dedicated `billing` package in your app:

```go
// billing/catalog.go
package billing

import (
    "github.com/bds421/rho-stripe/catalog"
)

var Catalog = catalog.MustSpec(catalog.Spec{
    Namespace: "myapp",
    Products: map[string]catalog.Product{
        "pro_plan": {
            Name:        "Pro Plan",
            Description: "Full access to pro features",
            TaxCategory: catalog.TaxCategorySaaS,
            Prices: map[string]catalog.Price{
                "monthly_eur": {Amount: 4900, Currency: "eur", Interval: "month"},
                "yearly_eur":  {Amount: 49000, Currency: "eur", Interval: "year"},
                "monthly_usd": {Amount: 5400, Currency: "usd", Interval: "month"},
            },
        },
        "credit_pack_1000_ai": {
            Name:        "1000 AI Credits",
            TaxCategory: catalog.TaxCategorySaaS,
            Prices: map[string]catalog.Price{
                "default": {Amount: 1000, Currency: "eur", Type: catalog.PriceTypeOneTime},
            },
            CreditGrant: &catalog.CreditGrant{
                Bucket: "ai", Amount: 1000, ValidDays: 90,
            },
        },
    },
    Coupons: map[string]catalog.Coupon{
        "SAVE20": {
            PercentOff: 20,
            Duration:   catalog.CouponDurationOnce,
        },
    },
})
```

### Step 3: Implement (or use reference) repos

If using Postgres, the reference impl handles everything:

```go
// billing/repos.go
package billing

import (
    "github.com/bds421/rho-stripe/connector"
    "github.com/bds421/rho-stripe/repos/postgres"
    "github.com/jackc/pgx/v5/pgxpool"
)

func NewRepos(db *pgxpool.Pool) connector.Repos {
    return postgres.NewRepos(db)
}
```

Apply the schema migrations shipped with the reference impl:

```bash
psql -d myapp -f $(go env GOPATH)/pkg/mod/github.com/bds421/rho-stripe/repos/postgres@<version>/schema/all.sql
```

(Apps use their preferred migration tool — goose, atlas, golang-migrate, raw psql — to apply the embedded SQL.)

### Step 4: Wire the connector

```go
// billing/setup.go
package billing

import (
    "context"
    "os"
    "github.com/bds421/rho-stripe/connector"
    "github.com/jackc/pgx/v5/pgxpool"
)

func NewConnector(ctx context.Context, db *pgxpool.Pool) (*connector.Connector, error) {
    return connector.New(ctx, connector.Config{
        SecretKey:     os.Getenv("STRIPE_SECRET_KEY"),
        WebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"),
        AppNamespace:  "myapp",
        Catalog:       Catalog,
        Repos:         NewRepos(db),
        Handlers: connector.Handlers{
            OnCheckoutCompleted:    OnCheckoutCompleted,
            OnSubscriptionCreated:  OnSubscriptionCreated,
            OnSubscriptionCanceled: OnSubscriptionCanceled,
            OnInvoicePaid:          OnInvoicePaid,
            OnPaymentFailed:        OnPaymentFailed,
        },
    })
}
```

`connector.New` eagerly resolves the catalog against Stripe (per [adr-0003]) and fails fast on misconfiguration. If it returns an error at startup, fix the issue (typo in price key, missing sync, wrong API key) before going further.

### Step 5: Run sync

Before the app first starts (and on every deploy where the catalog changed):

```bash
# In CI/CD, before deploying:
go run ./cmd/sync diff                # see the plan
go run ./cmd/sync sync --apply         # apply it
```

Your `cmd/sync/main.go`:

```go
package main

import (
    "github.com/bds421/rho-stripe/cmd/rho-stripe/cli"
    "github.com/me/myapp/billing"
)

func main() {
    cli.Run(billing.Catalog)
}
```

### Step 6: Wire the webhook endpoint

Configure a webhook endpoint in Stripe dashboard:
- URL: `https://yourapp.example.com/webhooks/stripe`
- Events: subscribe to at least `checkout.session.*`, `customer.subscription.*`, `invoice.*`, `payment_intent.*`.
- Copy the signing secret into your `STRIPE_WEBHOOK_SECRET` env var.

In your HTTP server:

```go
// using net/http:
http.HandleFunc("/webhooks/stripe", func(w http.ResponseWriter, r *http.Request) {
    conn.Webhooks.Handle(w, r)
})
```

**Critical**: ensure no middleware reads or parses `r.Body` before `Handle` runs. See the [framework gotchas](#webhook-raw-body-gotchas-per-framework) section below.

### Step 7: Implement your handlers

```go
// billing/handlers.go
package billing

import (
    "context"
    "github.com/bds421/rho-stripe/checkout"
    "github.com/bds421/rho-stripe/subscriptions"
    "github.com/bds421/rho-stripe/invoices"
)

func OnCheckoutCompleted(ctx context.Context, evt checkout.CompletedEvent) error {
    // Subscription state and credits are already updated by built-in subsystems.
    // Your job: app-specific orchestration (welcome email, analytics, etc.).
    return nil
}

func OnSubscriptionCreated(ctx context.Context, evt subscriptions.Event) error {
    // Unlock features for evt.SubjectID; send onboarding email; etc.
    return nil
}

func OnSubscriptionCanceled(ctx context.Context, evt subscriptions.Event) error {
    // Schedule access removal at period end; send cancel-confirmation email.
    return nil
}

func OnInvoicePaid(ctx context.Context, evt invoices.Event) error {
    // Notify finance system; update customer LTV; etc.
    return nil
}

func OnPaymentFailed(ctx context.Context, evt invoices.PaymentFailedEvent) error {
    // Stripe's built-in dunning will retry. Your handler: notify customer/support.
    return nil
}
```

That's the full minimum. ~20 lines of wiring in `setup.go` + your handler bodies.

### Step 8: Create a Checkout Session

When a customer clicks "Buy Pro":

```go
session, err := conn.Checkout.CreateSession(ctx, checkout.Input{
    SubjectID:    checkout.SubjectID(orgID),
    Actor:      checkout.ActorID(userID),     // optional, audit trail
    LineItems:  []checkout.LineItem{{PriceKey: "pro_plan.yearly_eur"}},
    SuccessURL: "https://myapp.example.com/billing/success?session_id={CHECKOUT_SESSION_ID}",
    CancelURL:  "https://myapp.example.com/billing/cancel",
})
http.Redirect(w, r, session.URL, http.StatusSeeOther)
```

The customer goes to Stripe's hosted page, completes payment, returns to your success URL. The `checkout.session.completed` webhook fires and your handler runs.

## The "SubjectID discipline" rule

Pick one meaning for `SubjectID` per app and stick to it (per [adr-0002]). For B2B apps, `SubjectID = OrgID`. For B2C apps, `SubjectID = UserID`. **Do not mix.**

If you mix (sometimes pass org_id, sometimes user_id), you'll get duplicate Stripe Customers and a hard-to-diagnose mess in production. The lib enforces "one SubjectID ↔ one Stripe Customer" but cannot enforce what SubjectID *means* — that's your discipline.

## Webhook raw-body gotchas per framework

Stripe signs the exact bytes sent. If your framework parses or re-serializes the body, signature verification fails. Per-framework fixes:

### net/http

No middleware by default — works out of the box:

```go
http.HandleFunc("/webhooks/stripe", conn.Webhooks.Handle)
```

### chi

Same as net/http unless you've added body-parsing middleware. Don't wrap the webhook handler in any:

```go
r := chi.NewRouter()
r.Post("/webhooks/stripe", conn.Webhooks.Handle)   // no middleware on this route
```

### gin

`c.BindJSON` consumes the body. Use `c.Request` directly:

```go
r.POST("/webhooks/stripe", func(c *gin.Context) {
    conn.Webhooks.Handle(c.Writer, c.Request)
})
```

Do NOT call `c.ShouldBind*` or `c.BindJSON` before `Handle`.

### echo

Similar to gin. Use raw `Request`:

```go
e.POST("/webhooks/stripe", func(c echo.Context) error {
    conn.Webhooks.Handle(c.Response().Writer, c.Request())
    return nil
})
```

### fiber

Different `Request` shape. Use the adapter:

```go
import "github.com/gofiber/fiber/v2/middleware/adaptor"

app.Post("/webhooks/stripe", adaptor.HTTPHandler(http.HandlerFunc(conn.Webhooks.Handle)))
```

### Verify your wiring works

Run the startup self-test once during app init to confirm:

```go
if err := conn.Webhooks.TestSignatureVerification(ctx); err != nil {
    log.Fatalf("webhook signature verification broken: %v", err)
}
```

This constructs a known payload, signs it with your configured secret, and runs verification end-to-end. Catches misconfiguration at boot instead of mid-checkout.

## Adding subscription state queries (phase 1+)

```go
// On the hot path, check if a customer has access to a feature:
subs, err := conn.Subscriptions.ListActive(ctx, subjectID)
hasPro := conn.Subscriptions.HasActivePrice(ctx, subjectID, "pro_plan.*")
```

These hit your DB only. Sub-ms.

## Adding credits (phase 3+)

```go
// Deduct on each AI completion:
ok, balance, err := conn.Credits.TryDeduct(ctx, credits.DeductInput{
    SubjectID:   subjectID,
    Bucket:    "ai",
    Amount:    1,
    Reason:    "ai_completion",
    RequestID: reqID,
})
if !ok {
    return ErrInsufficientCredits
}
```

Idempotent on `RequestID`. Two calls with the same RequestID deduct once.

Schedule the expiry job:

```go
// In a cron-like scheduler
cron.AddJob("0 * * * *", func() {
    n, units, err := conn.Credits.RunExpiry(ctx)
    log.Printf("Expired %d grants (%d units)", n, units)
})
```

## Adding usage-based billing (phase 5+)

Hot path:

```go
err := conn.Metering.Record(ctx, metering.MeterEvent{
    SubjectID:   subjectID,
    Metric:    "api_calls",
    Quantity:  1,
    RequestID: reqID,
})
```

Cold path (scheduled):

```go
cron.AddJob("0 * * * *", func() {
    err := conn.Metering.ReconcileToStripe(ctx, metering.Period{
        Start: lastHour, End: thisHour,
    })
})
```

## Testing your integration

The lib ships `connector/testing` with helpers:

```go
import "github.com/bds421/rho-stripe/testing"

func TestMyOnInvoicePaid(t *testing.T) {
    repos := testing.InMemoryRepos()
    conn, _ := connector.New(ctx, connector.Config{
        // ... using testing.SecretKey, repos, etc.
    })

    signer := testing.NewWebhookSigner("whsec_test_xxx")
    event := testing.EventFixtures.InvoicePaid("org_acme", 4900)
    body, sig := signer.Sign(event)

    req := httptest.NewRequest("POST", "/webhook", bytes.NewReader(body))
    req.Header.Set("Stripe-Signature", sig)
    rec := httptest.NewRecorder()

    conn.Webhooks.Handle(rec, req)

    assert.Equal(t, 200, rec.Code)
    // Assert on side-effects in repos or app state.
}
```

For local development with real Stripe events, use the Stripe CLI:

```bash
stripe listen --forward-to localhost:8080/webhooks/stripe
```

This forwards real test-mode webhook events from Stripe to your localhost.

## Operational checklist for new app deployment

When deploying a new app integration:

1. **Stripe webhook endpoint configured.** URL, event subscriptions, signing secret in env var.
2. **Sync run.** `rho-stripe sync --apply` succeeded for the target environment.
3. **App startup green.** `connector.New` returned without error (eager catalog resolution succeeded, signature verification self-test passed).
4. **Smoke test.** Manually run a test checkout end-to-end. Webhook arrives. Subscription/credits update.
5. **Audit log spot-check.** Run `conn.Invoices.ExportAuditLog` for the current period; verify output format.
6. **Monitoring wired.** Alert on `conn.Webhooks.StuckEvents(ctx, 5)` returning non-empty results.
7. **Scheduled jobs running.** Credit expiry (if used). Usage reconciliation (if used). Old-event pruning.

## Environment variables

| Variable | Purpose | Example |
|---|---|---|
| `STRIPE_SECRET_KEY` | Stripe API secret. Test or live. | `sk_test_...` |
| `STRIPE_WEBHOOK_SECRET` | Signing secret from the webhook endpoint config. | `whsec_...` |

That's it. No other lib-specific env vars in phase 0.

## Common errors and what they mean

| Error | Cause | Fix |
|---|---|---|
| `ErrPriceKeyNotFound` | Catalog key in code that's not in Stripe (sync skipped or typo). | Run `rho-stripe sync --apply`. Check key spelling. |
| `ErrInvalidLineItemCombination` | Mixed recurring and one-time line items in one Checkout. | Split into two checkouts. |
| `ErrSubjectMissing` | `SubjectID` was empty when calling lib API. | Pass a non-empty subject. |
| `signature mismatch (400)` | Webhook secret wrong, OR body was modified by middleware. | Check `STRIPE_WEBHOOK_SECRET`; verify framework's raw-body handling. |
| `event timestamp too old (400)` | Replay protection (>5min skew). | Check server clock; if real replay, manually replay via dashboard. |
| Stripe API: "No such price" | Catalog key resolved to a price that was deleted. | Sync mismatch — re-run sync. |

## Where to get help

- For lib behavior questions: read the relevant [howto/](howto/) recipe or the package's GoDoc.
- For "why does it work this way" questions: read the relevant [adr/](adr/).
- For Stripe API questions: [Stripe docs](https://stripe.com/docs) and [stripe-go reference](https://pkg.go.dev/github.com/stripe/stripe-go).
- For tax/legal questions: ask a tax advisor, not this lib. Stripe Tax gives you the data; filing is yours.

## Migration / upgrade

Pre-1.0 versions can have breaking changes between minor versions. Pin to specific versions in your `go.mod`:

```go
require github.com/bds421/rho-stripe v0.3.0
require github.com/bds421/rho-stripe/repos/postgres v0.1.2
```

Each release's CHANGELOG documents breaking changes and migration steps.

Post-1.0, strict semver applies — minor bumps are safe.
