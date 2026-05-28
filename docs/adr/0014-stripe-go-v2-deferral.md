# ADR-0014: Defer stripe-go V2 surface migration

**Status:** Accepted (2026-05-28). Trigger to revisit: an adopter
needs to handle events from a Stripe V2 resource (Meters, Event
Destinations, etc.) or to ingest meter events at >10 req/s sustained.

## Context

Slice 57 modernized rho-stripe onto stripe-go's V1 service surface
(`*stripe.Client.V1Customers`, `V1Subscriptions`, etc.) — that work
cleared 171 deprecation warnings and is shipped in v0.1.0.

stripe-go v82 also exposes a parallel V2 surface for a small number of
newer Stripe products:

| Surface | What it is |
|---|---|
| `V2BillingMeterEvents.Create` | Single meter-event ingest with `time.Time` timestamps (vs V1's `int64`) |
| `V2BillingMeterEventStreams.Create` | High-throughput batched meter ingest (up to 10k req/s; requires a 15-minute auth session) |
| `V2BillingMeterEventAdjustments` | Adjust previously-sent meter events |
| `V2CoreEvents` | **Thin Events** delivery: webhook body is a tiny notification, full payload fetched on demand from `/v2/core/events/{id}` |
| `V2CoreEventDestinations` | Declarative API for managing webhook destinations + per-destination event filtering |

Question: should rho-stripe migrate any of these for v0.1.x?

## Decision

**Defer all V2 surface adoption until a real adopter need surfaces.**

## Rationale

### Thin Events (`V2CoreEvents`) — not relevant today

Per Stripe documentation: **Stripe currently emits thin events only
from V2 endpoints and resources.** Every V1 resource — Customer,
Subscription, Invoice, Charge, PaymentIntent, SetupIntent — continues
to deliver full-payload events. rho-stripe overwhelmingly handles V1
resources (subscriptions, invoices, customers, checkouts, charges,
payment methods, disputes); none of those produce thin events.

The only way thin events would enter rho-stripe's webhook surface is
if an adopter subscribed to V2-resource events directly (e.g.
`v2.core.event_destination.ping`, `v2.billing.meter_event_summary.flushed`).
We have no such adopter today, and the events themselves carry no
information that V1 webhooks don't already surface for the same
underlying business event.

The cost of migrating proactively is non-trivial:
- The Thin Events contract is different (notification + on-demand
  fetch vs full-payload-in-body). The lib's `webhooks` package would
  need a parallel dispatch path with its own dedup semantics and
  typed handler surface.
- Existing apps depending on `webhooks.Event.Raw.Data.Raw` (the full
  payload byte slice) would need a deprecation cycle.
- The fetch-on-demand step adds a Stripe API round-trip per event,
  with its own retry / circuit-breaker behavior to think about.

The benefit is hypothetical: smaller webhook bodies (already <100KiB
in practice; bandwidth is not the bottleneck) and per-destination
filtering (which our `webhooks` package already does via namespace
metadata).

### V2 Meter Events — same rationale, plus an active-strategy mismatch

`V2BillingMeterEvents.Create` is a functional twin of
`V1BillingMeterEvents.Create` with a typed-timestamp argument. The
migration is mechanical but the win is marginal — neither performance
nor correctness changes.

`V2BillingMeterEventStreams.Create` is the genuine V2 upgrade:
batched ingest, designed for very high throughput. It mismatches
rho-stripe's documented metering strategy (ADR-0004: usage tracked
in app DB on the hot path; aggregated and pushed to Stripe
periodically, hourly default). At hourly cadence × any realistic
customer count, throughput never approaches the 10 req/s threshold
that motivates Streams. Adopting Streams would mean carrying the
extra complexity of session refresh + dual auth for zero throughput
gain.

### V2 Core Event Destinations — a new feature, not a migration

`V2CoreEventDestinations` lets apps declaratively manage their webhook
destinations from code (analogous to `portalconfig.Sync`'s declarative
billing-portal management). This is a feature add, not a stripe-go
upgrade — it would belong in a new `webhooks/destinationsync` package
with its own ADR. Deferred until an adopter asks for it.

## Consequences

- **Adopters consume the V1 webhook contract unchanged.** Every
  full-payload event delivered by Stripe today continues to land in
  `webhooks.Event.Raw.Data.Raw` exactly as it did in v0.1.0.
- **The `conn.Stripe` escape hatch (`*stripe.Client`) exposes the V2
  services already** (`conn.Stripe.V2CoreEvents`, `V2BillingMeterEvents`,
  etc.). Apps that need a V2 surface for a one-off can reach in
  directly without waiting for a lib wrapper — the type is part of
  the stable API contract (ADR-0012-equivalent: connector.Stripe
  doc-comment in `connector/connector.go`).
- **The lib's documented Stripe API version is `2025-08-27.basil`.**
  When we eventually adopt V2 surfaces, this version may need to
  advance; that becomes its own breaking-change discussion at the
  time.

## When to revisit

Specifically, revisit V2 adoption when ANY of these become concrete:

1. An adopter has a use case that requires subscribing to a V2-resource
   webhook event (e.g. a Meters product they're shipping that needs
   `v2.billing.meter_event_summary.flushed` for reconciliation).
2. An adopter's metering volume sustainedly exceeds 10 req/s and
   batched ingest becomes a real bottleneck (would also imply
   rethinking ADR-0004's hourly-batch strategy).
3. Adopters routinely want declarative webhook-destination management
   (the third migration item, which is really a new feature).

Until then, the V2 services live behind the `conn.Stripe` escape
hatch — available for one-offs without the lib paying the cost of a
parallel typed surface and a deprecation cycle.

## References

- stripe-go v82 V2 surfaces: `github.com/stripe/stripe-go/v82` (search
  for `V2BillingMeterEvents`, `V2CoreEvents`, `V2CoreEventDestinations`).
- Stripe Thin Events docs: https://docs.stripe.com/event-destinations/thin-events
- ADR-0004 (Usage tracked in app) — the metering strategy that
  V2BillingMeterEventStreams mismatches.
- ADR-0006 (Webhook idempotency) — the contract a Thin Events
  migration would need to extend.
- ADR-0011 (rho-kit foundation) — pattern for "adopt the latest
  surface when there's a reason; defer with a documented trigger
  otherwise."
