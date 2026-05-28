# ADR-0003: Catalog binding via Stripe `lookup_key` with eager in-memory cache

- **Status:** Accepted
- **Date:** 2026-05-26
- **Deciders:** Markus

## Context

The catalog is declared in Go code (see [[adr-0001]] when written). Stripe assigns its own opaque IDs (`price_NXxxx`) to the Products and Prices created via sync. The lib needs to bridge "logical key in code" → "Stripe ID at runtime" cleanly across multiple environments (test/prod), surviving:

- Environment differences (test and prod have different `price_xxx` for the same logical price).
- Process restarts.
- Stripe-side drift (someone editing in the dashboard).
- Price changes (Stripe Prices are immutable once used).

## Decision

### Binding: Stripe `lookup_key` is the primary identifier

The lib uses Stripe's native `lookup_key` field as the binding mechanism:

- **Prices** get a `lookup_key` set during sync, equal to the namespaced catalog key (e.g. `app1.pro_plan.monthly_eur`).
- **Products** get a custom `id` set to a deterministic value (`prod_app1_pro_plan`). Stripe allows custom Product IDs.
- **Coupons** get a custom `id` set to the catalog key (`SAVE20_app1`).
- **Tax rates** are not bound by the lib — Stripe Tax (per ADR-0009) handles them.

Apps refer to everything by logical key in their code; the lib resolves to actual Stripe IDs as needed.

### Resolution: eager startup warmup, in-memory cache

On `connector.New(...)`, the lib resolves all declared catalog keys to current Stripe IDs in a single batched API call (`stripe.prices.list({lookup_keys: [...]})`, which accepts up to 10 keys per call; the lib batches as needed). The resolved map is held in memory for the process lifetime.

- **Fail-fast on startup:** if Stripe is unreachable or any declared key is missing in Stripe, `connector.New` returns an error. The app fails to start with a clear error message. This is correct for billing-critical apps — silent runtime "price not found" failures during checkout are worse than a loud startup failure.
- **Cache miss after warmup:** treated as a configuration error (a new key was used without a sync). Logged loudly and surfaces as an explicit "catalog key not found" error to the caller, not silently re-resolved. Prevents drift between deployed catalog and Stripe state from masking real bugs.
- **Cache refresh:** explicit via `conn.Catalog.Refresh(ctx)` (e.g. called after a runtime sync); the lib does not auto-refresh on cache miss because doing so would mask the "forgot to re-sync after deploy" failure.

### Sync semantics for changing prices

Stripe Prices are immutable once used in any transaction. When the catalog declares a changed price (amount, currency, interval), sync:

1. Creates a new Stripe Price with `active: true` and the new values.
2. Transfers the `lookup_key` from the old Price to the new one (Stripe API supports this).
3. Sets the old Price to `active: false` (archived).

**Result: grandfathering is automatic.** Existing subscriptions remain on the old Price (Stripe does not auto-migrate). New checkouts route to the new Price via the transferred `lookup_key`.

For the rare case where an app wants to migrate existing subscribers to the new price, the lib offers a separate explicit operation: `conn.Subscriptions.Migrate(ctx, subjectID, fromKey, toKey, prorate bool)`. Not part of sync. Documented as "this affects real customer bills — use with intent."

### Sync semantics for removing catalog entries

If a Product or Price is removed from the catalog code and sync runs:

- Existing Prices for that Product are archived (`active: false`), not deleted. Stripe doesn't allow deletion of Prices that have been used.
- The Product itself is archived (`active: false`) if it has no active Prices remaining.
- Coupons removed from the catalog are deleted (Stripe allows coupon deletion; the lib preserves them via an opt-in `Coupon.Preserve = true` flag if the app wants to keep an old code valid for redemptions in flight).
- Sync logs every archival prominently. Apps that want to prevent accidental archival can run `rho-stripe sync --dry-run` first.

**Sync is never destructive of customer-facing data.** Worst-case, archived items can be re-activated manually in the Stripe dashboard.

### Drift detection: at sync time only

`rho-stripe sync` runs in three phases:

1. **Compare**: fetch current Stripe state for the namespace, diff against the declared catalog.
2. **Report**: print a human-readable plan (create / update / archive) and any drift detected (objects in Stripe with matching namespace prefix but no declaration in code, or vice versa).
3. **Apply**: with `--apply` flag (default is dry-run-style report-only), perform the changes.

Drift items (Stripe-side objects not in catalog) are reported but NOT auto-cleaned — they may be deliberate (a test product, a manual coupon for a one-off). The integrator decides per-item what to do.

Continuous drift detection (a periodic job) is deferred to phase 2+. The sync-time check catches drift on every deploy, which is the dominant case.

## Options considered

### Option A: per-call `lookup_key` resolution (no cache) — rejected

Doubles the Stripe API calls and adds 50-200ms to every checkout. Not acceptable for hot-path billing operations.

### Option B: generated `catalog_ids.go` file — rejected

Forces a "regenerate per environment" build step. Stale files mask Stripe-side drift silently. Adds a generated artifact to commit (or per-env build pipeline). The classic pre-2022 pattern; superseded by lookup_keys for good reason.

### Option C: lookup_key + in-memory cache (chosen)

Best ergonomics, modern Stripe-native pattern, recoverable at runtime (cache rebuild via Stripe API), no generated artifacts to maintain.

### Warmup: eager vs lazy vs hybrid — chose eager

Lazy ("resolve on first use") gives a slow first checkout per process AND hides bad-config issues (typo in lookup_key only surfaces when that specific price is checked out, potentially weeks later). Eager warmup with fail-fast surfaces configuration errors at app startup, when humans are paying attention.

The hybrid option ("eager, fall back to lazy if Stripe down") was tempting for resilience but rejected because it masks the "Stripe is unreachable" condition at startup — which is exactly the moment we want loud failure, not silent degradation. If Stripe is down, the app cannot accept payments anyway; failing to start is correct.

### Audit file: yes vs no — chose no

A committed audit file (`catalog_lockfile.json`) was considered for code-review and disaster-recovery value. Rejected because:
- The catalog declaration in code is already the source of truth, diffable in git.
- Stripe is the source of truth for resolved IDs; the lockfile would just duplicate state and risk going stale.
- Disaster recovery (Stripe data lost) is already covered by the sync command — re-running creates equivalent state.
- The Stripe API call to list all Prices for a namespace prefix gives the equivalent of an audit file on demand, no file maintenance.

### Drift detection: sync-time vs continuous vs none — chose sync-time

Continuous is real value at scale but adds operational surface area (scheduled job, alerting hook, false-positive handling for in-progress changes). Sync-time catches the dominant case (drift between deploys) for zero ongoing cost. Continuous is a phase 2+ enhancement.

## Consequences

### Positive

- Catalog code is environment-independent — same declaration works in test and prod, with Stripe holding the resolved IDs per env.
- Price changes auto-grandfather existing customers via Stripe's Price immutability + lookup_key transfer.
- Bad catalog config (typo, missing sync) fails loudly at startup, not silently mid-checkout.
- Drift detection on every sync catches manual dashboard edits before they cause runtime surprises.
- Zero generated files in the repo — git diff is meaningful, no merge conflicts on auto-generated state.

### Negative / accepted risks

- **Startup depends on Stripe being reachable.** Accepted; a billing-critical app that can't reach Stripe is already broken — failing to start is the correct response, and the error message points clearly at Stripe. The cache itself is small (one map[string]string), so warmup is fast (~200ms typical for tens of keys).
- **Cache is process-local.** Multiple replicas of the same app each pay the warmup cost on start. Acceptable; warmup is one batched API call, well within Stripe's rate limits even for fleet rolling deploys.
- **No automatic migration of existing subscribers on price change.** Intentional; grandfathering is the right default. The explicit `Migrate` operation is documented for the cases where it isn't.
- **Continuous drift detection deferred.** Apps that have multiple dashboard editors and need eventual-consistency monitoring will want this later. The sync command's drift report covers the deployment-driven case.

### Lib API implications

- `connector.New(ctx, config)` returns `(*Connector, error)` — the error case includes catalog-resolution failures.
- `conn.Catalog.Refresh(ctx)` for explicit cache refresh; `conn.Catalog.Lookup(key)` for explicit ID retrieval (mostly internal).
- `conn.Catalog.ValidateKey(key)` (compile-time helper not possible in Go without code generation; runtime helper instead) for app-side validation of dynamic key construction.
- `conn.Subscriptions.Migrate(...)` is the only API that knowingly mutates an existing customer's billing state. Documented as deliberately disruptive.
- The `rho-stripe` CLI is the apply path; the lib never auto-syncs at runtime. Deployment process must run `rho-stripe sync --apply` before the new app version starts (deployment doc / runbook concern).

## Out of scope (defer to other docs)

- The full sync algorithm (compare, report, apply phases; conflict handling; coupon preservation rules) → the relevant package GoDoc.
- The CLI UX of `rho-stripe sync` → the relevant package GoDoc.
- How tax categories on Products are declared and synced → covered in [[adr-0009]].
- Continuous drift detection → phase 2+ enhancement, not designed yet.
