# How-to guides

Task-oriented recipes. Each file is self-contained.

## Catalog

- **[catalog-sync.md](catalog-sync.md)** — declare products in Go, push to Stripe, detect dashboard drift.
- **[catalog-multi-currency.md](catalog-multi-currency.md)** — one product, prices in EUR/USD/GBP.
- **[tax-codes.md](tax-codes.md)** — picking the right `TaxCategory`.
- **[drift-detection.md](drift-detection.md)** — alerting on out-of-band edits.

## Checkout

- **[checkout-hosted.md](checkout-hosted.md)** — Stripe's hosted page (default).
- **[checkout-embedded.md](checkout-embedded.md)** — embed the Payment Element inline.
- **[checkout-b2c.md](checkout-b2c.md)** — disabling B2B defaults for B2C apps.
- **[customer-portal.md](customer-portal.md)** — letting customers self-manage.
- **[trials.md](trials.md)** — free-trial sessions.

## Webhooks

- **[webhooks-basic.md](webhooks-basic.md)** — minimum viable wiring.
- **[webhooks-async-dispatch.md](webhooks-async-dispatch.md)** — async dispatch.
- **[webhooks-secret-rotation.md](webhooks-secret-rotation.md)** — rotating signing secrets.
- **[webhooks-replay.md](webhooks-replay.md)** — replaying events after a handler fix.

## Subscriptions

- **[subscription-mirror.md](subscription-mirror.md)** — hot-path access checks.
- **[subscription-upgrade.md](subscription-upgrade.md)** — Migrate + PreviewMigrate.
- **[subscription-schedules.md](subscription-schedules.md)** — multi-phase billing.
- **[subscription-seats.md](subscription-seats.md)** — per-seat plans.
- **[subscription-pause-resume.md](subscription-pause-resume.md)** — pause billing without canceling.

## Credits & metered billing

- **[credits-included-quota.md](credits-included-quota.md)** — "60 minutes/month included" pattern.
- **[credits-prepaid-packs.md](credits-prepaid-packs.md)** — one-time top-ups + auto-refill.
- **[credits-load.md](credits-load.md)** — concurrency / retry under load.
- **[metered-billing.md](metered-billing.md)** — usage-based pricing.

## Invoices & payments

- **[invoices-create.md](invoices-create.md)** — programmatic invoice creation.
- **[invoices-gapless-numbering.md](invoices-gapless-numbering.md)** — Austrian §11 UStG-style compliance.
- **[refunds.md](refunds.md)** — refund flows + auto credit-ledger reversal.
- **[customer-balance.md](customer-balance.md)** — Stripe's customer_balance (vs the lib's credits).
- **[bank-transfers.md](bank-transfers.md)** — SEPA / ACH / wire patterns.

## Operations

- **[deployment-multi-process.md](deployment-multi-process.md)** — multi-replica deployments.
- **[observability.md](observability.md)** — OpenTelemetry tracing + inbound Prometheus metrics.
- **[metrics-outbound.md](metrics-outbound.md)** — outbound Stripe API call metrics.
- **[health-checks.md](health-checks.md)** — `/healthz` + `/readyz` patterns.
- **[ip-allowlist.md](ip-allowlist.md)** — webhook source-IP allowlist + auto-refresh from Stripe.
- **[production-checklist.md](production-checklist.md)** — the pre-launch list.
- **[retries-and-idempotency.md](retries-and-idempotency.md)** — how the lib stays safe under network failure.
- **[troubleshooting.md](troubleshooting.md)** — common adoption pain points.

## Plans, dunning, lifecycle

- **[plan-limits.md](plan-limits.md)** — "what can this customer do?" without re-implementing per app.
- **[dunning-and-grace.md](dunning-and-grace.md)** — payment-delay UX without you writing it.

## Compliance & adoption

- **[gdpr.md](gdpr.md)** — `Customers.Export` (Art. 15) + `Customers.Forget` (Art. 17).
- **[tax-ids.md](tax-ids.md)** — B2B VAT registration on the invoice.
- **[backfill.md](backfill.md)** — migrate existing Stripe customers into a namespace.
- **[migrate-from-stripe-go.md](migrate-from-stripe-go.md)** — incremental adoption.

## Payments & disputes

- **[paymentmethods.md](paymentmethods.md)** — SetupIntents + saved-card management outside Checkout.
- **[disputes.md](disputes.md)** — chargeback evidence submission + Visa CE3.0.
- **[quotes.md](quotes.md)** — B2B sales-led workflow (quote → accept → subscription).
- **[portal-config.md](portal-config.md)** — declarative Customer Portal configuration sync.
