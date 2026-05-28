# ADR-0005: Credit ledger owned by the app; Stripe is only the cashier

- **Status:** Accepted (amended)
- **Date:** 2026-05-26
- **Deciders:** Markus
- **Amended by:** [[adr-0011]] (2026-05-26) — concurrency model uses `pgadvisory.Locker.AcquireTx(ctx, hash(subjectID))` from rho-kit's `data/lock/pgadvisory/v2`; audit trail may optionally use rho-kit's `data/actionlog` (chained append-only log) for tamper evidence. The ledger schema and FIFO algorithm are unchanged.

## Context

The charge model ([[project-charge-model]]) requires support for prepaid credits with expiry: customers buy N credits, valid for M days, deducted as they're used. Multi-bucket variants exist (e.g. separate API credits and AI credits per customer).

Stripe has **no native concept of "credits."** Stripe can take the customer's money for a "1000 credits" one-time purchase, but it has zero opinion about:

- What a credit is (a count? a dollar value? a token?).
- How a credit is deducted (which deduction-recording semantics, who decides, when).
- When credits expire (Stripe Invoices have due dates; credits don't).
- How partial use, refunds, expiry, or grant top-ups affect the balance.
- How to display balance to the customer.

This is a domain model that lives entirely outside Stripe. The lib must own it.

## Decision

**The credit ledger is implemented in the integrating app's database via a `CreditRepo` interface defined by the lib. Stripe is the cashier (one-time payment for credit packs); the lib + app DB own everything else.**

### Data model

Two related tables (sketch; full schema in the relevant package GoDoc):

```
credit_grants
─────────────
id              UUID PK
subject_id      TEXT NOT NULL          -- per [[adr-0002]]
bucket          TEXT NOT NULL          -- 'api', 'ai', 'storage', ...
amount_initial  BIGINT NOT NULL        -- granted amount in smallest unit
amount_remaining BIGINT NOT NULL       -- decrements as deducted, never negative
granted_at      TIMESTAMPTZ NOT NULL
expires_at      TIMESTAMPTZ NULL       -- null = never expires
source          TEXT NOT NULL          -- 'stripe_payment' | 'admin_grant' |
                                        -- 'signup_bonus' | 'refund' | ...
source_ref      TEXT                   -- 'pi_NXxxx' for stripe sources

credit_deductions
─────────────────
id              UUID PK
subject_id      TEXT NOT NULL
bucket          TEXT NOT NULL
grant_id        UUID NOT NULL REFERENCES credit_grants(id)
amount          BIGINT NOT NULL
reason          TEXT NOT NULL          -- 'api_call', 'ai_completion', ...
request_id      TEXT NOT NULL          -- idempotency
created_at      TIMESTAMPTZ NOT NULL
UNIQUE (subject_id, request_id)        -- enforces idempotency
```

### Deduction semantics: FIFO by expiry

When deducting N units:

1. Lock all non-expired grants for (subject, bucket) with `amount_remaining > 0`, ordered by `expires_at ASC NULLS LAST, granted_at ASC`.
2. Walk grants, deducting up to `amount_remaining` from each until N is satisfied.
3. Insert a `credit_deductions` row referencing the source grant for audit.
4. If grants run out before N is satisfied, the deduction fails (insufficient credit) — no partial deductions persisted.

FIFO-by-expiry means soonest-to-expire credits are used first. Customers don't lose credits they paid for to expiry while older, longer-lived credits sit unused.

### Expiry

A scheduled job (`conn.Credits.RunExpiry(ctx)`) marks `amount_remaining = 0` for grants past `expires_at`. The app schedules this (cron, k8s CronJob, etc.) — the lib does not run background timers.

Expired remainder is preserved in the grant row for audit (`amount_initial - sum(deductions) = expired amount`); not deleted, not refunded.

### Multi-bucket support

`bucket` is a free-form string set per grant. Apps using separate API and AI credit pools declare them as different buckets at grant time (typically driven by the catalog's `CreditGrant.Bucket` field on the purchased product). Balance queries are per-bucket: `conn.Credits.Balance(ctx, subjectID, "ai")`.

### Catalog-driven auto-grant

The catalog can declare a `CreditGrant` on a one-time Price:

```go
"credit_pack_1000_ai": Product{
    Prices: map[string]Price{
        "default": {Amount: 1000, Currency: "eur", Type: "one_time"},
    },
    CreditGrant: &CreditGrant{
        Bucket: "ai", Amount: 1000, ValidDays: 90,
    },
},
```

When the webhook for `checkout.session.completed` fires for this product, the lib **automatically** calls `CreditRepo.GrantCredit(...)` with the appropriate values, sourcing from the Stripe payment ID. The app does not write per-product credit-granting webhook handlers.

### Concurrent deduction safety

Two requests deducting from the same subject's credits concurrently must not produce a negative balance. The lib uses **per-subject advisory locking** in the reference Postgres impl (PostgreSQL `pg_advisory_xact_lock(hashtext(subject_id))`) inside the deduction transaction. SQLite and other backends use their native equivalents. Detailed comparison in the relevant package GoDoc.

### Refund handling on subscription cancellation

If a customer cancels a subscription that included a credit grant (e.g. "Pro Plan includes 100 credits/month"), the lib does NOT automatically claw back unused credits. The default is "credits granted are kept." Apps that want clawback semantics implement it explicitly via `conn.Credits.RevokeGrant(grantID, reason)`. Reasoning: revocation is a customer-relations decision, not a billing-mechanic one.

## Options considered

### Stripe-native credit modeling — not available

Stripe has no first-class credit concept (as of API version current at decision time). The closest primitives are:

- **Customer balance:** a per-Customer credit balance applied to invoices, but unidirectional (you can credit, customer can't "use" it as quota; expiry not supported).
- **Promotional credits via coupons:** discount future purchases, but not a quota model.
- **Stripe-issued gift cards:** not applicable here.

None of these match "prepaid credit pool with expiry and deduction-on-use." Modeling in app is the only option.

### App ledger with FIFO-by-expiry (chosen) — accepted

Most natural model for "credits expire, oldest first." Matches customer intuition ("use my soon-to-expire ones first"). Audit trail is clean (every deduction references a specific grant).

### App ledger with LIFO or pro-rata — rejected

LIFO ("newest first") means customers' soon-to-expire credits get wasted, which is hostile to them. Pro-rata across all grants (deduct equally from each) is mathematically tidy but produces a confusing audit trail. FIFO-by-expiry is what customers expect and what mature systems use.

### Single-bucket only vs multi-bucket — chose multi-bucket

Single-bucket is simpler but locks out the "separate API and AI credits" requirement explicitly stated in [[project-charge-model]]. Multi-bucket adds a string column; cost is negligible.

### Advisory locks vs row locks vs optimistic locking for concurrency — chose advisory locks for Postgres

Detailed in the relevant package GoDoc; advisory locks scoped per subject avoid row-lock contention on the grants table and are dead simple. Other backends use their native equivalents.

## Consequences

### Positive

- Credit semantics are fully under app/lib control — no Stripe-side limitations to work around.
- Multi-bucket support is first-class; adding a third bucket (e.g. "storage credits") is a string change in the catalog.
- Catalog-driven auto-grant eliminates per-product webhook code.
- Audit trail is complete: every deduction references its source grant, every grant references its source (Stripe payment, admin grant, etc.).
- Expiry is enforced via the scheduled job; not a runtime-overhead concern on the hot path.

### Negative / accepted risks

- **Credit-deduction failures (insufficient balance) are an app-level concern.** The lib returns "insufficient credit" cleanly; the app must decide what to do (block the action, prompt to buy more, fall back to paid usage). Documented as a required app-level handler.
- **Concurrent high-rate deduction on the same subject is rate-limited by the per-subject advisory lock.** Realistic apps deducting 1 credit per request will hit ~1000-5000 deductions/sec/subject sustainable — far above typical workloads. Bulk deductions (e.g. "deduct 500 credits at once for a large operation") are faster than 500 individual deductions and recommended.
- **Refund clawback is not automatic.** Apps that need it must implement deliberately; documented as a policy decision.
- **Ledger growth.** A high-volume credit-using app accumulates many `credit_deductions` rows. The reference Postgres schema includes archival guidance (rollup-and-prune after N months) in the relevant package GoDoc.

### Lib API implications

- `connector/credits` exposes `Grant`, `TryDeduct`, `Balance`, `RevokeGrant`, `RunExpiry`, `History`.
- `CreditRepo` interface defines storage contract.
- Catalog's `CreditGrant` field auto-wires the grant flow via the webhook dispatcher.
- Idempotency on `(subject_id, request_id)` is enforced by the lib — apps must pass a stable `request_id` per logical deduction.

## Out of scope (defer to other docs)

- Full schema with indexes, partitioning options, archival policy → the relevant package GoDoc.
- Concurrency strategy comparison (advisory lock vs row lock vs optimistic) with benchmarks and recommendations → the relevant package GoDoc.
- Bulk-grant operations (admin-grant 1000 credits to N customers) → the relevant package GoDoc.
- Reporting helpers (revenue-recognition deferral for unused credits) → not in phase 0; finance integration is out of lib scope.
- Interaction with metered billing for the same product (mutually exclusive enforcement at sync time) → the relevant package GoDoc and the relevant package GoDoc.
