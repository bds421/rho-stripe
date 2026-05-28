package catalog_test

import (
	"strings"
	"testing"

	"github.com/bds421/rho-stripe/catalog"
)

// twoProductSpec is the baseline used across diff tests.
func twoProductSpec() *catalog.Spec {
	return catalog.MustSpec(catalog.Spec{
		Namespace: "demo",
		Products: map[string]catalog.Product{
			"pro_plan": {
				Name:        "Pro Plan",
				TaxCategory: catalog.TaxCategorySaaS,
				Prices: map[string]catalog.Price{
					"monthly_eur": {Amount: 4900, Currency: "eur", Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
					"yearly_eur":  {Amount: 49000, Currency: "eur", Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalYear},
				},
			},
			"starter": {
				Name:        "Starter",
				TaxCategory: catalog.TaxCategorySaaS,
				Prices: map[string]catalog.Price{
					"monthly_eur": {Amount: 900, Currency: "eur", Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
				},
			},
		},
	})
}

func TestDiff_EmptyStripeCreatesEverything(t *testing.T) {
	spec := twoProductSpec()
	plan := catalog.Diff(spec, nil)

	productCreates, priceCreates := countOps(plan, catalog.OpCreate)
	if productCreates != 2 {
		t.Errorf("expected 2 product CREATEs, got %d", productCreates)
	}
	if priceCreates != 3 {
		t.Errorf("expected 3 price CREATEs, got %d", priceCreates)
	}

	productArchives, priceArchives := countOps(plan, catalog.OpArchive)
	if productArchives != 0 || priceArchives != 0 {
		t.Errorf("expected 0 archives, got product=%d price=%d", productArchives, priceArchives)
	}
}

func TestDiff_MatchingStateIsEmpty(t *testing.T) {
	spec := twoProductSpec()
	current := stripeStateMatching(spec)
	plan := catalog.Diff(spec, current)
	if !plan.Empty() {
		t.Errorf("expected empty plan; got %d items:\n%s", len(plan.Items), plan)
	}
}

func TestDiff_RemovedProductIsArchived(t *testing.T) {
	spec := twoProductSpec()
	current := stripeStateMatching(spec)
	// Drop "starter" from the spec — sync should archive it.
	delete(spec.Products, "starter")

	plan := catalog.Diff(spec, current)

	var sawProduct, sawPrice bool
	for _, it := range plan.Items {
		if it.Op != catalog.OpArchive {
			continue
		}
		switch it.Kind {
		case catalog.KindProduct:
			if it.Key == "starter" {
				sawProduct = true
			}
		case catalog.KindPrice:
			if it.Key == "starter.monthly_eur" {
				sawPrice = true
			}
		}
	}
	if !sawProduct {
		t.Errorf("expected ARCHIVE for product starter; plan was:\n%s", plan)
	}
	if !sawPrice {
		t.Errorf("expected ARCHIVE for price starter.monthly_eur; plan was:\n%s", plan)
	}
}

func TestDiff_RemovedPriceIsArchived(t *testing.T) {
	spec := twoProductSpec()
	current := stripeStateMatching(spec)
	// Drop yearly_eur from pro_plan in the spec.
	pro := spec.Products["pro_plan"]
	delete(pro.Prices, "yearly_eur")
	spec.Products["pro_plan"] = pro

	plan := catalog.Diff(spec, current)

	var sawPrice bool
	for _, it := range plan.Items {
		if it.Op == catalog.OpArchive && it.Kind == catalog.KindPrice && it.Key == "pro_plan.yearly_eur" {
			sawPrice = true
		}
	}
	if !sawPrice {
		t.Errorf("expected ARCHIVE for price pro_plan.yearly_eur; plan was:\n%s", plan)
	}
}

func TestDiff_ArchivedProductInStripeNotInSpecIsSilent(t *testing.T) {
	spec := twoProductSpec()
	current := stripeStateMatching(spec)
	// Mark "starter" archived in Stripe and remove from spec — no plan item.
	for i := range current {
		if current[i].ID == "prod_demo_starter" {
			current[i].Active = false
			for j := range current[i].Prices {
				current[i].Prices[j].Active = false
			}
		}
	}
	delete(spec.Products, "starter")

	plan := catalog.Diff(spec, current)
	for _, it := range plan.Items {
		if strings.Contains(it.Key, "starter") {
			t.Errorf("unexpected plan item for already-archived starter: %+v", it)
		}
	}
}

func TestDiff_ArchivedProductBackInSpecEmitsUnarchive(t *testing.T) {
	spec := twoProductSpec()
	current := stripeStateMatching(spec)
	// Pre-archive starter in Stripe but keep it in spec.
	for i := range current {
		if current[i].ID == "prod_demo_starter" {
			current[i].Active = false
		}
	}

	plan := catalog.Diff(spec, current)
	var sawUnarchive bool
	for _, it := range plan.Items {
		if it.Op == catalog.OpUnarchive && it.Kind == catalog.KindProduct && it.Key == "starter" {
			sawUnarchive = true
		}
	}
	if !sawUnarchive {
		t.Errorf("expected UNARCHIVE for archived-but-in-spec product; plan was:\n%s", plan)
	}
}

func TestDiff_PriceAmountChangeEmitsReplace(t *testing.T) {
	spec := twoProductSpec()
	current := stripeStateMatching(spec)
	pro := spec.Products["pro_plan"]
	m := pro.Prices["monthly_eur"]
	m.Amount = 5900
	pro.Prices["monthly_eur"] = m
	spec.Products["pro_plan"] = pro

	plan := catalog.Diff(spec, current)

	var replaceItem *catalog.PlanItem
	for i, it := range plan.Items {
		if it.Op == catalog.OpReplace && it.Kind == catalog.KindPrice && it.Key == "pro_plan.monthly_eur" {
			replaceItem = &plan.Items[i]
		}
	}
	if replaceItem == nil {
		t.Fatalf("expected REPLACE for pro_plan.monthly_eur; plan:\n%s", plan)
	}
	if replaceItem.NewPrice == nil || replaceItem.ExistingPrice == nil {
		t.Error("REPLACE item should carry both NewPrice and ExistingPrice")
	}
	if !replaceItem.NewPrice.TransferLookupKey {
		t.Error("REPLACE NewPrice should set TransferLookupKey=true")
	}
}

func TestDiff_PriceIntervalChangeEmitsReplace(t *testing.T) {
	spec := twoProductSpec()
	current := stripeStateMatching(spec)
	pro := spec.Products["pro_plan"]
	m := pro.Prices["monthly_eur"]
	m.Interval = catalog.IntervalYear
	pro.Prices["monthly_eur"] = m
	spec.Products["pro_plan"] = pro

	plan := catalog.Diff(spec, current)
	var saw bool
	for _, it := range plan.Items {
		if it.Op == catalog.OpReplace && it.Kind == catalog.KindPrice {
			saw = true
		}
	}
	if !saw {
		t.Errorf("expected REPLACE for interval change; plan:\n%s", plan)
	}
}

func TestDiff_PriceUnchangedNoReplace(t *testing.T) {
	spec := twoProductSpec()
	current := stripeStateMatching(spec)
	plan := catalog.Diff(spec, current)
	for _, it := range plan.Items {
		if it.Op == catalog.OpReplace {
			t.Errorf("unexpected REPLACE for unchanged spec: %+v", it)
		}
	}
}

func TestDiff_ProductRenameEmitsUpdate(t *testing.T) {
	spec := twoProductSpec()
	current := stripeStateMatching(spec)
	pro := spec.Products["pro_plan"]
	pro.Name = "Pro (New Name)"
	spec.Products["pro_plan"] = pro

	plan := catalog.Diff(spec, current)
	var saw bool
	for _, it := range plan.Items {
		if it.Op == catalog.OpUpdate && it.Kind == catalog.KindProduct && it.Key == "pro_plan" {
			saw = true
		}
	}
	if !saw {
		t.Errorf("expected UPDATE for product rename; plan:\n%s", plan)
	}
}

func TestDiff_ProductDescriptionChangeEmitsUpdate(t *testing.T) {
	spec := twoProductSpec()
	current := stripeStateMatching(spec)
	pro := spec.Products["pro_plan"]
	pro.Description = "New description"
	spec.Products["pro_plan"] = pro

	plan := catalog.Diff(spec, current)
	var saw bool
	for _, it := range plan.Items {
		if it.Op == catalog.OpUpdate && it.Kind == catalog.KindProduct {
			saw = true
		}
	}
	if !saw {
		t.Errorf("expected UPDATE for description change; plan:\n%s", plan)
	}
}

func TestPlan_StringContainsNamespace(t *testing.T) {
	spec := twoProductSpec()
	plan := catalog.Diff(spec, nil)
	out := plan.String()
	if !strings.Contains(out, `"demo"`) {
		t.Errorf("Plan.String() does not contain namespace: %s", out)
	}
	if !strings.Contains(out, "CREATE") {
		t.Errorf("Plan.String() does not mention CREATE: %s", out)
	}
}

func TestPlan_EmptyString(t *testing.T) {
	spec := twoProductSpec()
	plan := catalog.Diff(spec, stripeStateMatching(spec))
	out := plan.String()
	if !strings.Contains(out, "no changes") {
		t.Errorf("Plan.String() for empty plan should say 'no changes': %s", out)
	}
}

func TestPlan_CreatePricesCarryNamespaceMetadata(t *testing.T) {
	spec := twoProductSpec()
	plan := catalog.Diff(spec, nil)
	for _, it := range plan.Items {
		if it.Op == catalog.OpCreate && it.Kind == catalog.KindPrice {
			if it.NewPrice == nil {
				t.Fatal("CREATE price item missing NewPrice")
			}
			if it.NewPrice.Metadata["app_namespace"] != "demo" {
				t.Errorf("price metadata.app_namespace = %q, want %q",
					it.NewPrice.Metadata["app_namespace"], "demo")
			}
		}
	}
}

// --- helpers ---

func countOps(plan catalog.Plan, op catalog.PlanOp) (products, prices int) {
	for _, it := range plan.Items {
		if it.Op != op {
			continue
		}
		switch it.Kind {
		case catalog.KindProduct:
			products++
		case catalog.KindPrice:
			prices++
		}
	}
	return
}

// stripeStateMatching synthesizes the Stripe state that would exist
// after a successful sync of spec — used as the "matching" baseline
// in tests that mutate one thing and expect a specific plan.
func stripeStateMatching(spec *catalog.Spec) []catalog.ExistingProduct {
	var out []catalog.ExistingProduct
	for productKey, product := range spec.Products {
		ep := catalog.ExistingProduct{
			ID:      spec.NamespacedProductID(productKey),
			Name:    product.Name,
			TaxCode: string(product.TaxCategory),
			Active:  true,
		}
		i := 0
		for priceKey, price := range product.Prices {
			ep.Prices = append(ep.Prices, catalog.ExistingPrice{
				ID:            "price_fake_" + productKey + "_" + priceKey,
				LookupKey:     spec.NamespacedPriceKey(productKey, priceKey),
				Active:        true,
				Amount:        price.Amount,
				Currency:      price.Currency,
				Type:          price.Type,
				Interval:      price.Interval,
				IntervalCount: price.IntervalCount,
			})
			i++
		}
		out = append(out, ep)
	}
	return out
}
