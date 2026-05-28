# Examples

Five runnable example catalogs covering the most common SaaS pricing
shapes, plus a frontend integration demo.

## Catalogs

Each catalog is a self-contained Go package. Two ways to use:

```bash
# 1. Run as a CLI directly against your Stripe test account
set -a; source ../.env; set +a
go run ./examples/saas_tiers/main sync --apply
go run ./examples/saas_tiers/main checkout standard.monthly_eur

# 2. Import the Spec from your own app
import "github.com/bds421/rho-stripe/examples/saas_tiers"
conn, _ := connector.New(ctx, connector.Config{
    Catalog: saas_tiers.Spec(),
    // ...
})
```

| Example | What it demonstrates |
|---|---|
| [`saas_tiers/`](saas_tiers) | Free / Standard / Pro / Enterprise tiers + monthly+yearly + multi-currency + per-tier feature metadata |
| [`credit_topup/`](credit_topup) | One-time credit-pack purchases + auto-refill subscription pattern |
| [`included_quota/`](included_quota) | "60 voice-minutes/month included" + metered overage |
| [`per_seat_metered/`](per_seat_metered) | Per-seat subscription + metered API calls on top |
| [`cohort_course/`](cohort_course) | Buy-once / time-limited access (online courses, training cohorts) |

Pick the one closest to your shape and copy the source into your own
`catalog/` directory. Rename the `Namespace` and adjust pricing.

## Frontend

[`embedded_frontend/`](embedded_frontend) — minimal full-stack demo
of Stripe's embedded Payment Element, served by a tiny Go HTTP
server that returns the `client_secret`. The HTML+Stripe.js snippet
inside is the bit you copy into your own templates.

## Which one is right for my app?

| If you sell… | Start with… |
|---|---|
| A tiered SaaS (Pro/Standard/Enterprise) | `saas_tiers` |
| API calls / per-unit consumption with no monthly base | `credit_topup` |
| Recurring plan with a fixed-quota included benefit (minutes, GB) | `included_quota` |
| Per-user SaaS with usage-based add-ons | `per_seat_metered` |
| Time-limited courses or content cohorts | `cohort_course` |
| Marketplace / split payments | **not supported** — Stripe Connect is out of scope |

## All examples share the same patterns

Every example demonstrates:

- A `Spec` declaration with namespace, products, prices, optional coupons + meters
- A `main.go` that hands the spec to `cli.Main` (so `sync --apply`,
  `diff`, `checkout`, etc. all work out of the box)
- Comments explaining the catalog choices + linking to the relevant
  how-to docs

Read the catalog source first — it's heavily commented and faster
than any tutorial.
