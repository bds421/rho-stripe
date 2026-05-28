package portalconfig

import (
	"testing"

	stripe "github.com/stripe/stripe-go/v82"
)

func TestSpec_ValidatesNamespace(t *testing.T) {
	s := &Spec{DefaultReturnURL: "https://example.com"}
	if err := s.Validate(); err == nil || err.Error() != "portalconfig: Namespace is required" {
		t.Fatalf("want namespace validation; got %v", err)
	}
}

func TestSpec_ValidatesReturnURL(t *testing.T) {
	s := &Spec{Namespace: "ns"}
	if err := s.Validate(); err == nil || err.Error() != "portalconfig: DefaultReturnURL is required" {
		t.Fatalf("want returnURL validation; got %v", err)
	}
}

func TestSpec_ValidValidates(t *testing.T) {
	s := &Spec{Namespace: "ns", DefaultReturnURL: "https://example.com"}
	if err := s.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestNew_PanicsOnNilClient(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil client")
		}
	}()
	_ = New(nil)
}

func TestBuildParams_DefaultsFeatureFlagsToBools(t *testing.T) {
	f := Features{} // everything false
	p := buildCreateFeaturesParams(f)
	if p.InvoiceHistory.Enabled == nil || *p.InvoiceHistory.Enabled {
		t.Errorf("InvoiceHistory should default to enabled=false")
	}
	if p.PaymentMethodUpdate.Enabled == nil || *p.PaymentMethodUpdate.Enabled {
		t.Errorf("PaymentMethodUpdate should default to enabled=false")
	}
}

func TestBuildParams_PassesProductsThrough(t *testing.T) {
	f := Features{
		SubscriptionUpdate: SubscriptionUpdateFeature{
			Enabled: true,
			Products: []SubscriptionUpdateProduct{
				{StripeProductID: "prod_A", StripePriceIDs: []string{"price_1", "price_2"}},
			},
		},
	}
	p := buildCreateFeaturesParams(f)
	if len(p.SubscriptionUpdate.Products) != 1 {
		t.Fatalf("expected 1 product; got %d", len(p.SubscriptionUpdate.Products))
	}
	prod := p.SubscriptionUpdate.Products[0]
	if prod.Product == nil || *prod.Product != "prod_A" {
		t.Errorf("product id not passed through")
	}
	if len(prod.Prices) != 2 {
		t.Errorf("expected 2 prices; got %d", len(prod.Prices))
	}
}

// compile-only smoke: Operations.findByNamespace doesn't panic on
// nil-client construction (would panic on actual call, but the path
// to the call is what we care about here).
func TestOperations_ConstructWithRealClient(t *testing.T) {
	if got := New(&stripe.Client{}); got == nil {
		t.Fatal("New returned nil")
	}
}
