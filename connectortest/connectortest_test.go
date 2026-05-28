package connectortest_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/bds421/rho-stripe/catalog"
	"github.com/bds421/rho-stripe/checkout"
	"github.com/bds421/rho-stripe/connector"
	"github.com/bds421/rho-stripe/connectortest"
	"github.com/bds421/rho-stripe/credits"
	"github.com/bds421/rho-stripe/webhooks"
)

const testSecret = "whsec_test_xxx"

func miniSpec() *catalog.Spec {
	return catalog.MustSpec(catalog.Spec{
		Namespace: "demo",
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

func TestMemoryRepos_IndependentInstances(t *testing.T) {
	a := connectortest.NewMemoryRepos()
	b := connectortest.NewMemoryRepos()
	_ = a.Customers.Upsert(t.Context(), "org_acme", "cus_a")
	if _, ok, _ := b.Customers.Get(t.Context(), "org_acme"); ok {
		t.Error("instance b should not see instance a's upserts")
	}
}

func TestEventFixtures_CheckoutCompletedShape(t *testing.T) {
	evt := connectortest.EventFixtures.CheckoutCompleted("demo", "org_acme", "cs_test_1")
	if evt.Type != "checkout.session.completed" {
		t.Errorf("Type = %q", evt.Type)
	}
	if evt.Raw == nil || evt.Raw.Data == nil || len(evt.Raw.Data.Raw) == 0 {
		t.Fatal("Raw event data missing")
	}
	body := string(evt.Raw.Data.Raw)
	if !bytes.Contains(evt.Raw.Data.Raw, []byte(`"app_namespace":"demo"`)) {
		t.Errorf("body missing app_namespace stamp: %s", body)
	}
	if !bytes.Contains(evt.Raw.Data.Raw, []byte(`"subject_id":"org_acme"`)) {
		t.Errorf("body missing subject_id stamp: %s", body)
	}
}

func TestEventFixtures_CheckoutCompletedWithCredits(t *testing.T) {
	grants := []credits.PendingGrant{
		{Bucket: "ai", Amount: 1000, ValidDays: 90, ProductKey: "credit_pack", Quantity: 1},
	}
	evt, err := connectortest.EventFixtures.CheckoutCompletedWithCredits("demo", "org_x", "cs_g", grants)
	if err != nil {
		t.Fatalf("CheckoutCompletedWithCredits: %v", err)
	}
	if !bytes.Contains(evt.Raw.Data.Raw, []byte(`"credit_grants"`)) {
		t.Errorf("body missing credit_grants: %s", string(evt.Raw.Data.Raw))
	}
}

func TestSignedBody_Roundtrip(t *testing.T) {
	// End-to-end: fixture → signed envelope → POST to a real Webhooks
	// instance → handler fires → ledger updated. This is the canonical
	// "test your handler" pattern for downstream apps.
	repos := connectortest.NewMemoryRepos()

	conn, err := connector.New(t.Context(), connector.Config{
		WebhookSecret:           testSecret,
		AppNamespace:            "demo",
		Catalog:                 miniSpec(),
		Customers:               repos.Customers,
		Events:                  repos.Events,
		Credits:                 repos.Credits,
		BackendOverride:         &noopCatalogBackend{},
		CheckoutBackendOverride: noopCheckoutBackend{},
		Handlers: webhooks.Handlers{
			OnCheckoutCompleted: func(_ context.Context, _ webhooks.Event) error { return nil },
		},
	})
	if err != nil {
		t.Fatalf("connector.New: %v", err)
	}
	t.Cleanup(func() { _ = conn.Shutdown(t.Context()) })

	grants := []credits.PendingGrant{
		{Bucket: "ai", Amount: 500, ValidDays: 30, ProductKey: "credit_pack", Quantity: 1},
	}
	evt, err := connectortest.EventFixtures.CheckoutCompletedWithCredits("demo", "org_acme", "cs_demo", grants)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	body, signature, err := connectortest.SignedBody(testSecret, evt)
	if err != nil {
		t.Fatalf("SignedBody: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", signature)
	rec := httptest.NewRecorder()
	conn.Webhooks.Handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Handle status = %d (body: %s)", rec.Code, rec.Body.String())
	}

	list, _ := repos.Credits.ListBySubject(t.Context(), "org_acme")
	if len(list) != 1 || list[0].AmountInitial != 500 || list[0].Bucket != "ai" {
		t.Errorf("expected 1 grant of 500 ai credits; got %+v", list)
	}
}

func TestSignedBody_HandlerCalledOnSimpleEvent(t *testing.T) {
	var called atomic.Int32
	repos := connectortest.NewMemoryRepos()
	conn, err := connector.New(t.Context(), connector.Config{
		WebhookSecret:           testSecret,
		AppNamespace:            "demo",
		Catalog:                 miniSpec(),
		Customers:               repos.Customers,
		Events:                  repos.Events,
		BackendOverride:         &noopCatalogBackend{},
		CheckoutBackendOverride: noopCheckoutBackend{},
		Handlers: webhooks.Handlers{
			OnSubscriptionCreated: func(_ context.Context, _ webhooks.Event) error {
				called.Add(1)
				return nil
			},
		},
	})
	if err != nil {
		t.Fatalf("connector.New: %v", err)
	}
	t.Cleanup(func() { _ = conn.Shutdown(t.Context()) })

	evt := connectortest.EventFixtures.SubscriptionCreated("demo", "org_acme", "sub_1", "cus_1")
	body, signature, err := connectortest.SignedBody(testSecret, evt)
	if err != nil {
		t.Fatalf("SignedBody: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", signature)
	rec := httptest.NewRecorder()
	conn.Webhooks.Handle(rec, req)
	if rec.Code != http.StatusOK || called.Load() != 1 {
		t.Errorf("status=%d called=%d (body=%s)", rec.Code, called.Load(), rec.Body.String())
	}
}

// --- minimal no-op backends so connector.New doesn't try to hit Stripe ---

type noopCatalogBackend struct{}

func (noopCatalogBackend) ListProductsByNamespace(context.Context, string) ([]catalog.ExistingProduct, error) {
	return nil, nil
}
func (noopCatalogBackend) ListByLookupKeys(_ context.Context, keys []string) ([]catalog.ResolvedPrice, error) {
	out := make([]catalog.ResolvedPrice, 0, len(keys))
	for _, k := range keys {
		out = append(out, catalog.ResolvedPrice{LookupKey: k, PriceID: "price_" + k, Active: true})
	}
	return out, nil
}
func (noopCatalogBackend) CreateProduct(context.Context, catalog.NewProduct) (catalog.ExistingProduct, error) {
	return catalog.ExistingProduct{}, nil
}
func (noopCatalogBackend) UpdateProduct(context.Context, string, catalog.ProductUpdate) error {
	return nil
}
func (noopCatalogBackend) UpdateProductActive(context.Context, string, bool) error { return nil }
func (noopCatalogBackend) CreatePrice(context.Context, catalog.NewPrice) (catalog.ExistingPrice, error) {
	return catalog.ExistingPrice{}, nil
}
func (noopCatalogBackend) UpdatePriceActive(context.Context, string, bool) error { return nil }
func (noopCatalogBackend) ListMetersByNamespace(context.Context, string) ([]catalog.ExistingMeter, error) {
	return nil, nil
}
func (noopCatalogBackend) CreateMeter(context.Context, catalog.NewMeter) (catalog.ExistingMeter, error) {
	return catalog.ExistingMeter{}, nil
}
func (noopCatalogBackend) ArchiveMeter(context.Context, string) error { return nil }

type noopCheckoutBackend struct{}

func (noopCheckoutBackend) CreateCustomer(context.Context, checkout.CustomerCreate) (checkout.StripeCustomerID, error) {
	return "cus_n", nil
}
func (noopCheckoutBackend) CreateCheckoutSession(context.Context, checkout.SessionCreate) (checkout.Session, error) {
	return checkout.Session{ID: "cs_n", URL: "https://example.com/cs"}, nil
}
func (noopCheckoutBackend) CreatePortalSession(context.Context, checkout.PortalSessionCreate) (checkout.PortalSession, error) {
	return checkout.PortalSession{ID: "bps_n", URL: "https://example.com/bps"}, nil
}
