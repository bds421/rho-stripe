package credits_test

import (
	"encoding/json"
	"testing"

	"github.com/bds421/rho-stripe/catalog"
	"github.com/bds421/rho-stripe/credits"
	"github.com/bds421/rho-stripe/webhooks"
	stripe "github.com/stripe/stripe-go/v82"
)

func subSpec() *catalog.Spec {
	return catalog.MustSpec(catalog.Spec{
		Namespace: "demo",
		Products: map[string]catalog.Product{
			"pro_plan": {
				Name: "Pro", TaxCategory: catalog.TaxCategorySaaS,
				Prices: map[string]catalog.Price{
					"monthly_eur": {Amount: 4900, Currency: "eur", Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
				},
				RecurringGrant: &catalog.RecurringGrant{Bucket: "ai", Amount: 1000, ValidDaysFromGrant: 30},
			},
		},
	})
}

func invoicePaidEvent(invoiceID, subID, subject, lookupKey string) webhooks.Event {
	body := map[string]any{
		"id":           invoiceID,
		"subscription": subID,
		"metadata":     map[string]string{},
		"subscription_details": map[string]any{
			"metadata": map[string]string{"subject_id": subject, "app_namespace": "demo"},
		},
		"lines": map[string]any{
			"data": []map[string]any{
				{
					"price": map[string]any{
						"id":         "price_xyz",
						"lookup_key": lookupKey,
					},
				},
			},
		},
	}
	raw, _ := json.Marshal(body)
	return webhooks.Event{
		ID: "evt_inv", Type: "invoice.paid",
		Raw: &stripe.Event{Data: &stripe.EventData{Raw: raw}},
	}
}

func TestApplyRecurringGrants_GrantsOncePerInvoice(t *testing.T) {
	repo := credits.NewMemoryRepo()
	spec := subSpec()
	evt := invoicePaidEvent("in_1", "sub_1", "org_acme", "demo.pro_plan.monthly_eur")

	if err := credits.ApplyRecurringGrantsFromInvoice(t.Context(), repo, spec, evt, nil); err != nil {
		t.Fatalf("ApplyRecurringGrantsFromInvoice: %v", err)
	}
	list, _ := repo.ListBySubject(t.Context(), "org_acme")
	if len(list) != 1 || list[0].Bucket != "ai" || list[0].AmountInitial != 1000 {
		t.Errorf("expected 1 grant of 1000 ai credits, got %+v", list)
	}
}

func TestApplyRecurringGrants_IdempotentOnReplay(t *testing.T) {
	repo := credits.NewMemoryRepo()
	spec := subSpec()
	evt := invoicePaidEvent("in_dup", "sub_1", "org_x", "demo.pro_plan.monthly_eur")
	for i := 0; i < 3; i++ {
		if err := credits.ApplyRecurringGrantsFromInvoice(t.Context(), repo, spec, evt, nil); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	list, _ := repo.ListBySubject(t.Context(), "org_x")
	if len(list) != 1 {
		t.Errorf("3 replays should produce 1 grant via SourceRef dedup, got %d", len(list))
	}
}

func TestApplyRecurringGrants_ProductWithoutRecurringGrantSkipped(t *testing.T) {
	repo := credits.NewMemoryRepo()
	spec := catalog.MustSpec(catalog.Spec{
		Namespace: "demo",
		Products: map[string]catalog.Product{
			"plain": {
				Name: "Plain", TaxCategory: catalog.TaxCategorySaaS,
				Prices: map[string]catalog.Price{"monthly_eur": {Amount: 100, Currency: "eur", Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth}},
			},
		},
	})
	evt := invoicePaidEvent("in_p", "sub_p", "org_p", "demo.plain.monthly_eur")
	if err := credits.ApplyRecurringGrantsFromInvoice(t.Context(), repo, spec, evt, nil); err != nil {
		t.Fatal(err)
	}
	list, _ := repo.ListBySubject(t.Context(), "org_p")
	if len(list) != 0 {
		t.Errorf("expected 0 grants for product without RecurringGrant, got %d", len(list))
	}
}

func TestApplyRecurringGrants_WrongEventTypeSkipped(t *testing.T) {
	repo := credits.NewMemoryRepo()
	spec := subSpec()
	evt := webhooks.Event{
		ID: "evt_x", Type: "checkout.session.completed",
		Raw: &stripe.Event{Data: &stripe.EventData{Raw: []byte(`{}`)}},
	}
	if err := credits.ApplyRecurringGrantsFromInvoice(t.Context(), repo, spec, evt, nil); err != nil {
		t.Errorf("non-invoice event should noop, got: %v", err)
	}
}
