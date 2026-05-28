package catalog_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/bds421/rho-stripe/catalog"
)

// fakeLister returns canned responses; tracks calls for assertions.
type fakeLister struct {
	mu      sync.Mutex
	calls   int
	results []catalog.ResolvedPrice
	err     error
}

func (f *fakeLister) ListByLookupKeys(_ context.Context, _ []string) ([]catalog.ResolvedPrice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.results, nil
}

func cacheSpec() *catalog.Spec {
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
		},
	})
}

func TestCache_WarmAndLookup(t *testing.T) {
	spec := cacheSpec()
	lister := &fakeLister{results: []catalog.ResolvedPrice{
		{LookupKey: "demo.pro_plan.monthly_eur", PriceID: "price_M1", ProductID: "prod_demo_pro_plan", Active: true},
		{LookupKey: "demo.pro_plan.yearly_eur", PriceID: "price_Y1", ProductID: "prod_demo_pro_plan", Active: true},
	}}
	cache := catalog.NewCache(lister)

	if err := cache.Warm(t.Context(), spec); err != nil {
		t.Fatalf("Warm: %v", err)
	}

	if !cache.Warmed() {
		t.Error("Warmed() = false after successful Warm")
	}

	for _, tc := range []struct{ key, want string }{
		{"demo.pro_plan.monthly_eur", "price_M1"},
		{"demo.pro_plan.yearly_eur", "price_Y1"},
	} {
		got, ok := cache.Lookup(tc.key)
		if !ok {
			t.Errorf("Lookup(%q): not found", tc.key)
			continue
		}
		if got != tc.want {
			t.Errorf("Lookup(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
}

func TestCache_LookupMissReturnsFalse(t *testing.T) {
	cache := catalog.NewCache(&fakeLister{})
	id, ok := cache.Lookup("never.added")
	if ok {
		t.Errorf("Lookup of unknown key returned ok=true (id=%q); want ok=false", id)
	}
}

func TestCache_WarmFailsOnMissingKeys(t *testing.T) {
	spec := cacheSpec() // declares two keys
	lister := &fakeLister{results: []catalog.ResolvedPrice{
		{LookupKey: "demo.pro_plan.monthly_eur", PriceID: "price_M1", Active: true},
		// yearly_eur intentionally missing
	}}
	cache := catalog.NewCache(lister)
	err := cache.Warm(t.Context(), spec)
	if err == nil {
		t.Fatal("Warm with missing keys returned nil; expected error")
	}
	if !strings.Contains(err.Error(), "demo.pro_plan.yearly_eur") {
		t.Errorf("error did not name the missing key: %v", err)
	}
	if !strings.Contains(err.Error(), "run sync") {
		t.Errorf("error did not hint at sync: %v", err)
	}
}

func TestCache_WarmFiltersInactivePrices(t *testing.T) {
	spec := cacheSpec()
	lister := &fakeLister{results: []catalog.ResolvedPrice{
		{LookupKey: "demo.pro_plan.monthly_eur", PriceID: "price_M_OLD", Active: false},
		{LookupKey: "demo.pro_plan.monthly_eur", PriceID: "price_M_NEW", Active: true},
		{LookupKey: "demo.pro_plan.yearly_eur", PriceID: "price_Y_NEW", Active: true},
	}}
	cache := catalog.NewCache(lister)
	if err := cache.Warm(t.Context(), spec); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	got, _ := cache.Lookup("demo.pro_plan.monthly_eur")
	if got != "price_M_NEW" {
		t.Errorf("expected active price, got %q", got)
	}
}

func TestCache_PropagatesListerError(t *testing.T) {
	wantErr := errors.New("network down")
	cache := catalog.NewCache(&fakeLister{err: wantErr})
	err := cache.Warm(t.Context(), cacheSpec())
	if err == nil || !errors.Is(err, wantErr) {
		t.Errorf("Warm: got %v, want wrapped %v", err, wantErr)
	}
}

func TestCache_RefreshReplacesContents(t *testing.T) {
	spec := cacheSpec()
	lister := &fakeLister{results: []catalog.ResolvedPrice{
		{LookupKey: "demo.pro_plan.monthly_eur", PriceID: "price_M1", Active: true},
		{LookupKey: "demo.pro_plan.yearly_eur", PriceID: "price_Y1", Active: true},
	}}
	cache := catalog.NewCache(lister)
	if err := cache.Warm(t.Context(), spec); err != nil {
		t.Fatalf("Warm: %v", err)
	}

	// Simulate sync that creates new price IDs (price changed).
	lister.results = []catalog.ResolvedPrice{
		{LookupKey: "demo.pro_plan.monthly_eur", PriceID: "price_M2", Active: true},
		{LookupKey: "demo.pro_plan.yearly_eur", PriceID: "price_Y2", Active: true},
	}
	if err := cache.Refresh(t.Context(), spec); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	got, _ := cache.Lookup("demo.pro_plan.monthly_eur")
	if got != "price_M2" {
		t.Errorf("after Refresh, monthly_eur = %q, want %q", got, "price_M2")
	}
}

func TestCache_EmptySpecWarms(t *testing.T) {
	spec := catalog.MustSpec(catalog.Spec{
		Namespace: "empty",
		Products: map[string]catalog.Product{
			"placeholder": {
				Name:        "P",
				TaxCategory: catalog.TaxCategorySaaS,
				Prices: map[string]catalog.Price{
					"p1": {Amount: 1, Currency: "eur", Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
				},
			},
		},
	})
	// Override: drop all prices so AllLookupKeys is empty.
	spec.Products = map[string]catalog.Product{}

	cache := catalog.NewCache(&fakeLister{})
	if err := cache.Warm(t.Context(), spec); err != nil {
		t.Errorf("Warm on empty spec returned error: %v", err)
	}
	if !cache.Warmed() {
		t.Error("Warmed() = false after Warm on empty spec")
	}
}

func TestSpec_AllLookupKeysSorted(t *testing.T) {
	spec := cacheSpec()
	got := spec.AllLookupKeys()
	want := []string{"demo.pro_plan.monthly_eur", "demo.pro_plan.yearly_eur"}
	if len(got) != len(want) {
		t.Fatalf("AllLookupKeys length = %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("AllLookupKeys[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
