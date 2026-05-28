// Package included_quota demonstrates the "60 minutes of voice
// transcription included this month" pattern — a subscription whose
// per-cycle benefit is a fixed quota of something consumable.
//
// Three tiers + an overage scheme:
//
//   - **Starter** — 60 minutes/month included; no overage allowed
//     (over-limit requests return ErrInsufficientCredit, app prompts
//     upgrade).
//   - **Pro** — 600 minutes/month included; overage billed via a
//     separate metered price at €0.05/minute (uses the metering
//     package: app records each minute, hourly reconcile pushes the
//     count to Stripe).
//   - **Enterprise** — unlimited (no quota grant; app uses the
//     subscription's tier metadata to skip credit deduction entirely).
//
// Who manages what:
//
//   - **Library** owns the per-cycle quota via Product.RecurringGrant.
//     Each invoice.paid grants the configured amount; old credits
//     expire at the next cycle (ValidDaysFromGrant=31) so unused
//     minutes don't accumulate forever.
//   - **App** consumes credits before serving each transcription
//     minute: conn.Credits.Deduct(ctx, subject, "voice_minutes", 1).
//     ErrInsufficientCredit drives the upsell / overage flow.
//   - **App** records overage usage via conn.Metering.Record once
//     credits are exhausted (Pro tier only); the metering package
//     batches + pushes to Stripe per its reconcile schedule.
//
// Run:
//
//	set -a; source .env; set +a
//	go run ./examples/included_quota/main sync --apply
package included_quota

import "github.com/bds421/rho-stripe/catalog"

// VoiceMinutesBucket is the credit bucket name the app deducts from.
const VoiceMinutesBucket = "voice_minutes"

// VoiceOverageMetric is the metering key for over-quota usage.
const VoiceOverageMetric = "voice_minutes_overage"

// Spec returns the example catalog.
func Spec() *catalog.Spec {
	return catalog.MustSpec(catalog.Spec{
		Namespace: "voicequota_demo",
		Products: map[string]catalog.Product{
			"starter": {
				Name:        "Voice Starter",
				Description: "60 minutes/month included. Hard cap — upgrade for more.",
				TaxCategory: catalog.TaxCategorySaaSBusiness,
				Metadata: map[string]string{
					"tier":             "starter",
					"included_minutes": "60",
					"overage_allowed":  "false",
				},
				RecurringGrant: &catalog.RecurringGrant{
					Bucket:             VoiceMinutesBucket,
					Amount:             60,
					ValidDaysFromGrant: 31, // expire at next cycle so unused don't stack
				},
				Prices: map[string]catalog.Price{
					"monthly_eur": {
						Amount: 1900, Currency: "eur", // €19/month
						Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth,
					},
				},
			},

			"pro": {
				Name:        "Voice Pro",
				Description: "600 minutes/month included; overage €0.05/min via metered billing",
				TaxCategory: catalog.TaxCategorySaaSBusiness,
				Metadata: map[string]string{
					"tier":               "pro",
					"included_minutes":   "600",
					"overage_allowed":    "true",
					"overage_metric":     VoiceOverageMetric,
					"overage_unit_cents": "5",
				},
				RecurringGrant: &catalog.RecurringGrant{
					Bucket:             VoiceMinutesBucket,
					Amount:             600,
					ValidDaysFromGrant: 31,
				},
				Prices: map[string]catalog.Price{
					"monthly_eur": {
						Amount: 9900, Currency: "eur", // €99/month base
						Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth,
					},
					// Separate metered price for overage. App records each
					// over-quota minute via conn.Metering.Record(VoiceOverageMetric, 1).
					"overage_eur": {
						Amount: 5, Currency: "eur", // €0.05/min
						Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth,
						MeterRef: "voice_overage_meter",
					},
				},
			},

			"enterprise": {
				Name:        "Voice Enterprise",
				Description: "Unlimited; custom-priced via Sales",
				TaxCategory: catalog.TaxCategorySaaSBusiness,
				Metadata: map[string]string{
					"tier":             "enterprise",
					"included_minutes": "unlimited",
					"contact_only":     "true",
				},
				// No RecurringGrant — app reads metadata.included_minutes
				// and skips the Deduct call when "unlimited".
				Prices: map[string]catalog.Price{
					"custom_yearly_eur": {
						Amount: 1, Currency: "eur", // placeholder, replaced via invoice override
						Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalYear,
					},
				},
			},
		},
		Meters: map[string]catalog.Meter{
			"voice_overage_meter": {
				DisplayName: "Voice transcription overage (minutes)",
				EventName:   "voice_overage", // app calls conn.Metering.Record with this event name
				AggregateBy: "sum",
			},
		},
	})
}
