# rho-stripe

`github.com/bds421/rho-stripe`

A batteries-included Go library for Stripe integration. One library, shared
across multiple apps, eliminating the manual "create product in dashboard →
copy `price_xxx` → paste in code → forget to update the second app" loop.

Part of the **rho** ecosystem alongside [rho-kit](https://github.com/bds421/rho-kit).
Compose `rho-kit` (HTTP resilience, idempotency, observability) with `rho-stripe`
(Stripe-specific integration) for a complete production billing stack.

**Status:** Pre-release (no tagged version yet).

Live-verified against Stripe test mode (19/19 live tests pass);
Postgres reference repos green; race-clean tests across all subsystems.
The API surface is stable in shape — pre-1.0 we may still rename / move
minor surfaces, but the core (`conn.Catalog`, `conn.Checkout`,
`conn.Webhooks`, `conn.Subscriptions`, `conn.Credits`) is settled.

An open adoption blocker remains: `go.mod` still has `replace`
directives pointing at local `../rho-kit/*`. Until rho-kit is
published to a public module path, the library can be imported only
from a checkout that has rho-kit alongside. This is the work item
before v0.1.0 cut.

## What it does

| You declare in Go | Library handles |
|---|---|
| Products, prices, coupons, meters | Sync to Stripe (`diff` / `sync --apply`); detects out-of-band dashboard edits via `drift-check` |
| `Subject` + `LineItems` | Checkout session (hosted OR embedded Payment Element) with B2B defaults — automatic tax, VAT ID collection, billing address. Per-app namespace metadata stamping. |
| Webhook handlers + a signing secret | Signature verification (rotation-safe), dedup via `idempotency.Store`, namespace filtering, sync OR async dispatch with optional per-event give-up policies, EventLog for replay |
| Subscription mirroring repo | State mirroring + access checks (`IsAccessGranting`), cancel/migrate/preview-upgrade/seats/schedules |
| Credit ledger repo | FIFO-by-expiry deduction, multi-bucket, auto-grant on `checkout.session.completed`, recurring auto-refill on `invoice.paid` |
| Usage events | Hot-path record in app DB; periodic aggregated push to Stripe Meters |
| Invoice creation | Draft / finalize / void / credit notes; optional gapless app-side numbering for §11 UStG-style compliance with orphan-recovery hook |

## What it isn't

- Not a frontend payment UI library. We provide `UIMode=embedded` server-side and an HTML+Stripe.js example; the actual UI is yours.
- Not a tax-filing tool. Stripe Tax computes rates; you (or your accountant) file.
- Not for non-Go apps.

## 30-second quickstart

```go
// main.go
spec := catalog.MustSpec(catalog.Spec{
    Namespace: "myapp",
    Products: map[string]catalog.Product{
        "pro": {Name: "Pro", TaxCategory: catalog.TaxCategorySaaSBusiness,
            Prices: map[string]catalog.Price{
                "monthly": {Amount: 1900, Currency: "eur",
                    Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
            }},
    },
})

conn, _ := connector.New(ctx, connector.Config{
    SecretKey:     os.Getenv("STRIPE_SECRET_KEY"),
    WebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"),
    AppNamespace:  "myapp",
    Catalog:       spec,
    Customers:     checkout.NewMemoryCustomerRepo(),
    Events:        idempotency.NewMemoryStore(),
    Handlers: webhooks.Handlers{
        OnInvoicePaid: func(_ context.Context, e webhooks.Event) error {
            log.Printf("invoice paid: %s", e.ID); return nil
        },
    },
})
defer conn.Shutdown(ctx)

mux := http.NewServeMux()
mux.HandleFunc("POST /webhook", conn.Webhooks.Handle)
mux.Handle("GET /healthz", conn.HealthHandler())
mux.HandleFunc("POST /checkout/{subject}", func(w http.ResponseWriter, r *http.Request) {
    sess, _ := conn.Checkout.CreateSession(r.Context(), checkout.Input{
        SubjectID:    checkout.SubjectID(r.PathValue("subject")),
        LineItems:  []checkout.LineItem{{PriceKey: "pro.monthly"}},
        SuccessURL: "https://example.com/ok", CancelURL: "https://example.com/no",
    })
    fmt.Fprintln(w, sess.URL)
})
http.ListenAndServe(":8080", mux)
```

That's the entire integration — declarative catalog, automatic webhook
verification + dedup + dispatch, hosted Checkout, K8s-ready /healthz.
See [examples/quickstart/](examples/quickstart/) for a runnable version.

## Quickstart (5 minutes)

```bash
# 0. Get the code
git clone https://github.com/bds421/rho-stripe
cd rho-stripe

# 1. Add your Stripe test key
cat > .env <<EOF
STRIPE_SECRET_KEY=sk_test_…
STRIPE_PUBLISHABLE_KEY=pk_test_…
EOF
set -a; source .env; set +a

# 2. Pick an example catalog and sync it to your test account
go run ./examples/saas_tiers/main sync --apply
# (or: credit_topup, included_quota, per_seat_metered)

# 3. Create a checkout session for the standard tier
go run ./examples/saas_tiers/main checkout standard.monthly_eur
# → opens https://checkout.stripe.com/c/cs_test_…
#   Pay with 4242 4242 4242 4242 (any expiry, any CVC)

# 4. Start the webhook server in another terminal
stripe listen --forward-to localhost:8080/webhook  # copy whsec_
STRIPE_WEBHOOK_SECRET=whsec_… go run ./cmd/example-webhook

# 5. The webhook runs handlers async, applies recurring grants,
#    mirrors subscriptions, and surfaces drift every 5 minutes.
```

Or wire into your own app:

```go
import (
    "github.com/bds421/rho-stripe/catalog"
    "github.com/bds421/rho-stripe/connector"
    pgrepo "github.com/bds421/rho-stripe/repos/postgres"
)

spec := catalog.MustSpec(catalog.Spec{
    Namespace: "myapp",
    Products: map[string]catalog.Product{
        "pro": {
            Name:        "Pro",
            TaxCategory: catalog.TaxCategorySaaSBusiness,
            Prices: map[string]catalog.Price{
                "monthly_eur": {Amount: 4900, Currency: "eur",
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
    Events:        idempotency.NewPgStore(db),
    Subscriptions: pgrepo.NewSubscriptionRepo(db),
    Credits:       pgrepo.NewCreditRepo(db),
    Handlers:      myHandlers,

    // Optional production-only:
    AsyncWebhooks:     &connector.AsyncWebhookConfig{Capacity: 256, Workers: 4},
    InvoiceNumberRepo: pgrepo.NewInvoiceNumberRepo(db),
    DriftDetector:     &connector.DriftDetectorConfig{Interval: 15*time.Minute, OnReport: alertSlack},
})
defer conn.Shutdown(ctx)
```

## Examples

Four runnable example catalogs covering the most-common SaaS shapes:

| Example | Pattern | Run |
|---|---|---|
| [`examples/saas_tiers`](examples/saas_tiers) | Free / Standard / Pro / Enterprise + monthly+yearly | `go run ./examples/saas_tiers/main` |
| [`examples/credit_topup`](examples/credit_topup) | One-time credit packs + auto-refill subscription | `go run ./examples/credit_topup/main` |
| [`examples/included_quota`](examples/included_quota) | "60 voice-minutes/month included" + metered overage | `go run ./examples/included_quota/main` |
| [`examples/per_seat_metered`](examples/per_seat_metered) | Per-seat subscription + metered API calls | `go run ./examples/per_seat_metered/main` |
| [`examples/embedded_frontend`](examples/embedded_frontend) | Full-stack embedded Payment Element demo (HTML+Stripe.js) | `go run ./examples/embedded_frontend` |

Each example's `catalog.go` is heavily commented — copy any of them as a starting point for your own catalog.

## Documentation

- **[docs/howto/](docs/howto)** — task-oriented how-tos for each feature (one file per topic)
- **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)** — system overview + package map
- **[docs/INTEGRATION.md](docs/INTEGRATION.md)** — wiring into an existing app
- **[docs/howto/](docs/howto)** — task-oriented guides (catalog sync, webhooks, GDPR, etc.)
- **[docs/openapi.yaml](docs/openapi.yaml)** — OpenAPI fragment for HTTP endpoints apps typically expose
- **[docs/adr/](docs/adr)** — Architecture Decision Records (the "why" behind each choice)
- **[CHANGELOG.md](CHANGELOG.md)** — release notes

## CLI reference

```
rho-stripe <command> [flags]

  diff                         Print the sync plan (default).
  sync --apply                 Apply the plan to Stripe.
  verify-account               Hit Stripe Accounts.Get to confirm the key works.
  checkout [--embed] <key>     Create a Checkout Session.
  portal --customer cus_…      Create a Customer Portal session.
  drift-check [--apply|--no-fail]   Compare Stripe state to spec.
  subs schedule …              Create a multi-phase SubscriptionSchedule.
  subs seats …                 Get/set/add per-seat quantity on a subscription.
  webhook replay <event-id>    Replay an event from the EventLog through the handlers.
  help                         Show this message.
```

## Testing

```bash
# Core unit tests (all packages)
go test ./...

# Race detector (recommended for webhook + queue packages)
go test -race ./webhooks/... ./catalog/... ./connector/... ./subscriptions/...

# Postgres reference repos (needs Docker postgres on :5433)
cd repos/postgres && go test -tags=postgres_integration ./...

# Live Stripe verification (needs STRIPE_SECRET_KEY)
go test -tags=live_stripe -v -run TestLive .
```

## License

Apache 2.0 — see [LICENSE](LICENSE).
