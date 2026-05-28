# ADR-0006: Webhook idempotency, signature verification, and dispatch model

- **Status:** Accepted (amended)
- **Date:** 2026-05-26
- **Deciders:** Markus
- **Amended by:** [[adr-0011]] (2026-05-26) — `EventRepo` is delegated to rho-kit's `idempotency.Store` (Postgres backend: `pgstore.Store`). Race-safe `INSERT ... ON CONFLICT DO NOTHING`, owner-token semantics, and TTL handling come from rho-kit. A separate small `webhook_events` table owned by the lib stores payload + event_type + last_error + attempt_count (fields not first-class in `idempotency.Store`). Signature verification, dispatch, namespace filtering, and failure policy are unchanged.

## Context

Webhooks are the source of truth for "did this thing actually happen in Stripe." Get them wrong and you have silent payment loss, granted-credits-for-no-purchase, or money-handling race conditions. The most common production failure modes in Stripe integrations:

- Missing signature verification → spoofed events trigger real state changes.
- Frameworks auto-parse the request body → signature verification fails on byte mismatch.
- No idempotency on `event.id` → duplicate processing on Stripe retries doubles credits/charges.
- Slow handlers → Stripe timeouts → forced retries → cascade.
- Cross-app event leakage in shared-account setups (see [[adr-0007]]) → app B processes app A's events.
- Failed events silently dropped → audit-trail gap that surfaces months later.

This ADR locks in the lib's webhook contract so apps can plug in without re-deriving these traps.

## Decision

### Endpoint topology

Each app exposes its own webhook URL. Stripe is configured with N endpoints (one per app per environment), each with its own webhook secret. The lib does not implement a "central webhook gateway" — each app's deployment owns its endpoint.

Forced by the architecture (separate apps, separate DBs, separate deployments). No choice to make.

### Signature verification

The lib's `Webhooks.Handle(w, r)` entrypoint:

1. Reads the **raw request body** (bytes, not parsed JSON).
2. Reads the `Stripe-Signature` header.
3. Computes HMAC-SHA256 against the configured webhook secret with the timestamp from the signature.
4. Rejects with HTTP 400 if signature mismatch, timestamp skew > 5 minutes, or body unreadable.

**Raw-body handling is the integrator's responsibility per framework**, but the lib ships adapters/examples for the common cases (net/http, gin, echo, chi, fiber). The integration doc has a section titled "Webhook raw-body gotchas per framework" — this is where 90% of integration bugs land.

### Idempotency via EventRepo

```sql
CREATE TABLE stripe_events (
    event_id        TEXT PRIMARY KEY,
    event_type      TEXT NOT NULL,
    received_at     TIMESTAMPTZ NOT NULL,
    status          TEXT NOT NULL,        -- 'received' | 'skipped' | 'processed' | 'failed'
    last_error      TEXT,
    attempt_count   INT NOT NULL DEFAULT 0,
    processed_at    TIMESTAMPTZ,
    payload         JSONB NOT NULL,
    expires_at      TIMESTAMPTZ           -- nullable; retention pruning
);
CREATE INDEX idx_events_received_at ON stripe_events (received_at);
CREATE INDEX idx_events_status      ON stripe_events (status) WHERE status IN ('received','failed');
CREATE INDEX idx_events_event_type  ON stripe_events (event_type);
```

The dispatcher does an `INSERT ... ON CONFLICT (event_id) DO NOTHING RETURNING ...`:

- 0 rows returned → duplicate delivery → return 200 with no action.
- 1 row returned → new event → proceed to filter + dispatch.

This is race-safe by design: two concurrent deliveries of the same event ID race for the unique constraint; only one wins, the other silently no-ops. No application-level locking needed.

The reference Postgres impl uses `JSONB` for `payload`; SQLite reference would use `TEXT` with the same content. Apps using other stores implement `EventRepo` accordingly.

### Automatic namespace filtering

Every Stripe object the lib creates carries `metadata.app_namespace: <namespace>` (per ADR-0007). The dispatcher reads this from the incoming event:

- Events with a stamped namespace ≠ this app's namespace → marked `skipped`, return 200, no dispatch.
- Events with a matching namespace → dispatch to handler.
- Events without a stamped namespace (e.g. account-level events like `account.updated`) → dispatch only if the app subscribes to that event type via `Handlers.OnAccountEvent`.

Apps never receive events for other apps' subjects. The filter is automatic — apps write handlers as if their app were the only one in the Stripe account.

### Handler dispatch is sync (phase 0)

The HTTP request handler runs the full pipeline inline: verify → dedup-insert → filter → dispatch → mark processed → return 200. All within the Stripe 10-second budget. Handlers are expected to be fast (<2 seconds typical); side-effects that aren't strictly needed for state correctness (emails, analytics, downstream API calls) belong in the app's own background workers, dispatched FROM the handler but executed elsewhere.

**Async dispatch is designed in but not implemented for phase 0.** The handler interface returns `(err error)`; later, an async-mode wrapper can be slotted in (events go to a queue, return 200 immediately, worker drains the queue). No breaking change required.

### Failure policy: retry forever, alert loudly

On handler error:

1. `EventRepo` row updated: `status = 'received'`, `attempt_count++`, `last_error = err.Error()`.
2. Handler returns 500 to Stripe → Stripe retries (up to 3 days).
3. On every retry, dispatcher checks `attempt_count`:
   - `< AlertThreshold` (default 3): proceed silently.
   - `≥ AlertThreshold`: fire `OnEventFailingRepeatedly` hook (configurable; defaults to structured log) and continue retrying.
   - `≥ MaxAttempts` (default 10): still returns 500, still fires alert each time. Lib NEVER swallows events without explicit configuration to do so.

Apps can override `MaxAttempts` to a finite cutoff if they prefer "give up + dead-letter" semantics. The default is "retry forever" because the cost of a few extra retries is much smaller than the cost of a silently lost payment event.

Events stuck in `status='received'` with high `attempt_count` are visible in any standard SQL query — the app's ops/monitoring can build a dashboard or alert on `SELECT count(*) FROM stripe_events WHERE status='received' AND attempt_count > 5`. The lib also exposes `conn.Webhooks.StuckEvents(ctx)` as a convenience.

### Payload retention

The full event payload is stored as JSONB. Default retention is 90 days, configurable. Retention pruning is the app's responsibility (the lib ships a `conn.Webhooks.PruneOldEvents(ctx, before time.Time)` helper that the app schedules via cron or similar). The lib never deletes events automatically.

Reasoning: storing payloads enables replay-from-DB without round-trip to Stripe (which has its own retention horizon), and lets developers debug "what did this event actually contain" months later when the customer asks why their subscription got renewed.

## Options considered

### Event filter: automatic namespace vs manual per-handler vs both — chose automatic

Manual per-handler filtering distributes the same logic across every handler in every app and forces every integrator to learn how to read `event.data.object.metadata`. Both-with-override sounded flexible but adds an escape hatch nobody needed; automatic is sufficient and we can add override later if a real case appears.

### Failure policy: retry forever vs N-then-stop vs per-app — chose retry forever with alert

"N-then-stop" means events get silently dropped after N failures, which means a deployment bug becomes a payments-lost incident. Per-app configurability sounds nice but invites "I'll figure it out later" defaults that bite. Retry-forever with prominent alerting puts the pressure on humans to fix the bug, not on the lib to clean up after the bug.

### Payload storage: full vs minimal vs encrypted — chose full

Minimal saves disk at the cost of every investigation requiring a Stripe API roundtrip. Encrypted-at-rest is a real consideration if the events contain regulated PII; for SaaS billing events (customer email, billing address, payment-method-last-4) this is already standard PII handling — same encryption posture as the rest of the app's DB. App-level DB encryption (pgcrypto column, disk encryption, RDS encryption-at-rest) is the right place for that, not the lib.

### Sync vs async dispatch — chose sync for phase 0, async designed in

Async is real value at scale (high-volume apps, slow handlers, queue-style observability) but requires queue infrastructure. Phase 0 ships sync; the dispatcher interface is shaped so an async wrapper drops in later without changing handler signatures.

## Consequences

### Positive

- Race-safe idempotency via DB unique constraint — no application-level locking, no Redis dependency, no distributed-lock service.
- Apps never see events for other apps' subjects → handler code stays focused.
- Spoofed events impossible without the webhook secret → no out-of-band trust required.
- Stuck/failing events are queryable in app's own DB → standard monitoring tools apply.
- Replay-from-DB enables debugging without Stripe-side API limits.

### Negative / accepted risks

- Stripe retries forever (3 days) on persistent app errors → if an event truly cannot be processed (e.g. customer was deleted), the alert fires every retry. Acceptable; the alternative is silent loss.
- Each app per environment requires its own webhook endpoint configured in Stripe → operational checklist when standing up a new app. The integration doc has a "new app setup" checklist.
- Raw-body handling per framework is a known integration trap → mitigated by per-framework examples in the doc and a startup-time self-test (`conn.Webhooks.TestSignatureVerification(...)`) that apps can call in init to confirm their HTTP plumbing preserves raw bytes.

### Lib API implications

- `connector.Config` requires `WebhookSecret` and `AppNamespace`.
- `connector.Handlers` is a struct with typed function fields per event family (e.g. `OnSubscriptionCreated`, `OnInvoicePaid`, `OnCheckoutCompleted`, `OnAccountEvent`). Unset handlers mean "lib still marks the event processed but doesn't invoke any app code."
- `conn.Webhooks.Handle(http.ResponseWriter, *http.Request)` is the main entrypoint; framework-specific adapters are thin wrappers.
- `conn.Webhooks.StuckEvents(ctx)`, `conn.Webhooks.ReplayEvent(ctx, eventID)`, `conn.Webhooks.PruneOldEvents(ctx, before)` are operational helpers.

## Out of scope (defer to other docs)

- Per-framework raw-body adapter implementations → the relevant package GoDoc design doc.
- Async dispatch implementation → phase 2+.
- Encrypted-at-rest payload storage → app-level DB concern, not lib.
- Subscription state-machine transitions triggered by webhook events → the relevant package GoDoc.
- Credit grant triggered by `checkout.session.completed` → the relevant package GoDoc.
