// Package saas_tiers is a runnable example of the most common SaaS
// pricing shape: Free + Standard + Pro + Enterprise tiers, each with
// monthly and yearly prices (yearly priced for two months free).
//
// What this demonstrates:
//
//   - Free tier as a Stripe product with a $0 recurring price (so
//     access checks via subscription mirroring work uniformly across
//     all tiers — apps never branch on "is the user paying").
//   - Monthly + yearly variants per tier as separate Price objects
//     under the same Product (yearly has its own LookupKey + Amount).
//   - Per-tier feature limits encoded as Metadata so app code can
//     read `product.Metadata["max_seats"]` without hard-coding.
//   - Enterprise tier with "Contact Sales" semantics: no public Price;
//     apps create custom invoices via conn.Invoices.CreateDraft.
//
// Run as a sync:
//
//	set -a; source .env; set +a
//	go run ./examples/saas_tiers
//
// Or import the Spec from your own app:
//
//	import "github.com/bds421/rho-stripe/examples/saas_tiers"
//	conn, err := connector.New(ctx, connector.Config{
//	    Catalog: saas_tiers.Spec(),
//	    ...
//	})
package saas_tiers

import "github.com/bds421/rho-stripe/catalog"

// Spec returns the example catalog. Apps that want to use this as a
// starting point copy the source and rename the namespace.
func Spec() *catalog.Spec {
	return catalog.MustSpec(catalog.Spec{
		Namespace: "saas_tiers_demo",
		Products: map[string]catalog.Product{
			"free": {
				Name:        "Free",
				Description: "Get started with limited features at no cost",
				TaxCategory: catalog.TaxCategorySaaS,
				Metadata: map[string]string{
					"tier":                "free",
					"max_seats":           "1",
					"max_api_calls_mo":    "1000",
					"support":             "community",
					"included_storage_gb": "1",
				},
				Prices: map[string]catalog.Price{
					"monthly": {
						Amount: 0, Currency: "eur",
						Type:     catalog.PriceTypeRecurring,
						Interval: catalog.IntervalMonth,
					},
				},
			},
			"standard": {
				Name:        "Standard",
				Description: "Everything most teams need",
				TaxCategory: catalog.TaxCategorySaaS,
				Metadata: map[string]string{
					"tier":                "standard",
					"max_seats":           "10",
					"max_api_calls_mo":    "50000",
					"support":             "business-hours email",
					"included_storage_gb": "50",
				},
				Prices: map[string]catalog.Price{
					"monthly_eur": {
						Amount: 2900, Currency: "eur", // €29/month
						Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth,
					},
					"yearly_eur": {
						Amount: 29000, Currency: "eur", // €290/year (~16% off → 2 months free)
						Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalYear,
					},
					"monthly_usd": {
						Amount: 3200, Currency: "usd",
						Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth,
					},
					"yearly_usd": {
						Amount: 32000, Currency: "usd",
						Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalYear,
					},
				},
			},
			"pro": {
				Name:        "Pro",
				Description: "Advanced features for growing teams",
				TaxCategory: catalog.TaxCategorySaaS,
				Metadata: map[string]string{
					"tier":                "pro",
					"max_seats":           "50",
					"max_api_calls_mo":    "500000",
					"support":             "priority email + chat",
					"included_storage_gb": "500",
					"sso_enabled":         "true",
					"audit_log_enabled":   "true",
				},
				Prices: map[string]catalog.Price{
					"monthly_eur": {
						Amount: 9900, Currency: "eur", // €99/month
						Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth,
					},
					"yearly_eur": {
						Amount: 99000, Currency: "eur", // €990/year
						Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalYear,
					},
					"monthly_usd": {
						Amount: 10900, Currency: "usd",
						Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth,
					},
					"yearly_usd": {
						Amount: 109000, Currency: "usd",
						Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalYear,
					},
				},
			},
			// Enterprise has NO public Price. App's "Contact Sales" flow
			// produces a custom-priced invoice via conn.Invoices.CreateDraft.
			// The Product still exists so audit reports + drift checks can
			// reason about enterprise customers uniformly.
			"enterprise": {
				Name:        "Enterprise",
				Description: "Custom pricing, dedicated support, contractual SLAs",
				TaxCategory: catalog.TaxCategorySaaS,
				Metadata: map[string]string{
					"tier":              "enterprise",
					"max_seats":         "custom",
					"max_api_calls_mo":  "custom",
					"support":           "dedicated CSM",
					"sso_enabled":       "true",
					"audit_log_enabled": "true",
					"contact_only":      "true",
				},
				Prices: map[string]catalog.Price{
					// One placeholder price so the product can be subscribed
					// to with a custom-quantity invoiced-collection flow:
					"custom_eur": {
						Amount: 1, Currency: "eur", // €0.01 placeholder; actual amount comes from CreateDraft override
						Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalYear,
					},
				},
			},
		},
		Coupons: map[string]catalog.Coupon{
			"NEW_CUSTOMER_20": {
				PercentOff: 20,
				Duration:   catalog.CouponDurationOnce,
			},
			"YEARLY_UPGRADE_10": {
				PercentOff:     10,
				Duration:       catalog.CouponDurationRepeating,
				DurationMonths: 12,
			},
		},
	})
}
