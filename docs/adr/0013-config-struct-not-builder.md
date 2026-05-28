# ADR-0013 — `Config` struct over functional-options builder

Date: 2026-05-27 (slice 54 review)
Status: Accepted

## Context

`connector.Config` has ~40 fields. Two common Go API shapes:

1. **Struct literal**: `connector.New(ctx, connector.Config{A: 1, B: 2, ...})`
2. **Functional options**: `connector.New(ctx, WithA(1), WithB(2), ...)`

The current shape is (1). Some reviewers prefer (2) for discoverability
(IDE autocomplete shows option function names; struct fields are less
discoverable to a newcomer).

## Decision

Keep the `Config` struct. Do **not** convert to a functional-options
builder.

## Rationale

- **Validation in one place.** `validateConfig(cfg)` runs once at the
  top of `New`. With options, each option func would have to validate
  its own field (or all validation moves to the end), and inter-field
  consistency checks (`AppNamespace == Catalog.Namespace`) are awkward.

- **Struct is grep-friendly.** "Find every test that uses
  `WebhookEventLog`" is one grep. With options, the call sites read
  `WithEventLog(myLog)` — same intent, less greppable on the field
  name.

- **Composition.** Apps that share config across multiple environments
  (dev / staging / prod) can build a base `Config{}` and assign
  per-env overrides. With options, the equivalent is a slice of
  `Option`s — also fine but more code.

- **Go's struct-literal syntax IS discoverable in modern editors.**
  gopls completes field names with their docstrings; the discovery
  argument was strong when functional options arose (~2014) but
  weaker today.

## Trade-offs accepted

- A new optional field is a non-breaking change BUT it changes the
  struct's zero-value semantics. The library handles this by making
  zero-value mean "use default" everywhere (e.g. `WarmTimeout == 0`
  → `DefaultWarmTimeout`).

- Required fields aren't enforced at compile time. `validateConfig`
  catches them at construction; a typed builder could enforce at
  compile time but at the cost of much more API surface.

## When to revisit

If the `Config` struct grows past ~80 fields, or if we add config
fields that have non-trivial validation dependencies on other fields,
revisit. Neither applies today.
