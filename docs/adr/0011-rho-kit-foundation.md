# ADR-0011: rho-kit is a foundational dependency; reuse over reinvention

- **Status:** Accepted
- **Date:** 2026-05-26
- **Deciders:** Markus
- **Amends:** [[adr-0002]], [[adr-0005]], [[adr-0006]], [[adr-0008]], [[adr-0010]]

## Context

`rho-kit` (Apache 2.0; `github.com/bds421/rho-kit/<pkg>/v2`) is the user's standard production Go service toolkit, used across every app he runs. As of 2026-05-26 it ships v2 with hardened primitives for:

- HTTP servers, middleware, signed-request handling.
- SQL connection management (`infra/sqldb`, `infra/sqldb/pgx`).
- Idempotency stores (`data/idempotency` + `pgstore`/`redisstore`).
- Distributed locking (`data/lock/pgadvisory`, `data/lock/redislock`).
- Retry / circuit breaker (`resilience/retry`, `resilience/circuitbreaker`).
- Typed errors (`core/apperror`).
- Tenant identity (`core/tenant`) with compile-time-isolated typed IDs.
- Secret handling (`core/secret`), validation (`core/validate`), redaction (`core/redact`), safe casts (`core/safecast`), testable clock (`core/clock`).
- Append-only chained action log (`data/actionlog`).
- Per-tenant cost/spend ledger (`data/budget`).
- Async approval workflows (`data/approval`).
- Resilient HTTP client (`httpx.NewResilientHTTPClient`).
- Structured logging (`observability/logging` + `logattr`), tracing (`observability/tracing`), RED metrics, runtime metrics, SLOs.
- Lifecycle / cron / batch workers (`runtime/*`).
- Comprehensive test helpers (`infra/sqldb/dbtest`, `infra/redis/redistest`, per-package `*test` packages, `testing/kittest`).

When the original ADRs for rho-stripe were written (0001-0010), they implicitly assumed the lib would re-implement these primitives itself. After discovering rho-kit's coverage, that assumption is wrong: it would duplicate battle-tested code for no portability benefit (this lib is for the user's apps, all of which already use rho-kit).

## Decision

**rho-stripe adopts rho-kit as a foundational dependency.** The core module and the Postgres adapter both depend on rho-kit; reuse rho-kit primitives wherever they fit, do not duplicate.

This is full Option A from the dependency analysis — not the conservative "only leaf primitives" Option C. The reasoning: the user has invested heavily in rho-kit's reliability and consistency, all consuming apps use it, and the portability concern (hypothetical non-rho-kit consumers) doesn't apply to this lib's audience.

## What this changes — concrete mapping

### Subsystems delegated to rho-kit

| rho-stripe concept | rho-kit primitive |
|---|---|
| `SubjectID` typed string | `tenant.ID` (no need to define our own; `tenant.ID` already has the exact semantics, including the same compile-time-isolation rationale) |
| `EventRepo` interface and Postgres impl | `idempotency.Store` interface; `pgstore.Store` is the Postgres backend. Namespace scoping via `data/idempotency/tenant` |
| Credit-deduction concurrency lock | `pgadvisory.Locker.AcquireTx(ctx, hash(subjectID))` inside the deduction transaction |
| Credit-ledger audit trail | `data/actionlog` (append-only chained log) for tamper-evident credit history |
| Stripe API HTTP client | `httpx.NewResilientHTTPClient` (wraps retry + circuit breaker + tracing automatically); stripe-go's `http.Client` set to this |
| Stripe API call retries | `resilience/retry` (used by the resilient HTTP client; can also be used directly for non-HTTP retryable operations) |
| Typed errors (ErrPriceKeyNotFound etc.) | `core/apperror` with our additions to the `Code` enum |
| Catalog spec validation | `core/validate` (struct-tag validator) |
| Catalog Money / Amount typed type | `core/safecast` for safe int64 → currency-amount conversions |
| `STRIPE_SECRET_KEY` / `STRIPE_WEBHOOK_SECRET` handling | `core/secret.Secret[string]` (zeroizable wrapper) |
| Logging in the lib | `observability/logging` + `logattr` for structured fields (consistent format across his apps) |
| Tracing | `observability/tracing` for OTel spans on Stripe API calls, sync runs, webhook dispatch, credit deductions |
| RED metrics for webhook handler | `observability/redmetrics` |
| Time handling in tests | `core/clock` for injectable clock (no `time.Now()` directly in business logic) |
| ID generation | `core/id` for any non-Stripe IDs we need |
| Reference Postgres adapter base | `infra/sqldb` + `infra/sqldb/pgx` |
| Integration tests for Postgres adapter | `infra/sqldb/dbtest/v2` (Docker-backed Postgres) |
| Integration tests for EventRepo contract | `data/idempotency/idempotencytest` |
| Integration tests for lock contract | `data/lock/locktest` |
| Cron / scheduled job hosting (if needed) | `runtime/cron` (but per design, apps schedule jobs themselves; this is available if a future phase wants to host them) |
| Async invoice approval (phase 6) | `data/approval` |

### Subsystems we still own

These remain pure rho-stripe code (no rho-kit equivalent):

- `catalog`: spec types, sync algorithm, drift detection, lookup_key cache, archival rules.
- `checkout`: Checkout Session and Customer Portal session creation; B2B defaults.
- `webhooks`: signature verification (via stripe-go's `webhook.ConstructEvent`), event parsing, namespace-based dispatch, typed handler routing, raw-body framework gotchas. The IDEMPOTENCY part delegates to rho-kit; everything else is ours.
- `subscriptions`: state mirroring, lifecycle operations, out-of-order event handling, reconciliation from Stripe.
- `credits`: ledger semantics, FIFO-by-expiry deduction algorithm, multi-bucket, catalog-driven auto-grant. The LOCK delegates to rho-kit; everything else is ours.
- `metering`: hot/cold split, aggregation, Stripe Meter reconciliation, RecordDirect escape hatch.
- `coupons`: catalog-declared coupons + promo code minting.
- `invoices`: invoice creation, send-invoice flow, audit-log helper. The APPROVAL workflow (phase 6) could delegate to `data/approval`.

### rho-kit packages we do NOT use

Even with full adoption, these are out of scope:

- `app` (service bootstrap) — rho-stripe is a library, not a service.
- `httpx/middleware/*`, `httpx/healthhttp`, `httpx/pagination`, `httpx/mcp` — we don't serve HTTP routes; the webhook handler is just an `http.Handler` consumers wire into THEIR HTTP server (possibly via rho-kit's `httpx`, possibly not).
- `authz`, `authz/openfga`, `httpx/authz` — auth is the consumer's concern.
- `security/jwtutil`, `security/csrf`, `security/asvs` — out of scope.
- `crypto/encrypt`, `crypto/envelope`, `crypto/paseto`, `crypto/passhash` — not relevant; Stripe handles card data, no encryption needs in the lib itself.
- `crypto/signing` — we use stripe-go's `webhook.ConstructEvent` for Stripe's specific HMAC scheme.
- `infra/messaging` — async webhook dispatch (phase 2+) may revisit this; phase 0 doesn't need it.
- `infra/storage`, `infra/storage/*` — no file storage in scope.
- `data/cache`, `data/cache/rediscache`, `data/cache/tenant` — the catalog cache is process-local in-memory; no need for distributed cache.
- `data/queue`, `data/stream` — same; phase 0 is sync.
- `data/ratelimit` — Stripe has its own rate limits; we don't expose ours.
- `runtime/eventbus`, `runtime/batchworker` — not needed for phase 0.
- `data/budget` — superseded by our credit ledger for the credits use case; not used.

### Conventions adopted from rho-kit

- **Module path convention:** `github.com/bds421/rho-stripe/v2/<pkg>` once we tag v2.0.0. Pre-v2.0.0, paths are `github.com/bds421/rho-stripe/<pkg>` (Go module versioning rule: only v2+ requires the suffix).
- **Multi-module structure:** core at root; `repos/postgres` as a separate go.mod (preserved from [[adr-0008]]). Each module gets its own CHANGES.md and tracks its own version.
- **`WithLogger(*slog.Logger)` option pattern** on every package that does logging.
- **Per-package `doc.go`, `CHANGES.md`, `AGENTS.md`** as in rho-kit. `AGENTS.md` per package documents the package decision tree slice ("when do I use this package?").
- **Migrations as embedded SQL** with a `migrations.go` that exposes them via `embed.FS`, applied by the consuming app's preferred migration tool (rho-kit's pattern in `infra/sqldb/pgx`).
- **Panic on programmer errors at construction time** (e.g. `catalog.MustSpec` panics on invalid spec; matches rho-kit's convention of panicking when `auth.JWT` is nil).
- **`core/contextutil` for typed context keys** wherever we propagate values through context.

## Specific ADR amendments

The following ADRs are amended in-place with a brief note pointing here. The substance of those ADRs is unchanged — only the implementation choice is delegated to rho-kit.

### ADR-0002 (Storage and Identity)

`SubjectID` is `tenant.ID` from `core/tenant/v2`. The repo interfaces remain ours; the Postgres reference impl in `repos/postgres` uses `infra/sqldb/pgx` underneath. `EventRepo` is replaced by direct use of `idempotency.Store` (see ADR-0006 amendment).

### ADR-0005 (Credit ledger in app)

Concurrency model uses `pgadvisory.Locker.AcquireTx(ctx, hash(subjectID))` inside the deduction transaction. The `credit_grants` and `credit_deductions` schemas stay as designed. Audit-trail querying may use `data/actionlog` as a complementary append-only chained log if tamper-evidence becomes a requirement; phase 3 starts without it.

### ADR-0006 (Webhook idempotency)

`EventRepo` is no longer a custom interface. The webhook dispatcher takes an `idempotency.Store` directly (or a thin wrapper if we need extra fields like `event_type` / `last_error` / `attempt_count` that aren't first-class in rho-kit's Store). Postgres backend is `pgstore.Store`. The `payload JSONB` storage is still ours — likely added via a separate small `webhook_events` table that references the idempotency key.

This is a non-trivial implementation simplification. The "INSERT ... ON CONFLICT DO NOTHING" race-safe pattern, the owner-token semantics, the TTL handling — all already done. We only implement the bits rho-kit doesn't already cover (typed event family, namespace filter, payload storage for replay).

### ADR-0008 (Go module layout)

The module path uses `/v2` suffix once we tag v2.0.0, matching rho-kit's convention. Pre-v2.0.0 the path is bare (Go module versioning). The core module declares `require` dependencies on rho-kit sub-modules: `core/v2`, `data/idempotency/v2`, `data/lock/pgadvisory/v2`, `resilience/retry/v2`, `observability/logging/v2`, `observability/tracing/v2`, `httpx/v2` (for `NewResilientHTTPClient`). The Postgres adapter additionally depends on `infra/sqldb/v2`, `infra/sqldb/pgx/v2`, `data/idempotency/pgstore/v2`, `data/lock/pgadvisory/v2`.

### ADR-0010 (Test mode + fixtures)

`infra/sqldb/dbtest/v2` provides Docker-backed Postgres test harnesses — replaces our `InMemoryRepos` for integration tests of the Postgres adapter. `data/idempotency/idempotencytest` and `data/lock/locktest` provide contract test suites apps can run against any custom implementation of those primitives. Our `testing.InMemoryRepos()` still ships for unit tests (apps that want zero infrastructure for unit tests of their handlers).

## Consequences

### Positive

- Phase 0 ships faster — 30-40% less code estimated, all the persistence primitives borrowed from battle-tested rho-kit code.
- Consistency across the user's apps: same logging format, same error codes, same retry/tracing behavior, same idempotency semantics.
- Test fixtures are stronger: dbtest-backed integration tests catch bugs that in-memory would miss.
- Observability is first-class from day one: rho-kit's RED metrics + tracing + structured logging are automatic on every Stripe API call.
- Operational handles: rho-kit's resilient HTTP client surfaces Stripe rate-limit and 5xx responses with consistent metrics labels.

### Negative / accepted risks

- **rho-kit version churn affects rho-stripe.** rho-kit is at v2 RC; future v3 would mean coordinated migration. Mitigation: rho-kit is the user's own toolkit; coordination cost is internal.
- **Larger transitive dependency tree.** rho-kit pulls pgx, otel libraries, etc. Acceptable; all consuming apps already pull these via their own rho-kit usage.
- **Less portable for hypothetical non-rho-kit consumers.** Accepted; the user explicitly stated this is not a concern.
- **Convention drift risk.** If rho-kit's conventions shift (e.g. `WithLogger` becomes `Logger(l)`), rho-stripe must follow. Mitigation: minor; CHANGES.md tracking in rho-kit gives advance notice.

### Lib API implications

- `connector.Config.SecretKey` becomes `secret.Secret[string]` (or accepts `string` and wraps internally).
- `connector.Config.Repos.*` signatures take `tenant.ID` instead of a custom `SubjectID`.
- `connector.Config.Repos.Events` becomes `idempotency.Store` directly, OR an `EventStore` extension interface that embeds `idempotency.Store` and adds payload storage.
- Errors returned from lib functions are typed via `apperror` with our codes added to the enum (e.g. `apperror.Code("STRIPE_PRICE_NOT_FOUND")`).
- The lib's loggers accept and propagate the consumer's `*slog.Logger` per the `WithLogger` pattern, or pull from `slog.Default()`.
- `testing.InMemoryRepos()` still ships and returns implementations of `tenant.ID`-keyed maps + an in-memory `idempotency.Store`.

## What needs to be reviewed in the design docs

These design docs reference implementation specifics that change with rho-kit adoption. Surgical edits will follow as part of phase 0 implementation; the design intent is unchanged:

- the relevant package GoDoc: EventRepo section needs to describe the wrap-idempotency-Store pattern instead of the bespoke schema.
- the relevant package GoDoc: Concurrency-model section names `pgadvisory.Locker` instead of describing advisory locks from scratch.
- [[INTEGRATION]]: Step 4 wiring example shows `secret.New(os.Getenv(...))` wrapping the API key; logging uses `slog.Default()`; etc.

## Out of scope

- **Migration of an existing lib's data** (none exists yet; greenfield).
- **rho-kit contribution guidelines** — the user owns rho-kit; this lib is a consumer, not a contributor (for now).
- **Vendoring vs go.mod dependency** — standard go.mod approach; rho-kit modules pulled normally.
- **Whether to also adopt rho-kit's `app` framework** in consuming apps — that's an app-level decision, not a rho-stripe decision.
