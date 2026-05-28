# fixtures/

Stripe Fixtures CLI inputs for seeding test-mode accounts to a known
baseline state. Useful for new contributors and CI fresh-account
setup.

## Apply

```bash
set -a && source .env && set +a
stripe fixtures fixtures/baseline.json --api-key "$STRIPE_SECRET_KEY"
```

Re-applying is safe-ish: products and prices with explicit IDs will
fail with "id already taken" on the second run, but coupons (because
of the explicit IDs) will too. To reset cleanly: archive the products
in the dashboard first, or use a fresh test account.

## What `baseline.json` creates

Equivalent state to running `rho-stripe sync --apply` against
the `cmd/example-sync` catalog:

- Product `prod_scdemo_pro_plan` ("Demo Pro Plan")
  - Price `scdemo.pro_plan.monthly_eur` — €49.00/month
  - Price `scdemo.pro_plan.yearly_eur` — €490.00/year
- Product `prod_scdemo_credit_pack_1000_ai` ("1000 Demo AI Credits")
  - Price `scdemo.credit_pack_1000_ai.default` — €10.00 one-time
- Coupon `SAVE20_scdemo` — 20% off once
- Coupon `WELCOME500EUR_scdemo` — €5.00 off once

All objects carry `metadata.app_namespace = "scdemo"` so the webhook
namespace filter routes their events to the demo handler.

## Differences vs `rho-stripe sync`

- `sync` is the production path: declared in Go code, idempotent,
  detects drift, runs UPDATE/REPLACE/ARCHIVE.
- `fixtures` is a "one-shot bootstrap" path: creates everything once,
  intended for fresh accounts.

Real apps never use fixtures in production; they declare a catalog in
Go and run sync.
