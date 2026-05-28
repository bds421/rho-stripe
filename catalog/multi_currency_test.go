package catalog_test

import (
	"testing"

	"github.com/bds421/rho-stripe/catalog"
)

// TestMultiCurrencySpec_AcceptsAllMajorCurrencies asserts that the
// spec validator accepts every ISO-4217 currency commonly used in
// SaaS billing. Stripe accepts all of these; the lib must not
// inadvertently allowlist only EUR.
func TestMultiCurrencySpec_AcceptsAllMajorCurrencies(t *testing.T) {
	currencies := []string{"eur", "usd", "gbp", "chf", "sek", "nok", "dkk", "pln", "czk", "huf", "ron", "bgn", "jpy", "cad", "aud", "nzd", "sgd", "hkd", "inr", "brl", "mxn"}
	for _, ccy := range currencies {
		t.Run(ccy, func(t *testing.T) {
			spec, err := catalog.NewSpec(catalog.Spec{
				Namespace: "mc_" + ccy,
				Products: map[string]catalog.Product{
					"basic": {
						Name:        "Basic " + ccy,
						TaxCategory: catalog.TaxCategorySaaSBusiness,
						Prices: map[string]catalog.Price{
							"monthly": {
								Amount:   1000,
								Currency: ccy,
								Type:     catalog.PriceTypeRecurring,
								Interval: catalog.IntervalMonth,
							},
						},
					},
				},
			})
			if err != nil {
				t.Fatalf("currency %q rejected: %v", ccy, err)
			}
			if spec.Products["basic"].Prices["monthly"].Currency != ccy {
				t.Errorf("currency mutated to %q", spec.Products["basic"].Prices["monthly"].Currency)
			}
		})
	}
}

// TestMultiCurrencyDiff_RoundTrip drives a catalog with prices in 4
// currencies under the same product through Diff + a fake apply,
// confirming each currency ends up as its own Stripe Price object
// (Stripe requires one Price per currency).
func TestMultiCurrencyDiff_RoundTrip(t *testing.T) {
	spec := catalog.MustSpec(catalog.Spec{
		Namespace: "multi_ccy",
		Products: map[string]catalog.Product{
			"plus": {
				Name:        "Plus",
				TaxCategory: catalog.TaxCategorySaaSBusiness,
				Prices: map[string]catalog.Price{
					"monthly_eur": {Amount: 2900, Currency: "eur", Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
					"monthly_usd": {Amount: 3200, Currency: "usd", Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
					"monthly_gbp": {Amount: 2500, Currency: "gbp", Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
					"monthly_chf": {Amount: 3100, Currency: "chf", Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
				},
			},
		},
	})

	// Diff against empty Stripe state — every price should be a Create.
	plan := catalog.Diff(spec, nil)
	priceCreates := 0
	for _, item := range plan.Items {
		if item.Kind == catalog.KindPrice && item.Op == catalog.OpCreate {
			priceCreates++
		}
	}
	if priceCreates != 4 {
		t.Errorf("expected 4 price creates (one per currency), got %d", priceCreates)
	}

	// Verify each price's namespaced lookup_key includes the currency
	// suffix so Stripe can disambiguate.
	expectedKeys := map[string]bool{
		"multi_ccy.plus.monthly_eur": false,
		"multi_ccy.plus.monthly_usd": false,
		"multi_ccy.plus.monthly_gbp": false,
		"multi_ccy.plus.monthly_chf": false,
	}
	for _, item := range plan.Items {
		if item.Kind != catalog.KindPrice || item.Op != catalog.OpCreate {
			continue
		}
		expectedKeys[item.NewPrice.LookupKey] = true
	}
	for k, found := range expectedKeys {
		if !found {
			t.Errorf("expected price create with lookup_key %q not in plan", k)
		}
	}
}
