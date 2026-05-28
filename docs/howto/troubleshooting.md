# Troubleshooting

Most-common adopter pain points and how to debug them.

## "signature verification failed" on every webhook

**Symptom**: every `/webhook` POST returns 400 with body `signature verification failed`. Stripe Dashboard → Webhooks shows red failed deliveries.

**Cause**: one of:
1. Wrong signing secret (e.g. you copied the LIVE secret into TEST env, or vice versa).
2. Middleware ate the body before `Webhooks.Handle` read it (e.g. body-buffering middleware that doesn't preserve the original bytes).
3. The body got re-encoded (some JSON-marshalling middlewares whitespace-strip).

**Fix**:
- Run `conn.Webhooks.TestSignatureVerification()` at startup. It signs a known body with your configured secret and verifies — fails fast if the secret is wrong.
- Make sure no middleware reads `r.Body` before `Handle`. Specifically check gin's `c.Request.Body` middlewares, echo's `BodyDump`, etc.
- If you're proxying through nginx/caddy: ensure `proxy_pass` doesn't re-encode the body.

## Signing secret rotation: "I deployed the new secret but old events are 400ing"

**Cause**: Stripe takes minutes to switch endpoints over after you create a new secret. During that window Stripe is signing with the OLD secret but you've deployed code expecting the NEW one.

**Fix**: use `Config.AdditionalSigningSecrets` to accept both during rotation:

```go
SigningSecret:            os.Getenv("STRIPE_WEBHOOK_SECRET_NEW"),
AdditionalSigningSecrets: []string{os.Getenv("STRIPE_WEBHOOK_SECRET_OLD")},
```

Remove the old one from `AdditionalSigningSecrets` once Stripe's dashboard shows the new secret being used.

## "api_version mismatch" warnings flooding the logs

**Symptom**: every webhook logs `webhook: stripe.Event.APIVersion (2018-11-08) differs from stripe-go SDK (2025-08-27.basil)`.

**Cause**: your Stripe account's default API version (set when the account was first created) is older than what stripe-go expects. Common for accounts created years ago.

**Fix**: either update the account default in Dashboard → Developers → API → API version, or leave `Config.StrictAPIVersion=false` (the default) so verification proceeds anyway. The lib reads ids + metadata, not deeply-nested fields, so version skew is tolerable for most use cases.

## `drift-check` shows drift on a freshly-synced catalog

**Symptom**: `sync --apply` returned `applied.`, but `drift-check` immediately shows "drift detected" on the same products.

**Causes + fixes**:
- **`OpDrift` (informational)**: the lib reports fields it can't reconcile (Stripe API doesn't allow updating). Read the drift items — if they're all `OpDrift` (not `OpCreate`/`OpUpdate`/`OpReplace`), it's just informational. Apps wanting CI gating should filter those out:
  ```go
  hasActionableDrift := false
  for _, item := range report.Items {
      if item.Op != catalog.OpDrift { hasActionableDrift = true }
  }
  ```
- **Metadata difference**: someone added a metadata key in the dashboard that's not in the spec. Sync removes it on next `--apply`. Add it to `Product.Metadata` in the spec if it should be there permanently.
- **Tax code mismatch**: tax code is **not updatable** on existing Stripe products. Drift will persist until you archive the product + create a new one with the new tax code (which the diff does automatically; just run `--apply`).

## "Tax registration required in {jurisdiction}" during checkout

**Cause**: Stripe Tax is enabled (default) but your account isn't registered to collect tax in the customer's location.

**Fix**:
- Production: register in the relevant jurisdiction via Stripe Dashboard → Tax → Registrations.
- Test mode: same — Stripe Tax in test mode also requires registrations.
- Per-customer override: set `Defaults.AutomaticTax = ptr(false)` on the specific Input to skip tax for that session (B2C-only flow).

## "no such Price: price_xxx" during checkout

**Cause**: the catalog cache is warm with a Price ID that doesn't exist in Stripe anymore. Usually because:
1. Someone manually deleted/archived the Price in Stripe Dashboard.
2. You ran `sync --apply` against a different namespace/account.
3. The cache was warmed once and never refreshed; meanwhile a `sync --apply` (in a different process) created new Prices.

**Fix**:
- Re-warm the cache: call `conn.Catalog.Refresh(ctx, spec)` (or just bounce the process).
- Long-running servers: schedule periodic refresh (every 15min), OR react to a `drift-check` ping with a refresh.

## "Couldn't find the lookup_key: xxx.yyy" during checkout

**Cause**: the spec declares `xxx.yyy` but `sync --apply` was never run, so the Price doesn't exist in Stripe.

**Fix**:
- Run `sync --apply`.
- For test environments, ensure your seed step includes the sync (or your CI deploys it).

## "active="" filter rejected by Stripe (ListProductsByNamespace)" — old issue, should never come back

**Cause**: the `stripeapi.listPricesForProduct` originally sent an empty `active=` filter which Stripe accepted on first sync (no existing products) but rejected on the second. Fixed in slice 4 with a REGRESSION GUARD comment.

If you see this in production: check `stripeapi/backend.go:listPricesForProduct` — it should NOT have a `params.Active = stripe.Bool(...)` line. The empty-bool footgun was removed.

## "pgadvisory: lock timeout" on CreditRepo.TryDeduct

**Cause**: many concurrent Deducts on the same subject contending for the per-subject pg_try_advisory_xact_lock. Lock is held only during the txn but high contention can timeout.

**Fix**:
- Bump the request's context deadline.
- Ensure callers `Deduct` with the smallest possible amount (don't batch 1000 deductions into one call).
- Production load: shard by subject hash across multiple Postgres replicas (lib uses xact-scoped advisory locks, so DB-level partitioning works).

## Live test fails with "meter_already_deactivated"

**Cause**: harmless — your test's `t.Cleanup` tried to archive a meter that the test body's `ArchiveMeter` call had already archived. Stripe's API errors on double-archive.

**Fix**: ignore — the test has already passed. To silence the log line, the test cleanup function can swallow the specific error.

## "AddInvoiceItem requires StripePriceID OR (InlineAmount>0 + Currency + Name)"

**Cause**: you populated `subscriptions.AddInvoiceItem` with neither a `StripePriceID` nor a complete inline definition.

**Fix**: pick one path:
```go
// Catalog reference
{StripePriceID: cache.MustLookup("setup_fee.default")}

// Inline ad-hoc (e.g. enterprise custom quote)
{InlineAmount: 50000, Currency: "eur", Name: "Onboarding fee"}
```

## "TrialDays must be between 1 and 730"

**Cause**: Stripe's hard cap is 730 days (2 years). You passed something outside [1, 730].

**Fix**: clamp to that range. For "trial of less than 1 day" (e.g. 1-hour preview), use a different mechanism (free tier + manual upgrade flow).

## My handler runs twice for the same event

**Cause**: most likely:
1. **Async dispatch mode without `wh.ProcessQueued` as the dispatch**: the queue's worker isn't calling the lib's lifecycle method, so the dedup store never sees "processed." Stripe redelivers → handler runs again. Use `webhooks.NewMemoryQueue(cap, n, wh.ProcessQueued)` exactly.
2. **Multiple replicas without a shared idempotency.Store**: each replica's `MemoryStore` is independent. Use Postgres-backed store (`rho-kit pgstore`).
3. **You restored the database from a backup** that predated some processed events. Acceptable trade-off — the dedup store TTL eventually catches up.

## Subscription mirror is stale after a Migrate

**Cause**: `Migrate` triggers Stripe to send a `customer.subscription.updated` event, but your code read the mirror before the event arrived.

**Fix**: there's an unavoidable window. Two options:
- **Eventual consistency**: tell the user "Plan updated" + show the new plan from the input (not the mirror) until the next page load.
- **Strong consistency**: after Migrate, call `conn.Subscriptions.Repo().GetByStripeID(ctx, id)` in a short poll loop until `StripeUpdatedAt` advances. Typically <1 second.

Don't use the Stripe API directly to re-fetch — that defeats the whole point of the mirror.

## "context deadline exceeded" on Stripe calls

**Cause**: default HTTP timeout is 30s. Some Stripe endpoints (mass list operations) routinely take longer.

**Fix**:
```go
conn, err := connector.New(ctx, connector.Config{
    HTTPTimeout: 60 * time.Second,
    // ...
})
```

For specific slow calls (e.g. bulk reconcile), pass a longer context to the call itself.
