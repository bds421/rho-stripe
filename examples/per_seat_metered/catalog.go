// Package per_seat_metered demonstrates the modern B2B SaaS shape:
// a per-seat base subscription PLUS metered API consumption on top
// (think Linear, Sentry, Datadog).
//
// The customer signs up for N seats; each seat is €15/month. Their API
// calls accumulate during the cycle and bill as a separate metered
// line item on the same invoice. The library hides the multi-item
// subscription mechanics behind:
//
//   - **conn.Subscriptions.SetSeats / AddSeats** for headcount changes
//     (with Stripe-side fallback when the mirror hasn't seen the sub yet).
//   - **conn.Metering.Record + ReconcileToStripe** for the metered
//     API counter (hourly push aggregated by hour).
//
// Run:
//
//	set -a; source .env; set +a
//	go run ./examples/per_seat_metered/main sync --apply
package per_seat_metered

import "github.com/bds421/rho-stripe/catalog"

// APICallsMetric is the metering event name the app calls Record with.
const APICallsMetric = "api_calls"

// Spec returns the example catalog.
func Spec() *catalog.Spec {
	return catalog.MustSpec(catalog.Spec{
		Namespace: "perseat_demo",
		Products: map[string]catalog.Product{
			"team_plan": {
				Name:        "Team Plan",
				Description: "€15/seat/month + €0.001/API call after 100K free",
				TaxCategory: catalog.TaxCategorySaaSBusiness,
				Metadata: map[string]string{
					"shape":              "per_seat_with_metered_overage",
					"included_api_calls": "100000",
				},
				// Free 100K calls/month per subscription (no per-seat scaling)
				// granted via RecurringGrant. App deducts from this bucket
				// before reaching for the metered overage line.
				RecurringGrant: &catalog.RecurringGrant{
					Bucket:             "api_calls_included",
					Amount:             100000,
					ValidDaysFromGrant: 31,
				},
				Prices: map[string]catalog.Price{
					"per_seat_monthly_eur": {
						Amount: 1500, Currency: "eur", // €15 / seat / month
						Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth,
					},
					"per_seat_yearly_eur": {
						Amount: 15000, Currency: "eur", // €150 / seat / year (~17% off)
						Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalYear,
					},
					"api_calls_overage_eur": {
						Amount: 1, Currency: "eur", // €0.001 per call → 1 cent per 10 calls; smallest-unit price = 1 cent rounding shown for clarity
						Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth,
						MeterRef: "api_calls_meter",
					},
				},
			},
		},
		Meters: map[string]catalog.Meter{
			"api_calls_meter": {
				DisplayName: "API calls (over the included 100K)",
				EventName:   "api_call",
				AggregateBy: "sum",
			},
		},
		Coupons: map[string]catalog.Coupon{
			"ANNUAL_DISCOUNT_15": {
				PercentOff:     15,
				Duration:       catalog.CouponDurationRepeating,
				DurationMonths: 12,
			},
		},
	})
}
