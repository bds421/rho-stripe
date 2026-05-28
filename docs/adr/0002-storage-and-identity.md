# ADR-0002: Storage-agnostic via repo interfaces, abstract SubjectID, optional ActorID

- **Status:** Accepted (amended)
- **Date:** 2026-05-26
- **Deciders:** Markus
- **Amended by:** [[adr-0011]] (2026-05-26) — `SubjectID` is realized as `tenant.ID` from rho-kit's `core/tenant/v2`; the reference Postgres adapter uses `infra/sqldb/pgx/v2`; `EventRepo` is delegated to rho-kit's `idempotency.Store`. The design intent below is unchanged; only implementation choice shifts.

## Context

The `rho-stripe` library is shared across multiple apps with different databases (some Postgres, some SQLite, possibly others) and different identity models (some B2B with org-as-buyer, some B2C with user-as-buyer). The lib needs to support all of these without forcing a schema or a buyer model on its integrators.

Two concerns are intertwined and decided together in this ADR:

1. **How does the lib persist data** when each app owns its own database?
2. **What does "the customer" mean** to the lib — an org/tenant, an individual user, or neither?

## Decision

### 1. Storage: repo interfaces implemented per app

The lib defines a small set of storage interfaces. Each integrating app implements them against its own DB and passes them into `connector.New(...)`. The core lib has no database dependency, no driver imports, no migration runner.

```go
// Sketch — final signatures live in the package source.
type CustomerRepo interface {
    Get(ctx context.Context, subject SubjectID) (*Customer, error)
    Upsert(ctx context.Context, c *Customer) error
}

type SubscriptionRepo interface {
    GetBySubject(ctx context.Context, subject SubjectID) ([]*Subscription, error)
    Upsert(ctx context.Context, s *Subscription) error
}

type CreditRepo interface {
    GrantCredit(ctx context.Context, g CreditGrant) error
    DeductCredit(ctx context.Context, d CreditDeduction) (Balance, error)
    Balance(ctx context.Context, subject SubjectID, bucket string) (Balance, error)
    // ... expiry, listing, refund
}

type EventRepo interface {
    SeenEvent(ctx context.Context, eventID string) (bool, error)
    MarkEventSeen(ctx context.Context, eventID string) error
}

type UsageRepo interface {
    RecordUsage(ctx context.Context, u UsageEvent) error
    AggregateByPeriod(ctx context.Context, metric string, period Period) ([]UsageAggregate, error)
}
```

Reference implementations for Postgres ship in a separate sub-package (`connector/repos/postgres`). Apps using SQLite or another store implement the interfaces themselves. Migrations are NEVER run by the lib — the reference impls expose embedded SQL DDL that apps apply with their own migration tooling (goose, atlas, golang-migrate, raw `psql`, whatever).

### 2. Identity: abstract `SubjectID`, optional `ActorID`

The lib treats the billing subject as an opaque identifier:

```go
type SubjectID string  // app decides what this means
type ActorID   string  // optional, for audit trails
```

Each integrating app chooses what `SubjectID` represents — an org_id, a user_id, a tenant_id, whatever — and **uses it consistently within that app**. The lib enforces "one `SubjectID` ↔ one Stripe Customer" through `CustomerRepo`, but it does not (and cannot) enforce what a `SubjectID` *means*. That is documentation-and-discipline territory.

For B2B audit trails, the lib's checkout/portal/charge APIs accept an optional `ActorID` which is mapped to Stripe's `client_reference_id` on the resulting session. `ActorID` is who *initiated* the purchase (typically a user); `SubjectID` is who *pays* (typically the user's org). They are often equal in B2C, almost always different in B2B.

```go
url, _ := conn.Checkout.CreateSession(ctx, CheckoutInput{
    SubjectID:   "org_acme",     // who pays
    Actor:     "user_alice",   // who clicked (optional)
    LineItems: []string{"pro_plan.yearly"},
    // ...
})
```

## Options considered

### Identity: Pattern 1 (Subject=Org), Pattern 2 (Subject=User), Pattern 3 (abstract) — chose Pattern 3

User confirmed (2026-05-26) that some apps will be org-billed and others user-billed. Picking either Pattern 1 or Pattern 2 would force the other category of apps to model "an org of one" or similar awkwardness. Pattern 3 (abstract) imposes no model, costs nothing in API complexity, and supports both.

Trade-off accepted: the lib cannot prevent an app from mixing semantics within itself (sometimes treating a SubjectID as an org, sometimes as a user). This is documented as a rule in the integration guide — and mixed semantics would manifest as duplicate Stripe Customers, which is highly visible in the Stripe dashboard and easy to catch.

### Actor: optional vs always-required vs absent — chose optional

Always-required would force B2C apps (where actor == subject) to pass redundant data. Absent would mean B2B apps lose audit trail of who initiated a purchase. Optional gives both worlds without cost: B2C apps omit it, B2B apps pass it.

### Repos: one big interface vs many small ones — chose many small

A single `Storage` interface bundling all methods would force apps to implement methods they don't use (e.g. credits in a subscription-only app). Splitting per concern means each app's wiring only references repos for features it actually uses, and partial implementations are first-class. Matches Go interface conventions (small interfaces, composed at use site).

### Reference impl: in core vs sub-package — chose sub-package

Putting a Postgres impl in the core lib would force a `pq`/`pgx` dependency on everyone. Separate sub-package means SQLite apps don't pull in Postgres drivers, and apps using neither pay nothing.

## Consequences

### Positive

- The lib has zero runtime database dependencies. Easy to test, easy to mock, easy to port.
- Apps with existing schemas can implement the interfaces without restructuring (or use the reference impl as a starting point and adapt).
- Adding a new feature (e.g. invoices in phase 6) means adding a new repo interface, not changing existing ones — no breaking changes for apps that don't use the new feature.
- The `SubjectID`/`ActorID` split makes the B2B vs B2C distinction explicit in app code: every checkout call documents both "who pays" and "who clicked" at the call site, even if Actor is empty.

### Negative / accepted risks

- Apps that mix `SubjectID` semantics will produce duplicate Stripe Customers. Mitigation: clear documentation + the rule shows up immediately in the Stripe dashboard as obvious duplicates.
- Apps must implement repos correctly — bugs in app-side repo impls can corrupt state. Mitigation: ship a comprehensive test suite that an app's repo impl can run against itself (`repos/contract_test.go`) to verify it satisfies the lib's contract.
- The reference Postgres impl adds maintenance cost to keep schemas current with interface evolution. Accepted; the alternative (no reference impl) means every integrator pays the same cost in isolation.

### Lib API implications

- `connector.New(...)` takes a `Repos` struct with only the interfaces it needs; unsupplied repos disable features that require them (e.g. no `CreditRepo` → `conn.Credits.*` methods return a clear "not configured" error).
- `SubjectID` and `ActorID` are typed string aliases, not bare `string` — protects against parameter-order mistakes at compile time.
- The reference Postgres impl ships with `schema.sql` files per repo, designed to be embedded via `go:embed` so apps can read them programmatically if they want to.

## Out of scope (defer to other ADRs)

- How webhook events are deduped via `EventRepo` (per-app vs shared) → [[adr-0006]].
- How the credit ledger handles concurrent deductions on the same subject → the relevant package GoDoc.
- Whether the reference impl supports both Postgres and SQLite, or just one → defer to phase 0 implementation; start with Postgres only.
