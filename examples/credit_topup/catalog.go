// Package credit_topup demonstrates the "buy a pack of credits, use
// them in-app" pricing pattern (think OpenAI API credits, image-gen
// platforms, voice-clone tools).
//
// Two flavors are shown:
//
//   - **Manual top-up packs**: one-time purchases of N credits at a
//     fixed price (€10 → 1000 credits, €40 → 5000, €150 → 25000). The
//     packs that "scale better" (bigger pack = cheaper per credit) are
//     pure pricing — the lib doesn't impose a per-credit price.
//   - **Auto-refill subscription**: a recurring subscription that
//     grants N credits per cycle via Product.RecurringGrant. When the
//     customer's balance drops below a threshold the app charges them
//     again (handled app-side; this example shows the wiring).
//
// Who manages what:
//
//   - **Library** owns the credit ledger: grants on checkout.session.
//     completed (via credits.ApplyGrantsFromSession), deducts on
//     in-app usage (conn.Credits.Deduct), expires per-grant via
//     conn.Credits.RunExpiry.
//   - **App** owns the "should I let this request through?" decision:
//     before consuming a credit, call conn.Credits.Deduct and react
//     to ErrInsufficientCredit (return 402 / show upsell modal /
//     trigger auto-refill).
//   - **App** owns the auto-refill trigger: watch the balance via
//     conn.Credits.Balance(); when below the threshold, create a
//     checkout session for the refill pack programmatically.
//
// Run:
//
//	set -a; source .env; set +a
//	go run ./examples/credit_topup/main sync --apply
package credit_topup

import "github.com/bds421/rho-stripe/catalog"

// CreditsBucket is the bucket name credits land in. Apps reference
// this when calling Deduct.
const CreditsBucket = "api_credits"

// Spec returns the example catalog.
func Spec() *catalog.Spec {
	return catalog.MustSpec(catalog.Spec{
		Namespace: "credits_demo",
		Products: map[string]catalog.Product{
			"pack_small": {
				Name:        "Credit Pack — 1,000 credits",
				Description: "One-time top-up; credits never expire",
				// API credits are prepaid usage of a SaaS; SaaS-personal is the
				// closest standard category (consumed inside the SaaS service).
				// Apps targeting business customers can switch to
				// catalog.TaxCategorySaaSBusiness.
				TaxCategory: catalog.TaxCategorySaaSPersonal,
				CreditGrant: &catalog.CreditGrant{
					Bucket:    CreditsBucket,
					Amount:    1000,
					ValidDays: 0, // 0 = never expires
				},
				Prices: map[string]catalog.Price{
					"default": {
						Amount: 1000, Currency: "eur", // €10 / 1000 credits = €0.01 each
						Type: catalog.PriceTypeOneTime,
					},
				},
			},
			"pack_medium": {
				Name:        "Credit Pack — 5,000 credits",
				Description: "20% better unit price than the small pack",
				// API credits are prepaid usage of a SaaS; SaaS-personal is the
				// closest standard category (consumed inside the SaaS service).
				// Apps targeting business customers can switch to
				// catalog.TaxCategorySaaSBusiness.
				TaxCategory: catalog.TaxCategorySaaSPersonal,
				CreditGrant: &catalog.CreditGrant{
					Bucket:    CreditsBucket,
					Amount:    5000,
					ValidDays: 0,
				},
				Prices: map[string]catalog.Price{
					"default": {
						Amount: 4000, Currency: "eur", // €40 / 5000 = €0.008 each
						Type: catalog.PriceTypeOneTime,
					},
				},
			},
			"pack_large": {
				Name:        "Credit Pack — 25,000 credits",
				Description: "Best unit price",
				// API credits are prepaid usage of a SaaS; SaaS-personal is the
				// closest standard category (consumed inside the SaaS service).
				// Apps targeting business customers can switch to
				// catalog.TaxCategorySaaSBusiness.
				TaxCategory: catalog.TaxCategorySaaSPersonal,
				CreditGrant: &catalog.CreditGrant{
					Bucket:    CreditsBucket,
					Amount:    25000,
					ValidDays: 0,
				},
				Prices: map[string]catalog.Price{
					"default": {
						Amount: 15000, Currency: "eur", // €150 / 25000 = €0.006 each
						Type: catalog.PriceTypeOneTime,
					},
				},
			},

			// Auto-refill subscription: each successful invoice grants
			// 10000 credits that expire after 30 days. Apps trigger this
			// upsell when the customer's balance crosses a threshold.
			"auto_refill_pro": {
				Name:        "Auto-Refill — 10,000 credits/month",
				Description: "Monthly subscription; credits expire 30 days after grant",
				TaxCategory: catalog.TaxCategorySaaS,
				RecurringGrant: &catalog.RecurringGrant{
					Bucket:             CreditsBucket,
					Amount:             10000,
					ValidDaysFromGrant: 30,
				},
				Prices: map[string]catalog.Price{
					"monthly_eur": {
						Amount: 7900, Currency: "eur", // €79/month for 10k credits
						Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth,
					},
				},
			},
		},
	})
}
