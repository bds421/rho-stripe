# Architecture

This document describes the high-level structure of `rho-stripe`, the load-bearing patterns that span multiple packages, and the responsibilities of each component. It is the entry point for understanding "how does this fit together."

For *decisions* (why we chose this), see [adr/](adr/).
For *task-oriented usage*, see [howto/](howto/).
For *subsystem details*, read the per-package GoDoc comments —
each package has a doc.go-style header.

## What problem this lib solves

Multiple B2B apps under one business need to sell things — subscriptions, one-time purchases, prepaid credits, metered usage. Each app needs Stripe integration. Without an opinionated shared lib, each app re-implements:

- Catalog management (Products, Prices, Coupons in Stripe).
- Webhook handling (signature verification, idempotency, retries).
- Customer ↔ Stripe Customer resolution.
- Subscription state mirroring.
- Credit ledger semantics (Stripe has no credits primitive).
- Usage tracking and Stripe metering reconciliation.
- Test fixtures and signed event helpers.

Re-implementation is slow, error-prone (signature verification bugs, double-billing on webhook retries), and inconsistent across apps. The lib makes Stripe integration "wire 4 things, write 3 webhook handlers, done."

## System overview

```
┌──────────────────────────────────────────────────────────────────┐
│                         APP LAYER                                │
│  (Each app: business logic, HTTP handlers, its own DB)           │
│                                                                  │
│   ┌─────────────────────────────────────────────────────────┐    │
│   │  app code                                               │    │
│   │   • declares catalog.Spec                               │    │
│   │   • implements connector.Repos against its DB           │    │
│   │   • registers connector.Handlers for events it cares about │  │
│   │   • calls conn.Checkout(...), conn.Credits.Deduct(...)  │    │
│   └─────────────────────────────────────────────────────────┘    │
└───────────────────────────────┬──────────────────────────────────┘
                                │
                                ▼
┌──────────────────────────────────────────────────────────────────┐
│                       CONNECTOR LAYER (this lib)                 │
│                                                                  │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────────┐         │
│  │ catalog  │ │ checkout │ │ webhooks │ │ subscriptions│         │
│  │ + sync   │ │ + portal │ │ dispatch │ │ mirroring    │         │
│  └──────────┘ └──────────┘ └──────────┘ └──────────────┘         │
│                                                                  │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────────┐         │
│  │ credits  │ │ metering │ │ coupons  │ │  invoices    │         │
│  │ ledger   │ │ aggregate│ │ + promo  │ │  (phase 6)   │         │
│  └──────────┘ └──────────┘ └──────────┘ └──────────────┘         │
│                                                                  │
│  ┌─────────────────────────────────────────────────────────┐     │
│  │  Storage interfaces (you implement; lib never touches DB)│    │
│  │  CustomerRepo  SubscriptionRepo  CreditRepo  EventRepo   │    │
│  │  UsageRepo  (and InvoiceNumberRepo in phase 6)           │    │
│  └─────────────────────────────────────────────────────────┘     │
└───────────────────────────────┬──────────────────────────────────┘
                                │
                                ▼
┌──────────────────────────────────────────────────────────────────┐
│                            STRIPE                                │
│  Products, Prices, Customers, Subscriptions, Invoices, Webhooks  │
└──────────────────────────────────────────────────────────────────┘
```

The lib sits between app code and Stripe. App code never imports `stripe-go` directly — all Stripe interactions go through the lib's wrappers.

## Load-bearing patterns (used across multiple packages)

These patterns recur across the codebase. Understanding them once explains decisions in many places.

### 1. Catalog in code; Stripe state is derived

**Where it shows up:** [adr-0001], [adr-0003], `catalog` package, sync CLI.

The catalog (Products, Prices, Coupons, Meters) is declared in Go code. The sync CLI reconciles Stripe to match. Stripe's dashboard becomes a read-only view; manual edits are detected as drift.

This eliminates the "copy `price_xxx` ID from dashboard to env var" loop entirely. Apps reference everything by logical key (`pro_plan.monthly_eur`).

### 2. Storage-agnostic via repo interfaces

**Where it shows up:** [adr-0002], every subsystem with persistence.

The lib never owns a database. Each app implements `CustomerRepo`, `SubscriptionRepo`, `CreditRepo`, `EventRepo`, `UsageRepo` against its own DB. The lib ships reference implementations for Postgres (in `repos/postgres`, a separate Go module per [adr-0008]) but apps can use SQLite or anything else.

Result: the lib's core has zero database dependencies. Tests are fast. Apps with existing schemas integrate without restructuring.

### 3. Abstract `SubjectID` + optional `ActorID`

**Where it shows up:** [adr-0002], every operation that has a "customer."

The lib treats the billing subject as an opaque `SubjectID` string. Apps decide what it means — org/tenant ID for B2B, user ID for B2C, anything else. An optional `ActorID` captures "who clicked" in B2B flows where the user initiating a purchase differs from the org being billed.

Result: one library serves both B2B and B2C apps without forking.

### 4. Namespace-stamped metadata for multi-app event routing

**Where it shows up:** [adr-0007], [adr-0006], `catalog`, `checkout`, `webhooks`.

Multiple apps share one Stripe account ([adr-0007]). Each app gets a `Namespace` string. Every Stripe object the lib creates (Customer, Subscription, Invoice, PaymentIntent, Checkout Session) gets stamped with `metadata.app_namespace: <namespace>`. The webhook dispatcher uses this stamp to auto-filter events — each app only sees events for its own objects.

Result: no per-handler "is this event for me" logic. Sharing one Stripe account doesn't leak events across apps.

### 5. Built-in subsystems run before app handlers

**Where it shows up:** `webhooks`, `subscriptions`, `credits`.

When a webhook arrives, the dispatcher runs built-in subsystem actions BEFORE calling the app's typed handler:

- Subscription state mirrored to `SubscriptionRepo`.
- Credit grants applied (for catalog-declared CreditGrant on one-time prices).
- Customer state mirrored to `CustomerRepo`.

By the time the app's handler runs, its DB already reflects the latest state. Apps never write "first parse the event, then update my DB" boilerplate.

### 6. Idempotency everywhere money moves

**Where it shows up:** `webhooks`, `credits`, `metering`, `checkout`.

Every operation that changes money or quota state is idempotent:

- Webhook events deduped on `event.id` via `EventRepo` UNIQUE constraint.
- Credit deductions deduped on `(subject, request_id)`.
- Usage records deduped on `(subject, metric, request_id)`.
- Stripe API calls use idempotency keys for create operations.
- Reconciliation pushes to Stripe meter use deterministic `identifier` for dedup.

Result: retries are safe. Duplicate webhook delivery doesn't double-charge. Same logical operation invoked twice produces one effect.

### 7. App DB is hot-path truth; Stripe is the cashier; webhooks transition state

**Where it shows up:** `subscriptions`, `credits`, `metering`.

Three different sources of truth for three different questions:

| Question | Source of truth | Latency |
|---|---|---|
| "Is org_acme currently on Pro plan?" | App's `SubscriptionRepo` | Sub-ms |
| "How many AI credits does org_acme have left?" | App's `CreditRepo` | Sub-ms |
| "How many API calls this hour?" | App's `UsageRepo` | Sub-ms |
| "Did the customer's payment succeed?" | Webhook event from Stripe | Async (sub-second to minutes) |
| "What's on the customer's saved card?" | Stripe Customer record | Real API call (rare) |

Apps never call Stripe on the hot path. State changes flow Stripe → webhook → app DB.

### 8. Wrappers, not re-exports of stripe-go types

**Where it shows up:** [adr-0008], every subsystem.

The lib defines its own types (`catalog.Price`, `webhooks.Event`, `subscriptions.Subscription`, etc.) instead of re-exporting `stripe.Price`, `stripe.Event`, etc. Apps interact with lib types.

Costs: more code to write/maintain. Benefits: stripe-go can be upgraded (even across major versions) without breaking apps; the lib's API can include fields that stripe-go doesn't (like `TaxCategory` constants).

Escape hatch: `conn.RawClient()` returns the underlying `stripe-go` client for advanced operations the lib doesn't wrap. Documented as unstable.

## Package map

### Core packages (in the main module)

| Package | Responsibility | Phase |
|---|---|---|
| `connector` | Main facade. `connector.New(...)` constructs a `*Connector` wired to all subsystems. | 0 |
| `catalog` | Catalog spec types, validation, in-memory cache, sync algorithm. | 0 |
| `checkout` | Checkout Sessions, Customer Portal, refunds. | 0 |
| `webhooks` | Signature verification, dedup, dispatch, helpers. | 0 |
| `subscriptions` | Subscription state mirroring, lifecycle operations. | 1 |
| `coupons` | Promo code creation, validation, listing. | 2 |
| `credits` | Credit ledger: grants, deductions, FIFO expiry. | 3 |
| `metering` | Usage recording, Stripe reconciliation. | 5 |
| `invoices` | Invoice creation, send-invoice flow, audit log helper. | 6 (audit log in 0) |
| `payments` | Payment-method presets per intent (cards/SEPA/Apple/Google Pay). | 0 |
| `testing` | InMemoryRepos, WebhookSigner, EventFixtures, FakeConnector. | 0 |
| `internal/*` | Non-exported helpers (HTTP client, raw-body utilities, dispatch plumbing). | 0 |

### Sub-modules (separate go.mod)

| Module | Responsibility |
|---|---|
| `repos/postgres` | Reference repo implementations for PostgreSQL. |
| (future) `repos/sqlite` | Reference repo implementations for SQLite. |

### CLI

| Path | Responsibility |
|---|---|
| `cmd/rho-stripe` | The `rho-stripe` CLI binary. Sync, diff, verify, audit-export, smoke tests. |

## Request flow examples

### Flow A: Subscription purchase

```
1. App: conn.Checkout.CreateSession(subject=org_acme, line_items=["pro_plan.yearly_eur"])
   → Lib resolves price_key → stripe price_id via in-memory cache.
   → CustomerRepo.Get(org_acme) → Customer (or create new Stripe Customer).
   → Calls stripe-go: session = stripe.CheckoutSession.New(...)
   → Returns session URL.

2. App redirects customer to session URL.

3. Customer completes payment on Stripe-hosted page.

4. Stripe → POST /webhooks/stripe (checkout.session.completed)
   → conn.Webhooks.Handle(w, r)
       → Verify signature.
       → EventRepo.InsertEvent → new.
       → Read metadata.app_namespace, matches "app1".
       → Built-in: SubscriptionRepo.Upsert (subscription is now in app DB).
       → App handler: OnSubscriptionCreated runs (e.g. unlock features, send welcome email).
       → EventRepo.MarkProcessed.
       → Return 200.

5. App's success page: customer redirected; queries SubscriptionRepo; sees active subscription; shows success.
```

### Flow B: Credit deduction on API call

```
1. App receives an API call from customer.
2. App: ok, balance, err := conn.Credits.TryDeduct(subject=org_acme, bucket="ai", amount=1, request_id=req_id)
   → CreditRepo: BEGIN, pg_advisory_xact_lock(org_acme).
   → Fetch eligible grants FIFO by expiry.
   → Walk grants, plan deductions.
   → If sufficient: insert deduction, update grants, COMMIT, return (true, balance).
   → If insufficient: ROLLBACK, return (false, balance).
3. App acts on result (serve API call or return 402 Insufficient Credits).
```

### Flow C: Metered usage

```
Hot path:
  1. App: conn.Metering.Record(subject, metric="api_calls", quantity=1, request_id=req_id)
     → UsageRepo: INSERT meter_events.
     → Returns. Sub-ms.

Cold path (hourly cron):
  2. App scheduler: conn.Metering.ReconcileToStripe(period={hour_start, hour_end})
     → For each metric, aggregate UsageRepo per subject for the period.
     → For each (subject, total): stripe.MeterEvent.Create with deterministic identifier.
     → Stripe deduplicates; Stripe will bill at end of cycle based on aggregated meter values.
```

## What's intentionally NOT in the lib

These are out of scope by design, not oversight:

- **Frontend code / payment UI components.** Hosted Checkout + Customer Portal cover phase 0. Embedded Payment Element is phase 1+.
- **Email / notification sending.** Apps use their own transactional email system.
- **Tax filing / accounting integration.** Stripe Tax handles calculation; filing and accounting export is operator/finance responsibility.
- **Marketing/CRM integration.** Stripe → CRM sync is app-level orchestration via webhook handlers.
- **Multi-currency FX strategy** (when to convert, what rates). Stripe handles at settlement; apps that need more control use Stripe Connect (out of scope).
- **PCI-scope handling.** Stripe Checkout keeps the app out of PCI scope; the lib doesn't expose anything that would change this.
- **Fraud detection beyond Stripe Radar.** Stripe Radar runs automatically; the lib surfaces results via webhook events.

## Trust boundaries

| Boundary | Trust model |
|---|---|
| App ↔ Lib | Same process; full trust. Lib is a Go dependency, not a network service. |
| Lib ↔ Stripe API | TLS + API key. Lib uses `STRIPE_SECRET_KEY`; never logs it. |
| Stripe ↔ Lib (webhooks) | HMAC-SHA256 signature verification with shared webhook secret. Replay protection via 5-minute timestamp tolerance. |
| App ↔ App's DB | App's responsibility; lib defines interfaces, app provides implementations with whatever auth/encryption the app uses. |
| App ↔ Customer (Checkout) | Customer redirected to Stripe-hosted page; no customer payment data touches the app or the lib. |

## Configuration

A typical `connector.New(...)` call:

```go
conn, err := connector.New(ctx, connector.Config{
    SecretKey:     os.Getenv("STRIPE_SECRET_KEY"),
    WebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"),
    AppNamespace:  "app1",                        // per [adr-0007]
    Catalog:       myapp.Catalog,                 // per [adr-0001]
    Repos: connector.Repos{
        Customers:     pgCustomerRepo,
        Subscriptions: pgSubRepo,
        Credits:       pgCreditRepo,              // only if app uses credits
        Events:        pgEventRepo,
        Usage:         pgUsageRepo,               // only if app uses metering
    },
    Handlers: connector.Handlers{
        OnSubscriptionCreated:  app.OnSubCreated,
        OnSubscriptionCanceled: app.OnSubCanceled,
        OnInvoicePaid:          app.OnInvoicePaid,
        OnCheckoutCompleted:    app.OnCheckoutCompleted,
    },
    Webhooks: connector.WebhookConfig{
        AlertThreshold: 3,
        MaxAttempts:    10,
    },
    Credits: connector.CreditsConfig{
        RevokeOnRefund: false,
    },
})
```

Goal: ~20 lines of wiring per app. Detailed per-field documentation in [INTEGRATION](INTEGRATION.md).

## Versioning and stability

Per [adr-0008]:

- **Pre-1.0 (v0.x.y):** API can break between minor versions. Use with awareness; pin to specific versions in your go.mod.
- **Post-1.0:** strict semver. Breaking changes require major version bumps.

Each module (core, repos/postgres, etc.) versions independently.

## Where to read next

- **Building an app on this lib?** Read [INTEGRATION](INTEGRATION.md).
- **Common tasks?** Browse [howto/](howto/) for task-oriented recipes.
- **Per-package details?** Read the GoDoc comments in each package's
  `doc.go`-style header.
- **Curious about a decision?** Read the relevant [adr/](adr/).
