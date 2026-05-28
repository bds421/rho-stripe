# ADR-0010: Test mode and fixtures strategy

- **Status:** Accepted (amended)
- **Date:** 2026-05-26
- **Deciders:** Markus
- **Amended by:** [[adr-0011]] (2026-05-26) — Postgres adapter integration tests use rho-kit's `infra/sqldb/dbtest/v2` (Docker-backed Postgres harness). `EventRepo` contract tests use rho-kit's `data/idempotency/idempotencytest`. Lock contract tests use `data/lock/locktest`. The layered strategy (unit + stripe-mock + gated real Stripe) and `connector/testing` package scope are unchanged.

## Context

Two audiences need testing infrastructure:

1. **The lib itself** — its own CI test suite must give fast, reliable feedback as the lib evolves, while also catching real-Stripe-specific behaviors.
2. **Consuming apps** — each integrating app needs to test its handlers, business logic, and orchestration without standing up real Stripe in CI.

Testing payment integrations is its own discipline. The available tools cover a spectrum from "no Stripe at all" (in-memory mocks) to "real Stripe test mode over the wire" (highest fidelity, slowest, requires credentials). Picking the wrong point on the spectrum costs either fidelity (bugs reach prod) or velocity (tests are slow, flaky, or hard to run locally).

## Decision

### Lib's own tests: layered strategy

```
┌──────────────────────────────────────────────────────────────┐
│ Layer 1 — Unit tests (no Stripe)            ~70-80% of suite │
│  • In-memory repo impls test repo-using code.                │
│  • Catalog spec validation, sync diff algorithm, etc.        │
│  • Webhook handler logic with hand-constructed signed events │
│    using the WebhookSigner test helper.                      │
│  • Runs in <1s. No external deps. Every PR.                  │
├──────────────────────────────────────────────────────────────┤
│ Layer 2 — Integration tests against stripe-mock  ~15-20%     │
│  • Lib's HTTP path to Stripe exercised via stripe-mock.      │
│  • Catches "did we send the right params, did we handle the  │
│    response shape correctly" bugs.                           │
│  • Runs in ~10-30s. Every PR.                                │
├──────────────────────────────────────────────────────────────┤
│ Layer 3 — Smoke tests against real Stripe test mode  ~5-10%  │
│  • 10-20 tests exercising full flows: sync, checkout,        │
│    real webhook delivery via stripe-cli, state verification. │
│  • Runs nightly + on release-candidate tags. NOT on every PR │
│    (rate limits, time, credentials surface).                 │
│  • Uses a maintainer-controlled test-mode account; creds in  │
│    GH Actions secrets (or equivalent CI secret store).       │
└──────────────────────────────────────────────────────────────┘
```

Each test file declares which layer it belongs to via build tags (`//go:build unit`, `//go:build integration`, `//go:build e2e`). Default `go test ./...` runs layer 1. `make test-integration` adds layer 2. `make test-e2e` adds layer 3.

stripe-mock is run as a sidecar in CI (Docker container) and as a local binary in dev. Real-Stripe e2e tests use `stripe listen --forward-to` (via stripe-cli) to receive webhooks at the local/CI endpoint.

### Helpers shipped for consuming apps

The lib ships a dedicated sub-package `connector/testing` with:

**`testing.InMemoryRepos() connector.Repos`**
Returns a fully populated `connector.Repos` struct, all backed by in-memory maps. Apps wire it up with `connector.New(connector.Config{Repos: testing.InMemoryRepos(), ...})` for tests that don't need a DB. Thread-safe; safe for parallel tests.

**`testing.NewWebhookSigner(secret string) *WebhookSigner`**
Constructs a helper that signs payloads with the same algorithm Stripe uses. Apps build test events as `webhooks.Event` structs (or use `EventFixtures`), sign them, and POST to their own webhook handler in tests. No real Stripe involvement; signature verification works end-to-end.

```go
signer := testing.NewWebhookSigner("whsec_test_xxx")
body, sig := signer.Sign(myTestEvent)
req := httptest.NewRequest("POST", "/webhook", bytes.NewReader(body))
req.Header.Set("Stripe-Signature", sig)
myHandler.ServeHTTP(rec, req)
```

**`testing.EventFixtures`**
Pre-built event payloads for the common cases:
- `EventFixtures.CheckoutCompleted(subjectID, productKey)`
- `EventFixtures.InvoicePaid(subjectID, amount)`
- `EventFixtures.SubscriptionCreated(subjectID, productKey)`
- `EventFixtures.SubscriptionCanceled(subjectID)`
- `EventFixtures.PaymentFailed(subjectID, reason)`
- `EventFixtures.CreditPackPurchased(subjectID, productKey)`

Each returns a `*webhooks.Event` with realistic fields (timestamps, IDs, metadata stamps including `app_namespace`). Apps mutate the fields they care about and ignore the rest.

**`testing.FakeConnector(catalog) *connector.Connector`**
A `*Connector` whose Stripe-side operations succeed in-memory without API calls. `CreateCheckoutSession` returns a fake URL; `GrantCredit` records the grant in InMemoryRepos; `Refresh` is a no-op. Useful when an app wants to test its own orchestration logic ("when this happens, my code calls conn.Credits.TryDeduct correctly") without integration overhead.

### Baseline fixture shipped from phase 0

`fixtures/baseline.json` is a Stripe Fixtures CLI spec that creates a known starter dataset in a test-mode account:

- 2 sample products with monthly/yearly prices in EUR/USD/GBP
- 1 sample credit-pack product (one-time, with a CreditGrant declaration mirrored in the spec)
- 2 sample coupons (one fixed-amount, one percent-off)
- 3 sample customers (one with a saved card, one with SEPA mandate, one with no payment method)

Loading: `stripe fixtures fixtures/baseline.json --api-key sk_test_xxx`. A `make seed-test-account` target wraps this and is documented in the lib's README and contributor guide.

Reasoning for shipping from phase 0: the first new contributor will need it; the first time the lib is used in a fresh test environment, having a one-command reset is high-leverage. The cost (one JSON file) is small.

### Sync CLI test mode

The `rho-stripe` CLI accepts `--test-mode` (or auto-detects from `sk_test_xxx` keys) and operates against the test environment. The CLI is the same binary for test and prod; there's no separate test-mode tool to maintain.

### What does NOT get tested by the lib (deliberately)

- Real card processing (PCI scope) — never. Stripe handles all card data; the lib doesn't see card numbers.
- Real money movement — never. All tests use test-mode keys; production-key smoke tests are an anti-pattern.
- App-side business logic — the lib provides testing helpers, but each app tests its own logic.
- Stripe Tax calculations — Stripe Tax has its own test scenarios in Stripe's docs; the lib tests that it correctly enables `automatic_tax: true` and passes through the result, not that the tax math is right.

## Options considered

### All real Stripe vs all stripe-mock vs layered — chose layered

All-real is slow and credential-burdened on every PR. All-stripe-mock misses real-Stripe quirks and behaviors not in the OpenAPI spec. Layered gets the speed of mock for the common case and the fidelity of real Stripe for the cases that matter — at the cost of slightly more CI configuration. Industry standard for billing libs (Stripe's own SDKs use a similar pattern internally).

### Custom mocks vs stripe-mock — chose stripe-mock

Custom hand-written mocks become assertions about mock calls, not about behavior. They drift from real Stripe silently and break tests when SDK upgrades happen. stripe-mock is Stripe's own canonical mock, tracks the spec, and gets updates from Stripe themselves.

### Consuming-app helpers: full vs minimal vs none — chose full

Minimal (just `InMemoryRepos` + `WebhookSigner`) defers two helpers (`EventFixtures`, `FakeConnector`) that have obvious value. Apps that don't use them pay nothing. Apps that do are saved real work — `EventFixtures` alone eliminates the "construct a webhook event payload that Stripe would actually send" research task that every integrator otherwise does themselves. The marginal cost of shipping all four is small relative to the per-app cost saved.

### Baseline fixtures: phase 0 vs phase 1 vs skip — chose phase 0 (upgrade from original recommendation)

User chose to ship from phase 0. Cost is a single JSON file; benefit is "anyone can reset to a known state in one command" from day one. Pays off the first time someone — including future-Markus six months later — needs to recreate a fresh test environment.

## Consequences

### Positive

- Test layers are explicit and discoverable via build tags. Contributors know exactly what's running when.
- stripe-mock keeps PR-time CI under 30s for the whole suite; nightly real-Stripe smoke catches the high-fidelity cases.
- Consuming apps get a comprehensive testing toolkit that makes "test your handler" a one-screen task, not a weekend project.
- `baseline.json` removes a category of "set up your test account first" onboarding friction.
- The `connector/testing` sub-package keeps test-only code out of production builds (it's never imported by `connector` itself, so Go's dead-code elimination drops it).

### Negative / accepted risks

- **Maintainer test-mode account required.** Smoke tests need credentials. Acceptable; documented as a setup step in `CONTRIBUTING.md`. Forks/contributors can develop and run layers 1+2 without any Stripe credentials — only layer 3 requires them.
- **stripe-mock drift.** Stripe-mock can lag the real API. Acceptable because layer 3 catches drift on nightly runs, and most lib code is tested at layer 1 anyway.
- **`testing.EventFixtures` must be kept current** as Stripe event shapes evolve. Modest ongoing cost; failing tests at layer 3 would surface drift, and the fixtures are a finite, finite-growing list.
- **`FakeConnector` can mask integration bugs.** Apps that test only with FakeConnector and never against real (or mocked) Stripe might miss real-API behaviors. Mitigation: doc clearly recommends combining FakeConnector tests with at least one real or stripe-mock integration test per app.

### Lib API implications

- `connector/testing` is a Go sub-package; it imports `connector` and `webhooks` but is never imported by them. Safe to depend on from app test code, dead-code-eliminated in app production builds.
- The `WebhookSigner` reuses the same signing helper the lib uses internally to *verify* signatures — single implementation, two callers (real verification + test signing), no drift.
- The CLI gains a `rho-stripe verify-test-account` subcommand that runs a quick health check against a configured test-mode account (sync round-trip, fixture creation, webhook delivery test). Useful for confirming environment setup before running smoke tests.

## Out of scope (defer to other docs)

- Specific test cases per subsystem → see each package's `*_test.go` files.
- CI tooling choice (GitHub Actions, GitLab CI, etc.) → operational, not architectural. Initial setup will use GitHub Actions because it's the assumed default; switchable.
- Performance benchmarks → not part of phase 0; add when performance becomes a concern.
- Load tests against Stripe's rate limits → out of scope; the lib should not generate enough load in tests to hit rate limits.
