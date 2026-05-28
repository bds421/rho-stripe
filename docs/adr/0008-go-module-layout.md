# ADR-0008: Go module layout — core module + separate storage-adapter modules

- **Status:** Accepted (amended)
- **Date:** 2026-05-26
- **Deciders:** Markus
- **Amended by:** [[adr-0011]] (2026-05-26) — adopts rho-kit as a foundational dependency. Core module declares `require` on rho-kit sub-modules (`core/v2`, `data/idempotency/v2`, `resilience/retry/v2`, `observability/logging/v2`, `observability/tracing/v2`, `httpx/v2`). Postgres adapter additionally depends on `infra/sqldb/pgx/v2`, `data/idempotency/pgstore/v2`, `data/lock/pgadvisory/v2`. Module path uses `/v2` suffix once tagged v2.0.0 (rho-kit convention). The two-module structure (core + repos/postgres) is unchanged.

## Context

The lib has multiple sub-packages (`catalog`, `checkout`, `webhooks`, `subscriptions`, `credits`, `metering`, `payments`) and ships reference storage-backend implementations (Postgres now, possibly SQLite or others later). We need a Go module layout that:

- Avoids transitive dependency leakage — apps that don't use Postgres should not pull `pgx` into their build.
- Keeps the developer experience clean (low go.mod count when reasonable).
- Allows independent versioning of optional pieces.
- Matches established Go ecosystem patterns so integrators aren't surprised.

The decision space is constrained by Go's module system: every import dependency in a module's `go.mod` becomes a transitive dependency for anything that imports that module. There is no concept of "optional features" or "dev dependencies" — you either depend or you don't.

## Decision

### Layout: core module + separate adapter modules in the same repo

The git repo contains two (initially) Go modules:

```
rho-stripe/                                     ← git repo root
├── go.mod                                            ← core module
├── connector/                                        ← main facade package
├── catalog/  checkout/  webhooks/                    ← feature packages
├── subscriptions/  credits/  metering/  payments/    ← feature packages
├── internal/                                         ← non-exported helpers
├── cmd/rho-stripe/                             ← CLI binary
│
└── repos/
    └── postgres/
        ├── go.mod                                    ← separate adapter module
        └── (Postgres impl of repo interfaces, depends on core + pgx)
```

Apps that use the Postgres adapter import both:

```go
import (
    "<path>/connector"
    "<path>/repos/postgres"
)
```

Apps with their own DB or different storage import only `/connector` and implement the repo interfaces themselves.

### Module path: placeholder for now, renamable later

The core module declares its path as `github.com/bds421/rho-stripe` for now. This is a **placeholder** — the actual hosting decision (public GitHub vs private vs self-hosted) is deferred. Renaming requires a single repo-wide find-replace across `go.mod` files and any import documentation; no code change beyond that. The choice has no architectural consequences.

### Versioning: pre-1.0 until phase 0+1+2 ship and stabilize

The lib starts at `v0.1.0` and stays in the v0.x.y range until the API has been used in real apps for several months. Pre-1.0 versions can introduce breaking changes between minor versions (Go's module semver convention permits this).

After 1.0:
- Strict semver. Breaking changes require a new major version (`v2`).
- Major version bumps follow Go's "module path versioning" convention: `<path>/v2`.

Each module is versioned independently:
- Core: `v0.3.0`, `v0.4.0`, ... → eventually `v1.0.0`.
- Postgres adapter: `repos/postgres/v0.1.2`, ... independent track.

This lets us patch the Postgres adapter without re-releasing core, and vice versa.

### Local development: Go workspaces

A `go.work` file at the repo root includes all modules, so `go build ./...` works across modules during local dev without publishing intermediate versions. The `go.work` file is committed (Go 1.22+ best practice for monorepos).

```go
// go.work
go 1.22

use (
    .
    ./repos/postgres
)
```

### Internal vs exported packages

`internal/` packages are for non-public helpers:
- HTTP retry middleware, idempotency-key generation, raw-body utilities.
- Catalog sync algorithm details (public API is `Sync(ctx, ...)` + the CLI).
- Webhook dispatcher plumbing (public API is `Webhooks.Handle(...)` + Handlers struct).

Everything else is exported. Apps may construct catalog entries, checkout inputs, credit grants, etc. directly via the typed structs. The lib does NOT enforce constructor functions for every type — Go's natural ergonomic is `Type{Field: Value}` and we honor that.

### Stripe SDK dependency

Core module depends on `github.com/stripe/stripe-go/v76` (or the version current at phase 0 implementation). Pinned explicitly. Upgrade policy:

- Stripe-go minor/patch bumps: adopt on the next core release if no breaking changes.
- Stripe-go major bumps: adopt within ~3 months. The major-version migration becomes a core minor-version bump (in pre-1.0) or major-version bump (post-1.0).

The lib's API does NOT re-export Stripe SDK types directly. Wrappers exist for everything apps interact with (`catalog.Price`, `webhooks.Event`, etc.) so the Stripe SDK can be swapped or upgraded without breaking apps' code.

### CLI placement

`cmd/rho-stripe/` lives in the core module. The CLI depends on `catalog/` (declarative spec types) and `internal/sync/` (the sync algorithm). Putting it in core avoids a third module and keeps the CLI's version aligned with the catalog format it understands.

Install: `go install <path>/cmd/rho-stripe@latest`. The binary is named `rho-stripe`.

## Options considered

### Layout A: single module, everything inside — rejected

Forces every app to transitively depend on `pgx` (and any future SQLite/MySQL drivers) regardless of which storage they use. Bloats binary size, slows builds, ties release cadence of unrelated pieces. For a lib that explicitly aims at "small core + composable modules" (per [[adr-0002]]), this trade-off is wrong.

### Layout C: multi-module monorepo, every feature its own module — rejected

Each feature package (`catalog`, `webhooks`, etc.) becoming its own module would isolate dependencies further but adds 7+ go.mod files and 7+ release tracks for no real benefit — none of the feature packages have heavy or distinct dependencies. They all depend on `stripe-go` which is already required for core functionality. The split would be engineering for engineering's sake.

### Hosting: public GitHub vs private vs self-hosted vs deferred — chose deferred

The hosting question has operational implications (CI auth tokens, GOPRIVATE config, license choice) but no architectural ones. Picking a placeholder lets us proceed with all other design and code work; the rename to the final path is a 5-minute operation when the hosting decision is made.

### Versioning: pre-1.0 vs jump to 1.0 — chose pre-1.0

A 1.0 declaration commits to no breaking changes. For a lib designed before any consumer exists, this commitment would be premature. Pre-1.0 lets the API evolve based on what's actually painful when integrated. 1.0 is earned after real use, not assigned aspirationally.

### Wrappers vs re-exports of Stripe SDK types — chose wrappers

Re-exporting (`type Price = stripe.Price`) would save code but couple every consumer to the Stripe SDK's API shape and major-version cadence. Wrappers cost some code but give us control over the public surface, ability to add fields the SDK doesn't have (like our `TaxCategory` shorthand), and freedom to upgrade Stripe-go without breaking apps.

## Consequences

### Positive

- Apps that don't use Postgres are unaffected by its dependency chain.
- Core and adapters can be patched independently — Postgres bug fixes don't require a core release.
- `go.work` makes local cross-module development frictionless.
- The /internal/ convention enforces a clear public API boundary.
- Versioning strategy lets us evolve the API freely pre-1.0 and commit only after real-world feedback.

### Negative / accepted risks

- Two go.mod files to maintain (will grow to 3-4 over time as more adapters appear). Acceptable; Go tooling handles it well, and the alternative (single module with dep leakage) is worse.
- Version-skew is possible: an app might pin core to `v0.5.0` but Postgres adapter to `v0.6.0` requiring core `>=v0.7.0`. Mitigation: each adapter's `go.mod` declares its minimum core version explicitly; CI runs `go mod tidy` + `go build ./...` across both to catch skew before release.
- The placeholder import path will need a one-time rename when hosting is decided. Mitigation: that rename is mechanical and the surface (go.mod + docs) is small.
- Apps don't get the Stripe SDK types directly — they interact with our wrappers. Some Stripe operations not yet wrapped will be inaccessible until added. Mitigation: each wrapper type has an `Underlying() *stripe.Type` escape hatch on advanced types where useful, documented as "unstable, may change."

### Lib API implications

- Apps import `<path>/connector` for the main facade; sub-packages (`<path>/catalog`, `<path>/checkout`) for typed constructor structs.
- Apps using the reference Postgres adapter import `<path>/repos/postgres` and call `postgres.NewRepos(db)` to get a `connector.Repos` ready to wire in.
- The CLI is invoked as `rho-stripe sync`, `rho-stripe verify`, `rho-stripe diff`, etc. — subcommand UX detailed in the relevant package GoDoc.
- The Go version floor is `go 1.22` (for `go.work`, generics maturity, and slices/maps stdlib helpers).

## Out of scope (defer to other docs)

- CLI subcommand UX → the relevant package GoDoc.
- Wrapper type design for Stripe SDK objects → the relevant package GoDoc.
- Future SQLite adapter — same pattern (`repos/sqlite/go.mod`) when needed, no decision needed now.
- Public release process (changelog, release notes, GitHub Actions for tagging) → operational, not architectural.
