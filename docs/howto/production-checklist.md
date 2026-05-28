# Production checklist

Before flipping `STRIPE_SECRET_KEY=sk_live_…` and pointing a real
domain at the webhook endpoint, walk this list. Each item links to the
doc that goes deeper.

## Reliability

- [ ] **Idempotency keys enabled** — automatic; verify with
      `TestLive_IdempotencyKeyDedupesCustomers`. See
      [retries-and-idempotency.md](retries-and-idempotency.md).
- [ ] **`MaxNetworkRetries` set explicitly** — the default of 3 is fine
      for most apps; high-volume backends may want 5+.
- [ ] **Per-request `Timeout` tuned for your network** — default 30s.
- [ ] **Postgres-backed `idempotency.Store` + `webhook.EventLog`** —
      memory stores are for tests only; processes die.
- [ ] **Async webhook dispatch enabled** for endpoints expecting >100
      events/sec — without it, Stripe will mark the endpoint slow and
      back off. See [webhooks-async-dispatch.md](webhooks-async-dispatch.md).
- [ ] **Graceful shutdown wired** — call `conn.Shutdown(ctx)` from
      your SIGTERM handler; drains in-flight queue + drift detector.

## Compliance

- [ ] **`AppDataExporter` + `AppDataForgetter` callbacks wired** — see
      [gdpr.md](gdpr.md).
- [ ] **Webhook event-log pruning scheduled** — periodic
      `conn.Webhooks.PruneOldEvents(ctx, time.Now().Add(-90*24*time.Hour))`
      so old event payloads (potentially containing forgotten-customer
      data) don't accumulate.
- [ ] **Tax Codes set on every Product** — Stripe Tax will compute
      wrong rates if every product is `txcd_99999999` (generic).
      See [tax-codes.md](tax-codes.md).
- [ ] **B2B Tax-ID collection enabled** if you sell to businesses —
      default for `SessionDefaults`. See [tax-ids.md](tax-ids.md).
- [ ] **GDPR processor agreement in place** with Stripe (DPA).

## Security

- [ ] **`STRIPE_SECRET_KEY` and `STRIPE_WEBHOOK_SECRET` in secrets manager**,
      not env files in git. See [webhooks-secret-rotation.md](webhooks-secret-rotation.md).
- [ ] **Signing secret rotation runbook documented + tested**.
- [ ] **Webhook endpoint HTTPS-only** — Stripe will not deliver to HTTP
      in live mode.
- [ ] **Webhook handler returns 200 quickly** — heavy work goes to the
      async queue.
- [ ] **No `BackendOverride` / `CheckoutBackendOverride` in production
      config** — those are test seams.

## Operations

- [ ] **OpenTelemetry tracing wired** —
      `observability/otel.Wrap(wh, tracer)`. See [observability.md](observability.md).
- [ ] **Prometheus metrics scraped** — `observability/metrics.New(registry)`.
      Includes: webhook handle duration, queue depth, signature failures,
      dedup hits, async dispatch errors.
- [ ] **Drift detector running** — daily CronJob (see
      [examples/k8s/cronjob-drift-check.yaml](../../examples/k8s/cronjob-drift-check.yaml))
      or in-process via `Config.DriftDetector`.
- [ ] **Multi-replica deployment** — webhook dedup is correct across
      replicas (Postgres-backed); catalog cache is per-process and
      warms eagerly on `connector.New`. See
      [deployment-multi-process.md](deployment-multi-process.md).
- [ ] **`/healthz` returns 200 only after `connector.New` succeeded**.
- [ ] **`preStop` sleep + `terminationGracePeriodSeconds: 30`** on
      pods — gives in-flight webhooks time to drain. See
      `examples/k8s/deployment.yaml`.

## Migration / adoption

- [ ] **Existing customers imported** via `conn.Customers.ImportCustomer`
      — without this, their webhook events get dropped at the namespace
      filter. See [backfill.md](backfill.md).
- [ ] **Invoice number repo seeded** to your existing high-water mark
      so the lib doesn't reuse numbers your old system already issued.
      See [invoices-gapless-numbering.md](invoices-gapless-numbering.md).
- [ ] **Webhook endpoint registered in Stripe dashboard** for the new
      domain + new signing secret.

## Pre-launch test plan

- [ ] **Live tests pass** against your *test-mode* account:
      `go test -tags=live_stripe ./...` (17/17 expected as of slice 50).
- [ ] **Race detector clean** on touched packages:
      `go test -race ./webhooks/... ./subscriptions/... ./connector/... ./customers/...`.
- [ ] **Load test the async queue** at your expected webhook rate. The
      `webhooks/handler_test.go` async-queue tests cover correctness;
      run a 10-minute soak at 2× peak before flipping live.
- [ ] **Drift-check passes** on a fresh apply — proves your catalog
      spec matches what's in Stripe.
- [ ] **Manual fault-injection**: kill the app mid-checkout; verify
      the next Stripe webhook reconciles state correctly (the lib's
      `event.created` watermark + idempotency keys make this safe).

## Things this library deliberately does NOT do

So you know what you still need to handle:

- **Stripe Connect** (marketplace / multi-account) — out of scope.
- **Stripe Identity** (KYC) — out of scope.
- **Apple Pay / Google Pay domain verification** — handle via Stripe
  Dashboard one-time setup.
- **3DS / SCA enforcement** — Stripe handles automatically; the lib
  doesn't add a layer.
- **PCI scope reduction** — Stripe Checkout handles card collection;
  the lib never sees raw card data.

If you need any of these, you can use `conn.Stripe` (raw stripe-go
client) as an escape hatch.
