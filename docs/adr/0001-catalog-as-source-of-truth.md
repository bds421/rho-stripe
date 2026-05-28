# ADR-0001: Catalog declared in code is the source of truth; sync pushes to Stripe

- **Status:** Accepted
- **Date:** 2026-05-26
- **Deciders:** Markus

## Context

Every Stripe integration faces the same primary friction: synchronizing product/price definitions between application code and the Stripe dashboard. Without an opinion on this, teams develop a manual loop:

1. Engineer or PM opens Stripe dashboard → creates a Product → creates a Price.
2. Copies the generated `price_NXxxx` ID.
3. Pastes it into app code or an env var.
4. Deploys.
5. Repeats for every price change, every environment (test/prod), every new product, every new app.

This loop has several failure modes that this lib exists to eliminate:

- **No version control.** Dashboard edits leave no diff, no PR review, no rollback.
- **Environment drift.** Test and prod catalogs diverge silently when one is edited but not the other.
- **Manual transcription errors.** Copy-pasted IDs get corrupted; the bug only surfaces at checkout time.
- **No reproducibility.** A fresh test account can't be recreated from any source; engineers re-do the dashboard work each time.
- **Cross-app inconsistency.** Multiple apps under one Stripe account drift in naming conventions, tax categories, and discounting policy.

## Decision

**The catalog is declared in Go code. Stripe state is reconciled to match the declaration via a sync CLI.**

```go
var Catalog = connector.MustCatalog(connector.CatalogSpec{
    Products: map[string]connector.Product{
        "pro_plan": {
            Name:        "Pro Plan",
            TaxCategory: connector.TaxCategorySaaS,
            Prices: map[string]connector.Price{
                "monthly_eur": {Amount: 4900, Currency: "eur", Interval: "month"},
                "yearly_eur":  {Amount: 49000, Currency: "eur", Interval: "year"},
                "monthly_usd": {Amount: 5400, Currency: "usd", Interval: "month"},
            },
        },
        "credit_pack_1000_ai": {
            Name:        "1000 AI Credits",
            TaxCategory: connector.TaxCategorySaaS,
            Prices: map[string]connector.Price{
                "default": {Amount: 1000, Currency: "eur", Type: "one_time"},
            },
            CreditGrant: &connector.CreditGrant{
                Bucket: "ai", Amount: 1000, ValidDays: 90,
            },
        },
    },
    Coupons: map[string]connector.Coupon{
        "SAVE20": {PercentOff: 20, Duration: "once"},
    },
})
```

Sync (`rho-stripe sync --apply`) reconciles Stripe to match this spec — creating, updating (where Stripe permits), or archiving Stripe objects as needed. Sync is **idempotent**: re-running it with the same spec produces no changes.

**The Stripe dashboard becomes a read-only window into state**, not an editing surface. Manual dashboard edits are detected as drift on the next sync (see [[adr-0003]]).

## Options considered

### Option A: Catalog in code, synced to Stripe (chosen)

- ✅ Version controlled. Every price change is a code change, reviewable in a PR.
- ✅ Environment parity: same spec syncs to test and prod, producing equivalent (logically identical) catalogs.
- ✅ Reproducible: fresh test account = `git clone && rho-stripe sync --apply` away.
- ✅ Cross-app consistency: catalogs live next to each other in the same lib type; conventions naturally homogenize.
- ✅ No copy-pasting `price_xxx` IDs anywhere — apps refer to logical keys via [[adr-0003]] (lookup_keys).
- ❌ Deployment pipeline must run sync before app start, or fresh-deployed code references prices Stripe doesn't have yet. Mitigation: sync is idempotent and fast; standard practice is "sync as a deploy step before container restart."

### Option B: Stripe dashboard as truth, app references by lookup_key — rejected

- Apps would still need lookup_keys, but no one would own catalog evolution in code.
- Reverts to the manual-loop pain this lib exists to eliminate.
- No version control over price changes, no PR review, no rollback story.

### Option C: External database as joint source of truth (e.g. dedicated billing-catalog DB) — rejected

- Adds infrastructure: a third state surface besides code and Stripe.
- The DB would need its own management UI, migration tooling, access control.
- Catalog changes become DB writes, not code changes — losing the PR-review benefit.
- Appropriate for very large orgs with non-technical billing teams; unnecessary for our scale.

## Consequences

### Positive

- Catalog changes flow through the standard development process: branch → PR → review → merge → deploy.
- Code review catches pricing typos, tax-category mistakes, missing currencies before they reach customers.
- New environments stand up via one command; CI can spin up fresh Stripe test accounts and sync against them for integration testing.
- Apps under one Stripe account inherit a consistent catalog structure because they're all declared with the same types.
- Disaster recovery: if Stripe-side data is lost or corrupted, sync rebuilds it.

### Negative / accepted risks

- **Deployment ordering matters.** New code that references new prices will fail at runtime if sync hasn't run yet. Mitigation: sync runs as a pre-deploy step in CI/CD; the eager startup cache (per [[adr-0003]]) fails fast at app boot if any declared key is missing in Stripe — a clear, immediate error rather than a runtime surprise.
- **Manual dashboard edits are discouraged.** Some operators are used to "just changing a price in the dashboard." Mitigation: sync detects drift on every run; ops doc covers the policy ("if you must edit in the dashboard, also update the spec in code and re-sync").
- **The spec's expressiveness has limits.** Some Stripe features (custom invoice fields, complex tax behavior overrides, custom Connect flows) aren't in the spec because they're not in scope. Apps that need such features fall back to direct Stripe API calls via `conn.RawClient()`.

### Lib API implications

- `connector.CatalogSpec` is the declarative type. Apps construct it once at module init and pass it to `connector.New(...)`.
- `connector.MustCatalog(spec)` validates at startup (panics on invalid spec — catches typos, missing fields, illegal combinations like CreditGrant on a recurring Price).
- The sync CLI is shipped as part of the lib (`cmd/rho-stripe`); it consumes the same spec the app uses, ensuring sync and runtime agree on what's declared.

## Out of scope (defer to other docs)

- Specific sync algorithm (create/update/archive rules) → the relevant package GoDoc.
- Per-environment configuration (test vs prod credentials, dry-run flags) → the relevant package GoDoc.
- Drift detection semantics → covered in [[adr-0003]].
- Migration of existing subscribers on price change → [[adr-0003]] (auto-grandfathering); migration helper covered in the relevant package GoDoc.
