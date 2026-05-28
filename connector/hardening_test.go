package connector_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bds421/rho-stripe/catalog"
	"github.com/bds421/rho-stripe/connector"
)

func miniSpecWithNamespace(ns string) *catalog.Spec {
	return catalog.MustSpec(catalog.Spec{
		Namespace: ns,
		Products: map[string]catalog.Product{
			"pro_plan": {
				Name:        "Pro Plan",
				TaxCategory: catalog.TaxCategorySaaS,
				Prices: map[string]catalog.Price{
					"monthly_eur": {Amount: 4900, Currency: "eur", Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
				},
			},
		},
	})
}

func TestRequireLiveKey_RejectsTestKey(t *testing.T) {
	cfg := validCfg()
	cfg.SecretKey = "sk_test_123"
	cfg.BackendOverride = nil
	cfg.CheckoutBackendOverride = nil
	cfg.RequireLiveKey = true
	_, err := connector.New(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "RequireLiveKey") {
		t.Fatalf("want RequireLiveKey rejection; got %v", err)
	}
}

func TestRequireLiveKey_AcceptsLiveKey(t *testing.T) {
	cfg := validCfg()
	cfg.SecretKey = "sk_live_123"
	cfg.BackendOverride = nil
	cfg.CheckoutBackendOverride = nil
	cfg.RequireLiveKey = true
	// We expect this to fail later (because sk_live_123 is bogus and
	// Stripe will reject the catalog warm), but NOT at the
	// RequireLiveKey guard. The validate-stage error message would
	// mention RequireLiveKey; downstream errors won't.
	_, err := connector.New(t.Context(), cfg)
	if err != nil && strings.Contains(err.Error(), "RequireLiveKey") {
		t.Fatalf("RequireLiveKey wrongly tripped on live key: %v", err)
	}
}

func TestRequireLiveKey_OffAcceptsTestKey(t *testing.T) {
	cfg := validCfg() // defaults to RequireLiveKey=false
	cfg.SecretKey = "sk_test_123"
	cfg.BackendOverride = nil
	cfg.CheckoutBackendOverride = nil
	_, err := connector.New(t.Context(), cfg)
	if err != nil && strings.Contains(err.Error(), "RequireLiveKey") {
		t.Fatalf("RequireLiveKey wrongly tripped when disabled: %v", err)
	}
}

func TestConcurrentNew_SameNamespaceReturnsErr(t *testing.T) {
	cfg := validCfg()
	cfg.AppNamespace = "concurrent_test_ns"
	cfg.Catalog = miniSpecWithNamespace("concurrent_test_ns")
	conn, err := connector.New(t.Context(), cfg)
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	defer conn.Shutdown(context.Background())

	_, err = connector.New(t.Context(), cfg)
	if err == nil {
		t.Fatal("expected ErrDuplicateNamespace on duplicate New for same namespace")
	}
	if !errors.Is(err, connector.ErrDuplicateNamespace) {
		t.Fatalf("got %v, want errors.Is(err, connector.ErrDuplicateNamespace)", err)
	}
}

func TestConcurrentNew_DifferentNamespaceOK(t *testing.T) {
	cfg1 := validCfg()
	cfg1.AppNamespace = "ns_one"
	cfg1.Catalog = miniSpecWithNamespace("ns_one")
	c1, err := connector.New(t.Context(), cfg1)
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	t.Cleanup(func() { _ = c1.Shutdown(context.Background()) })

	cfg2 := validCfg()
	cfg2.AppNamespace = "ns_two"
	cfg2.Catalog = miniSpecWithNamespace("ns_two")
	c2, err := connector.New(t.Context(), cfg2)
	if err != nil {
		t.Fatalf("second New (different ns): %v", err)
	}
	t.Cleanup(func() { _ = c2.Shutdown(context.Background()) })
}

func TestConcurrentNew_AfterShutdownReuseAllowed(t *testing.T) {
	cfg := validCfg()
	cfg.AppNamespace = "reuse_ns"
	cfg.Catalog = miniSpecWithNamespace("reuse_ns")
	c1, err := connector.New(t.Context(), cfg)
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	if err := c1.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	c2, err := connector.New(t.Context(), cfg)
	if err != nil {
		t.Fatalf("second New after Shutdown: %v", err)
	}
	t.Cleanup(func() { _ = c2.Shutdown(context.Background()) })
}
