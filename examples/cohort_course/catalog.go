// Package cohort_course demonstrates the "buy-once, access for a
// window" pattern common in cohort-based education products and
// time-limited content access.
//
// The pattern:
//   - One-time purchase grants 1 "access" credit with ValidDays=N.
//   - The grant naturally expires at the end of the access window;
//     no app-side cron is needed.
//   - The app gates access via `conn.Credits.HasAccess(ctx, subject, "course_access")`
//     — true while a non-expired grant remains, false after expiry.
//
// Why credits + ValidDays instead of "purchase date + N":
//   - The library already handles expiry, audit trail, and per-tenant
//     isolation. Apps don't need to write a custom "is this still valid?"
//     query against a purchase-date column.
//   - Refunds / disputes can revoke the grant atomically.
//   - Multiple cohorts (Q1 cohort + Q2 cohort) deposit independent
//     grants in the same bucket; HasAccess returns true while either
//     window is open.
//
// Run:
//
//	set -a; source .env; set +a
//	go run ./examples/cohort_course/main sync --apply
package cohort_course

import "github.com/bds421/rho-stripe/catalog"

// AccessBucket is the credits bucket name course access is tracked in.
const AccessBucket = "course_access"

// Spec returns the example catalog. Each cohort is its own Product so
// it has its own price + name + dashboard visibility, but the grants
// all land in the same AccessBucket — the app cares about "do you
// have access" not "which cohort did you buy."
func Spec() *catalog.Spec {
	return catalog.MustSpec(catalog.Spec{
		Namespace: "cohort_demo",
		Products: map[string]catalog.Product{
			"cohort_q1_2026": {
				Name:        "Building B2B SaaS — Q1 2026 cohort",
				Description: "12 weekly live sessions + 6 months recording access",
				TaxCategory: catalog.TaxCategoryDigitalBookDownloadPermanent,
				CreditGrant: &catalog.CreditGrant{
					Bucket:    AccessBucket,
					Amount:    1,   // single access token
					ValidDays: 180, // 6 months
				},
				Prices: map[string]catalog.Price{
					"default": {
						Amount: 49900, Currency: "eur", // €499 cohort fee
						Type: catalog.PriceTypeOneTime,
					},
				},
			},
			"cohort_q2_2026": {
				Name:        "Building B2B SaaS — Q2 2026 cohort",
				Description: "Spring cohort, same format",
				TaxCategory: catalog.TaxCategoryDigitalBookDownloadPermanent,
				CreditGrant: &catalog.CreditGrant{
					Bucket:    AccessBucket,
					Amount:    1,
					ValidDays: 180,
				},
				Prices: map[string]catalog.Price{
					"default": {Amount: 49900, Currency: "eur", Type: catalog.PriceTypeOneTime},
				},
			},
			"lifetime_access": {
				Name:        "Lifetime access — all current + future cohorts",
				Description: "One-time purchase, no expiry",
				TaxCategory: catalog.TaxCategoryDigitalBookDownloadPermanent,
				CreditGrant: &catalog.CreditGrant{
					Bucket:    AccessBucket,
					Amount:    1,
					ValidDays: 0, // 0 = never expires
				},
				Prices: map[string]catalog.Price{
					"default": {Amount: 199900, Currency: "eur", Type: catalog.PriceTypeOneTime},
				},
			},
		},
		Coupons: map[string]catalog.Coupon{
			"EARLY_BIRD_20": {
				PercentOff: 20,
				Duration:   catalog.CouponDurationOnce,
			},
		},
	})
}
