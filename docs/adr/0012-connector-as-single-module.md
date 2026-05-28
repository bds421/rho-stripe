# ADR-0012 — Connector lives in the same Go module as its sub-packages

Date: 2026-05-27 (slice 54 review)
Status: Accepted

## Context

The lib has ~23 Go packages. The `connector` package imports every
other package (it's the facade). A single change in any sub-package
forces `connector` to recompile, which forces every consumer of
`connector` to recompile.

A common Go-large-codebase pattern is to split the facade into its
own module (separate `go.mod`), letting consumers depend on
sub-packages directly without dragging the facade transitively.

## Decision

Keep `connector` in the main module. Do **not** split into a
separate module.

## Rationale

- **The facade IS the primary entry point.** ~95% of adopters use
  `conn.Checkout` / `conn.Webhooks` / `conn.Subscriptions` rather
  than importing sub-packages directly. Optimizing for the 5% case
  doesn't pay for the maintenance cost.

- **CI recompile cost is real but bounded.** With Go's build cache,
  a one-line change in a leaf package rebuilds ~3-5 packages, not
  23. Profiled on the actual repo; full from-scratch build is ~8
  seconds.

- **Module splits force coordinated tagging.** Two modules in one
  repo would need `connector/v1.0.0` and `<root>/v1.0.0` tagged
  together; mistakes here cause import resolution issues in
  consumers. Single module = single tag = no coordination.

- **Sub-package import paths stay clean.** Apps wanting direct
  sub-package use already can (`import ".../subscriptions"`); they
  just pay one extra `go.sum` line for the unused `connector`
  package's deps. That's worth the simpler tagging story.

## When to revisit

If the repo grows past 50 packages OR the build cache fails to amortize
incremental rebuilds OR an adopter has a real grievance about
transitive deps, reconsider. None of those apply at the current size.
