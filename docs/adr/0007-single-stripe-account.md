# ADR-0007: Single Stripe account for all apps, with namespaced catalog keys

- **Status:** Accepted
- **Date:** 2026-05-26
- **Deciders:** Markus

## Context

The `rho-stripe` library is shared across multiple apps operated by a single Austrian business (one legal entity). We need to decide whether all apps share one Stripe account, each app has its own Stripe account, or we operate a Stripe Connect platform.

This decision cascades into:
- How the product catalog is namespaced (and how `lookup_key` collisions are prevented).
- Whether customers can be deduplicated across apps.
- Webhook routing and `EventRepo` dedup scope.
- Tax registration and Stripe Tax configuration.
- Operational overhead (number of API keys, webhook secrets, dashboards, bank-account hookups).

## Decision

**One Stripe account for all apps.** All products, prices, customers, subscriptions, and invoices live under a single Stripe account corresponding to the single legal entity.

Catalog keys are **namespaced by app** using a stable prefix:

```
app1.pro_plan.monthly
app1.pro_plan.yearly
app2.api_credits_1000
app2.ai_credits_5000
```

The namespace is the app's identifier (chosen by the integrator) and is enforced by the `catalog` package at sync time — collisions across apps fail loudly during the sync CLI run, before anything reaches Stripe.

## Options considered

### Option A: Single Stripe account, namespaced catalog (chosen)

- ✅ Customer dedup across apps is possible (one business using two apps = one Stripe Customer, one saved card, one billing relationship). Whether this materializes is unknown today; preserving the option costs nothing.
- ✅ Unified reporting in one Stripe dashboard — total MRR, total volume, cash position visible without external consolidation.
- ✅ Single bank-account hookup, single tax registration, single set of webhook secrets to manage.
- ✅ Matches the legal reality: one legal entity, one Stripe account. No legal restructuring required.
- ⚠️ Blast radius: a buggy webhook handler in one app could in principle touch another app's billing state. Mitigated by the per-app `EventRepo` (see [[adr-0006]] when written) and by app-level idempotency keys.
- ⚠️ Brand confusion on credit-card statements: customers using multiple apps see one merchant name on their card statement by default. Mitigatable later via `statement_descriptor_suffix` per charge (deferred — not a phase-0 concern).

### Option B: One Stripe account per app

- ❌ **Not practical for a single legal entity.** Stripe accounts are tied to legal entities; duplicate accounts for the same entity get closed. Would require establishing separate subsidiary companies (GmbHs etc.) per app — wildly out of scope for what is essentially a code-organization concern.
- Would only be appropriate for a holding-company structure with genuine subsidiary entities per app, which is not the current or planned business structure.

### Option C: Stripe Connect (platform + connected accounts)

- ❌ Connect is designed for marketplaces where the platform takes a cut from independent sellers (Shopify, Lyft, etc.). It introduces a fundamentally different account model, onboarding flows for "merchants," dashboard restrictions, and additional fee structures.
- ❌ Massively over-spec for the actual problem (one business selling multiple products).
- Would only become relevant if the apps themselves became multi-tenant platforms with third-party sellers, which is not the current direction.

## Consequences

### Positive

- The `catalog` package's only job for namespace safety is enforcing prefix uniqueness at sync time — a single check.
- Apps wire in their namespace once in their `catalog` declaration; everything else (checkout, webhooks, credits) routes through it automatically.
- Operational simplicity: one API key per environment (test/prod), one webhook secret, one dashboard.
- Customer-side: a Stripe Customer record can be reused across apps if cross-app linking is ever wired up (deferred — phase 1+ concern, only relevant if app-side `CustomerRepo` implementations choose to look up across apps).

### Negative / accepted risks

- Cross-app billing data isolation depends on app-side discipline. The lib enforces per-app event dedup and per-app catalog namespacing, but does not (cannot) prevent an app from querying or mutating another app's billing state if its repos are pointed at shared data. Each app's `CustomerRepo`/`SubscriptionRepo` must scope to its own customers.
- Selling or spinning off one app later means extracting its slice of billing data from a shared account — non-trivial migration. Accepted because it's a low-probability scenario and the cost of preventing it (per-app legal entities) is much higher than the cost of dealing with it if it ever happens.

### Lib API implications

- `connector.Config` requires an `AppNamespace` string. Sync, checkout, and credit operations automatically scope to this namespace.
- The catalog declaration does NOT include the namespace prefix in keys — the integrator writes `"pro_plan.monthly"` and the lib prepends `app1.` at sync time. This keeps app catalogs portable and readable.
- Webhook handlers receive a `WebhookContext` that includes the resolved app namespace (parsed from the event's referenced product/price), so handlers can ignore events meant for other apps.

### Multi-entity escape hatch (no code required)

If a subsidiary legal entity is ever established (e.g. a separate GmbH for a specific product line), the design accommodates it without changes: that subsidiary gets its own Stripe account, and the apps under it integrate the lib with a different `SecretKey` and `AppNamespace`. The lib itself is unaware of legal-entity structure — each integration is scoped to one Stripe account, period. Multi-entity is simply "N independent integrations of the same lib."

## Out of scope (defer to other ADRs)

- How `EventRepo` dedup scope works (per-app vs shared) → [[adr-0006]].
- Currency and tax configuration → [[adr-0009]].
- Customer/identity model (is "customer" always an org?) → [[adr-0002]].
- Statement-descriptor branding per charge → not an ADR; lib will support it via a per-checkout option in a later phase.
