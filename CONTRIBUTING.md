# Contributing

Conventions to follow when adding code to `rho-stripe`. These
exist because the lib has historical context that won't survive
ordinary code review without prompts.

## Convention: every Stripe-object creation MUST stamp `metadata.app_namespace`

The webhook dispatcher uses `metadata.app_namespace` to route events
to the correct app in a shared Stripe account ([ADR-0007](docs/adr/0007-single-stripe-account.md)).
Every call site in the lib that creates a Stripe object via stripe-go
**must** stamp this metadata, or downstream webhook events for that
object will be classified as `filterUnrouted` and silently dropped
(routed only to `OnOtherEvent`, never to typed handlers).

The current creation sites are:
- `catalog/sync.go` → `stripeapi.Backend.CreateProduct` / `CreatePrice`
  (stamps via `namespacedMetadata` in `diff.go`).
- `stripeapi/checkout_backend.go` → `Customers.New` (stamps via
  `Input.Metadata` propagation from `checkout.sessionMetadata`).
- `stripeapi/checkout_backend.go` → `CheckoutSessions.New` (stamps
  via `Input.Metadata` AND propagates to `SubscriptionData.Metadata` +
  `PaymentIntentData.Metadata` so downstream subscription/invoice
  events also carry the stamp).

When adding a new creation path (e.g. `Subscriptions.Migrate`,
`Refunds.Create`, `Invoices.Create`), the PR description **must**
list how the new object's metadata stamps include `app_namespace`,
and a test must assert it.

There is no compile-time enforcement of this; it's a discipline-only
convention. A periodic static-analysis sweep (see
`stripeapi/namespace_stamping_static_test.go`) catches new sites
that touch metadata without referencing app_namespace.

## Convention: never pass empty-string booleans to Stripe

Stripe's API rejects empty-string boolean values (e.g.
`active=""`) with a 400 `Invalid boolean`. To express "no filter
on this boolean field," **omit the parameter entirely** rather than
setting it to `""`. The slice-4 bug taught us this the hard way:
`listPricesForProduct` had `params.Filters.AddFilter("active", "", "")`
thinking it meant "no filter," but Stripe interpreted it as
`active=""` and 400'd.

The fix is now documented in a code comment in
`stripeapi/backend.go` next to `listPricesForProduct`. Apply the
same principle when adding new list calls: any optional boolean
parameter is either set to `"true"`/`"false"` or NOT added at all.

## Conventions on test layering

Per [ADR-0010](docs/adr/0010-test-mode-and-fixtures.md) tests run in
three layers:

1. **Unit tests** (default) — fast, in-memory, no Stripe. Default
   `go test ./...` runs only these. Build tag: none.
2. **Stripe-mock tests** (planned, not yet wired) — exercise the
   stripe-go HTTP path against stripe-mock. Build tag: `stripe_mock`.
3. **Live integration tests** — hit real Stripe test mode or a real
   Postgres. Build tag: `integration` or `postgres_integration`.
   Skip when their backing service isn't reachable.

When adding a test:
- Default to layer 1. Use `connectortest` helpers.
- Reach for layer 3 only when the unit-test fake genuinely can't
  catch the bug (anything depending on Stripe response shape or
  Postgres-specific concurrency semantics).

## CHANGELOG hygiene

Every user-visible change lands in `CHANGELOG.md` under `[Unreleased]`
before merge. Sections per Keep-a-Changelog: `Added`, `Changed`,
`Deprecated`, `Removed`, `Fixed`, `Security`. Breaking changes
prefixed with **BREAKING** until v1.0.

## Conventions on rho-kit usage

Per [ADR-0011](docs/adr/0011-rho-kit-foundation.md) the lib depends
on rho-kit broadly. When you need a primitive (idempotency, lock,
retry, structured logging, typed error codes), **check rho-kit
first** before reinventing it. The default answer is usually
"there's a rho-kit package for that."

## Code-style conventions

- Per-package `doc.go`-style package comment at the top of one
  source file in each package (Go convention). Keep it tight.
- Don't write comments that explain WHAT the code does — Go's
  named identifiers do that. Comments explain WHY, especially
  non-obvious WHY (e.g. "Stripe rejects empty-string booleans, so
  we omit the field entirely").
- Construct typed errors via `apperror.ValidationError` etc. when
  the lib's caller might want to switch on the error kind.
- `WithLogger(*slog.Logger)` option pattern wherever logging happens.

## Time and clocks

Prefer `clock.System.Now()` over `time.Now()` in code that should be
deterministically testable (subscription state transitions, credit
expiry, drift watermarks). Accept an injectable clock in the
Operations constructor or via a SetClock method.

WARNING: `time.Since(fakeClock.Now())` returns garbage — Go's
monotonic-clock rules. Use `clock.Since(c, t)` instead when both
sides should come from the same clock.

For pure-projection paths (mapping a Stripe timestamp into a struct),
`time.Unix(...)` is fine — no clock needed.

## Constructor convention

Operations-style constructors PANIC on missing required deps (matches
webhooks.New / subscriptions.New). Do NOT return `(_, error)` where
the error is never non-nil — it's an API smell that misleads callers.
Reserve error returns for validation paths that genuinely can fail.

## Test style

Prefer table-driven tests for any test with >2 similar cases:

	for _, c := range []struct{
	    name, input, want string
	}{
	    {"empty", "", ""},
	    {"ascii", "hello", "hello"},
	} {
	    t.Run(c.name, func(t *testing.T) { ... })
	}

White-box tests (same package as code) are fine for time-sensitive
internals; prefer black-box (`_test` package) for API surface tests
so refactors don't churn tests.

Add a benchmark for any hot path you touch (`Benchmark*` functions
next to the unit tests). The lib has benchmarks in `stripeapi` and
`webhooks` as starter shape.
