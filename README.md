# rho-stripe

**Production Stripe billing for Go.** One library, multiple apps, no
manual dashboard ↔ code drift, no hand-rolled webhook plumbing.

```bash
go get github.com/bds421/rho-stripe@v0.1.0
```

Part of the **rho** ecosystem with
[rho-kit](https://github.com/bds421/rho-kit) (HTTP resilience,
idempotency, advisory locks, observability). Targets Stripe API
`v82 / 2025-08-27.basil`.

---

## Why rho-stripe instead of raw `stripe-go`

`stripe-go` is a thin SDK over Stripe's HTTP API. rho-stripe is the
application layer you'd otherwise write twice:

| Concern | `stripe-go` | rho-stripe |
|---|---|---|
| Sync catalog (products, prices, meters, coupons) from code → Stripe | You write it | `catalog.Diff` + `Apply` + `CheckDrift` |
| Webhook signature, dedup, dispatch, retry, replay, EventLog | You write it | `conn.Webhooks.Handle` + typed `Handlers` |
| Subscription mirror with out-of-order webhook protection | You write it | `subscriptions.Repo` + auto-mirror handler |
| FIFO-by-expiry credit ledger with idempotent deduction | You write it | `conn.Credits.TryDeduct` (atomic, Postgres-backed) |
| "Can this customer do X / what's their quota?" entitlements | You write it | `conn.Plans.SnapshotFor` |
| Idempotency keys derived deterministically from operation intent | You set them by hand | Automatic; `WithIdempotencyKey(ctx, …)` to override |
| Per-app namespacing in a shared Stripe account | You stamp metadata + filter | `AppNamespace` does both |
| B2B Checkout defaults (Stripe Tax, VAT ID, billing address) | Set per session | Baked in |
| GDPR Article 15 export / Article 17 forget | You implement | `conn.Customers.Export` / `Forget` |

rho-stripe still uses `stripe-go` under the hood — `conn.Stripe` is
the escape hatch for anything we don't wrap.

---

## Architecture at a glance

```
    Your Go app
         │
         ▼
   ┌────────────────────────────────────────────────┐
   │                  rho-stripe                    │
   │                                                │
   │  catalog        declare in Go, sync to Stripe  │
   │  checkout       hosted + embedded Payment El.  │
   │  webhooks       verify, dedup, dispatch, queue │
   │  subscriptions  mirror + cancel/migrate/pause  │
   │  credits        FIFO ledger, idempotent debit  │
   │  plans          entitlement Snapshot           │
   │  metering       hot-path record → Stripe push  │
   │  customers      GDPR export / forget, tax IDs  │
   │  invoices       draft/finalize/credit-notes    │
   │  coupons        promo codes from spec          │
   └───────┬─────────────────────────────────┬──────┘
           │                                 │
           ▼                                 ▼
       Stripe API                      Your Postgres
       (v82 / basil)                   (state mirror)
```

You import only what you use — each sub-package has its own constructor.
The `connector.New(...)` facade wires them together for the common case.

---

## What it does

| You declare in Go | Library handles |
|---|---|
| Products, prices, coupons, meters | Sync to Stripe (`diff` / `sync --apply`); detects out-of-band dashboard edits via `drift-check`. Per-app namespacing in shared accounts. |
| `SubjectID` + `LineItems` | Hosted or embedded Checkout with B2B defaults (Stripe Tax, VAT ID collection, billing address). Customer mapping persisted in your DB. |
| Webhook handlers + signing secret | Signature verification (rotation-safe via multi-secret), body-size cap, IP allowlist, dedup via `idempotency.Store`, namespace filtering, sync OR async dispatch with optional per-event give-up policies, EventLog for replay. |
| Subscription mirror repo | State mirroring + access checks (`IsAccessGranting`), cancel/migrate/preview-upgrade/seats/schedules/pause/resume. |
| Credit ledger repo | FIFO-by-expiry deduction, multi-bucket, auto-grant on `checkout.session.completed`, recurring auto-refill on `invoice.paid`. |
| Plan feature flags + limits | `Snapshot.HasFeature("sso")` / `Snapshot.IntLimit("max_seats")` / `Snapshot.CreditsRemaining("docs")` — declared as Product metadata. |
| Usage events | Hot-path record in app DB; aggregated push to Stripe Meters (default hourly). |
| Invoice creation | Draft / finalize / void / credit notes; optional gapless app-side numbering for §11 UStG-style compliance with orphan-recovery hook. |
| GDPR DSARs | `Customers.Export` (Article 15) + `Customers.Forget` (Article 17) with disclosure constant for the response. |

---

## What it isn't

- Not a frontend payment UI. We provide `UIMode=embedded` server-side
  and an HTML+Stripe.js example; the actual UI is yours.
- Not a tax-filing tool. Stripe Tax computes rates; you (or your
  accountant) file.
- Not for non-Go apps.

---

## Quickstart

Production wiring with Postgres-backed durable state (the in-memory
stores are for tests only — they lose dedup history + customer mappings
on restart):

```go
package main

import (
    "context"
    "database/sql"
    "log"
    "net/http"
    "os"

    "github.com/bds421/rho-kit/data/idempotency/pgstore/v2"
    "github.com/bds421/rho-stripe/catalog"
    "github.com/bds421/rho-stripe/checkout"
    "github.com/bds421/rho-stripe/connector"
    pgrepo "github.com/bds421/rho-stripe/repos/postgres"
    "github.com/bds421/rho-stripe/webhooks"
    _ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
    ctx := context.Background()
    db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
    if err != nil { log.Fatal(err) }

    spec := catalog.MustSpec(catalog.Spec{
        Namespace: "myapp",
        Products: map[string]catalog.Product{
            "pro": {
                Name: "Pro", TaxCategory: catalog.TaxCategorySaaSBusiness,
                Prices: map[string]catalog.Price{
                    "monthly_eur": {Amount: 1900, Currency: "eur",
                        Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
                },
            },
        },
    })

    conn, err := connector.New(ctx, connector.Config{
        SecretKey:     os.Getenv("STRIPE_SECRET_KEY"),
        WebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"),
        AppNamespace:  "myapp",
        Catalog:       spec,
        Customers:     pgrepo.NewCustomerRepo(db),
        Events:        pgstore.New(db),
        Subscriptions: pgrepo.NewSubscriptionRepo(db),
        Credits:       pgrepo.NewCreditRepo(db),
        Handlers: webhooks.Handlers{
            OnInvoicePaid: func(_ context.Context, e webhooks.Event) error {
                log.Printf("invoice paid: %s", e.ID)
                return nil
            },
        },
    })
    if err != nil { log.Fatal(err) }
    defer conn.Shutdown(ctx)

    mux := http.NewServeMux()
    mux.HandleFunc("POST /webhook", conn.Webhooks.Handle)
    mux.Handle("GET /healthz", conn.HealthHandler())
    mux.HandleFunc("POST /checkout/{subject}", func(w http.ResponseWriter, r *http.Request) {
        sess, _ := conn.Checkout.CreateSession(r.Context(), checkout.Input{
            SubjectID:  checkout.SubjectID(r.PathValue("subject")),
            LineItems:  []checkout.LineItem{{PriceKey: "pro.monthly_eur"}},
            SuccessURL: "https://example.com/ok", CancelURL: "https://example.com/no",
        })
        _, _ = w.Write([]byte(sess.URL))
    })
    log.Fatal(http.ListenAndServe(":8080", mux))
}
```

Don't have Postgres yet? `examples/quickstart/` runs against in-memory
stores for evaluation — but every store referenced above has both
`pgrepo.New…` and an in-memory test variant. Switch on construction.

---

## The four questions every SaaS app asks

### 1. "Can this customer do X?" — feature flags + numeric limits

Declare features and limits as Product metadata:

```go
"pro": {
    Name: "Pro",
    Metadata: map[string]string{
        "feature.audit_log": "true",
        "feature.sso":       "true",
        "limit.max_seats":   "10",
        "limit.max_api_calls_mo": "50000",
    },
    Prices: map[string]catalog.Price{ /* ... */ },
},
```

Then gate at request time with a single typed snapshot:

```go
snap, _ := conn.Plans.SnapshotFor(ctx, subjectID)

if !snap.IsActive() {
    return ErrNoSubscription
}
if !snap.HasFeature("audit_log") {
    return ErrUpgradeRequired
}
if seats, has := snap.IntLimit("max_seats"); has && currentSeats >= seats {
    return ErrSeatLimitReached
}
```

The snapshot aggregates across every active subscription the subject
holds (base plan + addons), so multi-product upsell models work
out-of-the-box. `HasFeature` returns OR across plans. For limits, two
aggregation modes compose:

- `limit.X` → **MAX across plans** (tier-upgrade semantics: a customer
  on both Pro and Enterprise gets the Enterprise cap, not the sum).
- `limit.X.add` → **SUM across plans** (additive-addon semantics:
  storage packs, seat packs, API-call boosters).

```go
"pro":          { Metadata: {"limit.storage_gb":     "100"} },  // base
"storage_pack": { Metadata: {"limit.storage_gb.add": "50"}  },  // addon
// Customer on Pro + 2 storage packs → IntLimit("storage_gb") = 200.
```

`"unlimited"` is a first-class value on the base (returns
`math.MaxInt64`, saturating any addons).

### 2. "Deduct from monthly included quota" — Pro includes 50 documents/month

Declare an auto-grant on the recurring plan (lib grants on every
`invoice.paid`, expires after 30 days):

```go
"pro": {
    Name:   "Pro",
    Prices: map[string]catalog.Price{ /* ... */ },
    RecurringGrant: &catalog.RecurringGrant{
        Bucket: "documents", Amount: 50, ValidDaysFromGrant: 30,
    },
},
```

Then deduct atomically each time the customer does the metered action:

```go
ok, bal, err := conn.Credits.TryDeduct(ctx, credits.DeductInput{
    SubjectID: subjectID,
    Bucket:    "documents",
    Amount:    1,
    Reason:    "download",
    RequestID: requestID, // any unique-per-request value
})
if err != nil { return err }
if !ok {
    return ErrQuotaExceededUpgradePrompt
}
log.Printf("documents remaining this period: %d", bal.Total)
```

FIFO-by-expiry, idempotent on `(SubjectID, RequestID)` so a retried HTTP
call won't double-debit. Multi-bucket (you can run "documents" and
"ai_credits" side-by-side). Postgres-backed with row-level advisory
locks so concurrent deductions on the same subject serialize correctly.

### 3. "Preview the upgrade cost, then upgrade" — Pro → Enterprise

Show the customer what they'll be charged today (Stripe prorates the
remaining time on Pro):

```go
preview, _ := conn.Subscriptions.PreviewMigrate(ctx, subscriptions.MigrateInput{
    StripeSubID:  sub.StripeID,
    FromPriceKey: "pro.monthly_eur",
    ToPriceKey:   "enterprise.monthly_eur",
    Prorate:      true,
})
// preview.AmountDueNow → "+€X today, then €Y/month"
// preview.ProrationLineItems → human-readable breakdown
```

Commit when they confirm:

```go
err := conn.Subscriptions.Migrate(ctx, subscriptions.MigrateInput{
    StripeSubID:  sub.StripeID,
    FromPriceKey: "pro.monthly_eur",
    ToPriceKey:   "enterprise.monthly_eur",
    Prorate:      true,
})
```

The mirror catches up via the resulting `customer.subscription.updated`
event; the customer's `Snapshot.HasFeature(...)` flips immediately on
the next request.

### 4. "Sign up a new customer / start their subscription"

Hosted Checkout — Stripe handles card collection, 3DS, tax, VAT ID,
the whole flow:

```go
sess, _ := conn.Checkout.CreateSession(ctx, checkout.Input{
    SubjectID:  subjectID, // your stable identifier; we map to Stripe Customer
    LineItems:  []checkout.LineItem{{PriceKey: "pro.monthly_eur"}},
    SuccessURL: "https://app.example.com/welcome",
    CancelURL:  "https://app.example.com/pricing",
})
// Redirect customer to sess.URL.
```

Or embedded Payment Element on your own page (`UIMode:
checkout.UIModeEmbedded` + frontend with Stripe.js — see
[`examples/embedded_frontend`](examples/embedded_frontend) for the full
stack).

---

## CLI

The lib ships an embeddable subcommand surface for sync, drift checks,
checkout, portal sessions, replay, etc. Wire it into your own
`cmd/sync/main.go`:

```go
func main() { cli.Main(billing.Catalog) }
```

Then:

```text
rho-stripe <command> [flags]

  diff                              Print the sync plan (default).
  sync --apply                      Apply the plan to Stripe.
  drift-check [--apply|--no-fail]   Compare Stripe state to spec.
  verify-account                    Hit Stripe Accounts.Retrieve.
  checkout [--embed] <price_key>    Create a Checkout Session.
  portal --customer cus_…           Create a Customer Portal session.
  subs schedule …                   Create a multi-phase SubscriptionSchedule.
  subs seats …                      Get/set/add per-seat quantity.
  invoice draft|finalize|void|…     Manage invoices.
  coupon create|list|deactivate     Manage promo codes.
  webhook replay <event-id>         Replay an event from the EventLog.
  customer export|forget|import     GDPR DSAR + onboarding helpers.
  tax-id add|list|remove            B2B VAT registration on a Stripe Customer.
  completion bash|zsh|fish          Print shell-completion script.
```

`go run ./examples/saas_tiers/main diff` is a good first command.

---

## Examples

Five runnable catalogs covering the most common SaaS shapes:

| Example | Pattern |
|---|---|
| [`examples/quickstart`](examples/quickstart) | Smallest possible end-to-end — in-memory stores, single plan |
| [`examples/saas_tiers`](examples/saas_tiers) | Free / Standard / Pro / Enterprise + monthly+yearly |
| [`examples/credit_topup`](examples/credit_topup) | One-time credit packs + auto-refill subscription |
| [`examples/included_quota`](examples/included_quota) | "60 voice-minutes/month included" + metered overage |
| [`examples/per_seat_metered`](examples/per_seat_metered) | Per-seat subscription + metered API calls |
| [`examples/embedded_frontend`](examples/embedded_frontend) | Full-stack embedded Payment Element (HTML + Stripe.js) |

Each example's `catalog.go` is heavily commented — copy any of them as
a starting point.

---

## Production-ready details

What ships hardened by default (no opt-in flag required):

- **Signature verification** with multi-secret rotation support, body-size cap (1 MiB default), 5-minute clock-skew tolerance.
- **IP allowlist** middleware with auto-refresh from Stripe's published list (optional but one-line to enable).
- **Idempotency keys** derived deterministically from operation intent — retried HTTP calls never double-charge / double-create.
- **Async webhook queue** for high-volume endpoints (Postgres-backed, with sweeper for stuck rows, dead-letter sentinel for permanent failures, configurable per-event retry policy).
- **Out-of-order event protection** on the subscription mirror via per-event watermark.
- **Sync-dispatch timeout** below Stripe's 30s ACK so a stuck handler can't trigger a retry storm.
- **Race-clean** under `go test -race ./...`; `staticcheck` zero warnings.
- **Live-tested** against Stripe test mode (19/19 live tests green; `go test -tags=live_stripe`).
- **GDPR** Article 15 + 17 helpers with a plain-language disclosure constant.
- **Per-app namespace** in shared Stripe accounts — metadata stamping + webhook filter agree on one wire-format key.
- **Health probe** at `/readyz` includes Stripe-reachability + queue-depth.

---

## Documentation

- [`docs/howto/`](docs/howto) — task-oriented guides, one file per topic
- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — system overview + package map
- [`docs/INTEGRATION.md`](docs/INTEGRATION.md) — wiring into an existing app
- [`docs/openapi.yaml`](docs/openapi.yaml) — OpenAPI fragment for HTTP endpoints apps typically expose
- [`docs/adr/`](docs/adr) — Architecture Decision Records (the "why" behind each choice)
- [`CHANGELOG.md`](CHANGELOG.md) — release notes

---

## Testing

```bash
# Core unit tests
go test ./...

# Race detector (recommended for webhook + queue paths)
go test -race ./webhooks/... ./catalog/... ./connector/... ./subscriptions/...

# Postgres reference repos (needs Docker postgres on :5433)
cd repos/postgres && go test -tags=postgres_integration ./...

# Live Stripe verification (needs STRIPE_SECRET_KEY in test mode)
go test -tags=live_stripe -v -run TestLive .
```

---

## License

Apache 2.0 — see [LICENSE](LICENSE).
