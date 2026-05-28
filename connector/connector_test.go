package connector_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bds421/rho-kit/data/v2/idempotency"
	"github.com/bds421/rho-stripe/catalog"
	"github.com/bds421/rho-stripe/checkout"
	"github.com/bds421/rho-stripe/connector"
	"github.com/bds421/rho-stripe/invoices"
)

// fakeBackend implements both catalog.Backend and catalog.PriceLister
// (the dual interface the facade requires for cache warmup).
type fakeBackend struct{}

func (f *fakeBackend) ListProductsByNamespace(context.Context, string) ([]catalog.ExistingProduct, error) {
	return nil, nil
}
func (f *fakeBackend) ListByLookupKeys(_ context.Context, keys []string) ([]catalog.ResolvedPrice, error) {
	out := make([]catalog.ResolvedPrice, 0, len(keys))
	for _, k := range keys {
		out = append(out, catalog.ResolvedPrice{LookupKey: k, PriceID: "price_" + k, Active: true})
	}
	return out, nil
}
func (f *fakeBackend) CreateProduct(context.Context, catalog.NewProduct) (catalog.ExistingProduct, error) {
	return catalog.ExistingProduct{}, nil
}
func (f *fakeBackend) UpdateProduct(context.Context, string, catalog.ProductUpdate) error { return nil }
func (f *fakeBackend) UpdateProductActive(context.Context, string, bool) error            { return nil }
func (f *fakeBackend) CreatePrice(context.Context, catalog.NewPrice) (catalog.ExistingPrice, error) {
	return catalog.ExistingPrice{}, nil
}
func (f *fakeBackend) UpdatePriceActive(context.Context, string, bool) error { return nil }
func (f *fakeBackend) ListMetersByNamespace(context.Context, string) ([]catalog.ExistingMeter, error) {
	return nil, nil
}
func (f *fakeBackend) CreateMeter(context.Context, catalog.NewMeter) (catalog.ExistingMeter, error) {
	return catalog.ExistingMeter{}, nil
}
func (f *fakeBackend) ArchiveMeter(context.Context, string) error { return nil }

// fakeCheckoutBackend implements checkout.Backend; not exercised in
// these construction-time tests but required to be non-nil for the
// override path.
type fakeCheckoutBackend struct{}

func (fakeCheckoutBackend) CreateCustomer(context.Context, checkout.CustomerCreate) (checkout.StripeCustomerID, error) {
	return "cus_fake", nil
}
func (fakeCheckoutBackend) CreateCheckoutSession(context.Context, checkout.SessionCreate) (checkout.Session, error) {
	return checkout.Session{ID: "cs_fake", URL: "https://fake"}, nil
}
func (fakeCheckoutBackend) CreatePortalSession(context.Context, checkout.PortalSessionCreate) (checkout.PortalSession, error) {
	return checkout.PortalSession{ID: "bps_fake", URL: "https://fake"}, nil
}

func validSpec() *catalog.Spec {
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

func validCfg() connector.Config {
	return connector.Config{
		WebhookSecret:           "whsec_test",
		AppNamespace:            "demo",
		Catalog:                 validSpec(),
		Customers:               checkout.NewMemoryCustomerRepo(),
		Events:                  idempotency.NewMemoryStore(),
		BackendOverride:         &fakeBackend{},
		CheckoutBackendOverride: fakeCheckoutBackend{},
	}
}

func TestNew_HappyPath(t *testing.T) {
	conn, err := connector.New(t.Context(), validCfg())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = conn.Shutdown(t.Context()) })
	if conn.Catalog == nil || conn.Checkout == nil || conn.Webhooks == nil {
		t.Errorf("New returned Connector with nil subsystem: %+v", conn)
	}
	if !conn.Catalog.Warmed() {
		t.Error("Catalog cache not warmed after New")
	}
	if conn.Spec() == nil || conn.Spec().Namespace != "demo" {
		t.Errorf("Spec() wrong: %+v", conn.Spec())
	}
}

func TestNew_RequiresAppNamespace(t *testing.T) {
	cfg := validCfg()
	cfg.AppNamespace = ""
	_, err := connector.New(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "AppNamespace") {
		t.Errorf("expected AppNamespace error, got %v", err)
	}
}

func TestNew_RequiresCatalog(t *testing.T) {
	cfg := validCfg()
	cfg.Catalog = nil
	_, err := connector.New(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "Catalog") {
		t.Errorf("expected Catalog error, got %v", err)
	}
}

func TestNew_RejectsNamespaceMismatch(t *testing.T) {
	cfg := validCfg()
	cfg.AppNamespace = "other"
	_, err := connector.New(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "match") {
		t.Errorf("expected namespace-mismatch error, got %v", err)
	}
}

func TestNew_RequiresCustomers(t *testing.T) {
	cfg := validCfg()
	cfg.Customers = nil
	_, err := connector.New(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "Customers") {
		t.Errorf("expected Customers error, got %v", err)
	}
}

func TestNew_RequiresEvents(t *testing.T) {
	cfg := validCfg()
	cfg.Events = nil
	_, err := connector.New(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "Events") {
		t.Errorf("expected Events error, got %v", err)
	}
}

func TestNew_RequiresSecretKeyWhenNoOverrides(t *testing.T) {
	cfg := validCfg()
	cfg.BackendOverride = nil
	cfg.CheckoutBackendOverride = nil
	cfg.SecretKey = ""
	_, err := connector.New(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "SecretKey") {
		t.Errorf("expected SecretKey error, got %v", err)
	}
}

// failingBackend triggers Cache.Warm failure during New, verifying the
// fail-fast behavior promised by ADR-0003.
type failingBackend struct{ fakeBackend }

func (f *failingBackend) ListByLookupKeys(context.Context, []string) ([]catalog.ResolvedPrice, error) {
	return nil, errors.New("stripe unreachable")
}

func TestNew_FailsFastOnCacheWarmError(t *testing.T) {
	cfg := validCfg()
	cfg.BackendOverride = &failingBackend{}
	_, err := connector.New(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "warm catalog cache") {
		t.Errorf("expected warm-cache error, got %v", err)
	}
}

// --- Facade wiring for the post-slice-43 audit fixes ---

func TestNew_AsyncWebhooksWiredAndShutsDown(t *testing.T) {
	cfg := validCfg()
	cfg.AsyncWebhooks = &connector.AsyncWebhookConfig{Capacity: 4, Workers: 1}
	conn, err := connector.New(t.Context(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Shutdown must drain cleanly and be idempotent.
	if err := conn.Shutdown(t.Context()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	if err := conn.Shutdown(t.Context()); err != nil {
		t.Errorf("Shutdown (second call): %v", err)
	}
}

func TestNew_DriftDetectorWiredAndStoppedByShutdown(t *testing.T) {
	cfg := validCfg()
	calls := make(chan struct{}, 4)
	cfg.DriftDetector = &connector.DriftDetectorConfig{
		Interval: 20 * time.Millisecond,
		OnReport: func(_ catalog.DriftReport) {
			select {
			case calls <- struct{}{}:
			default:
			}
		},
	}
	conn, err := connector.New(t.Context(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = conn.Shutdown(t.Context()) })
	// Wait for at least one tick.
	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("drift detector never invoked OnReport")
	}
	if err := conn.Shutdown(t.Context()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	// Drain pending events the detector may have emitted right before stop.
drainLoop:
	for {
		select {
		case <-calls:
		default:
			break drainLoop
		}
	}
	// Give it some time and assert no more calls land.
	time.Sleep(60 * time.Millisecond)
	select {
	case <-calls:
		t.Error("drift detector continued after Shutdown")
	default:
	}
}

// recInvoiceBackend is a minimal recording invoice backend so the
// facade test below can prove the InvoiceNumberRepo + OrphanHook
// wiring actually reaches the invoices.Operations facade end-to-end.
type recInvoiceBackend struct {
	drafts []invoices.CreateInput
}

func (r *recInvoiceBackend) ListInvoices(context.Context, invoices.Period) ([]invoices.AuditEntry, error) {
	return nil, nil
}
func (r *recInvoiceBackend) CreateDraft(_ context.Context, in invoices.CreateInput) (invoices.Invoice, error) {
	r.drafts = append(r.drafts, in)
	return invoices.Invoice{StripeID: "in_test", Status: "draft", Customer: in.Customer}, nil
}
func (r *recInvoiceBackend) Finalize(_ context.Context, id string) (invoices.Invoice, error) {
	return invoices.Invoice{StripeID: id}, nil
}
func (r *recInvoiceBackend) FinalizeAndSend(_ context.Context, id string) (invoices.Invoice, error) {
	return invoices.Invoice{StripeID: id}, nil
}
func (r *recInvoiceBackend) Void(context.Context, string) error              { return nil }
func (r *recInvoiceBackend) MarkUncollectible(context.Context, string) error { return nil }
func (r *recInvoiceBackend) IssueCreditNote(context.Context, invoices.CreditNoteInput) error {
	return nil
}
func (r *recInvoiceBackend) ListOverdue(context.Context) ([]invoices.Invoice, error) { return nil, nil }
func (r *recInvoiceBackend) ListByCustomer(context.Context, invoices.StripeCustomerID, string, int) ([]invoices.Invoice, error) {
	return nil, nil
}
func (r *recInvoiceBackend) ListByCustomerPage(context.Context, invoices.InvoicePageRequest) (invoices.InvoicePage, error) {
	return invoices.InvoicePage{}, nil
}

func TestNew_InvoiceNumberRepoAndOrphanHookWired(t *testing.T) {
	cfg := validCfg()
	be := &recInvoiceBackend{}
	cfg.InvoiceBackendOverride = be
	repo := invoices.NewMemoryNumberRepo()
	cfg.InvoiceNumberRepo = repo
	cfg.InvoiceOrphanHook = func(_ context.Context, _ invoices.OrphanInfo) error { return nil }

	conn, err := connector.New(t.Context(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if conn.Invoices == nil {
		t.Fatal("Invoices must be non-nil when InvoiceBackendOverride is set")
	}
	inv, err := conn.Invoices.CreateDraft(t.Context(), invoices.CreateInput{
		Customer:     "cus_x",
		LineItems:    []invoices.CreateLineItem{{Amount: 1000, Currency: "eur"}},
		NumberPrefix: "AT-2026-",
	})
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if len(be.drafts) != 1 {
		t.Fatalf("backend not invoked")
	}
	got := be.drafts[0]
	if got.NumberOverride != "AT-2026-000001" {
		t.Errorf("NumberRepo not wired: NumberOverride=%q", got.NumberOverride)
	}
	if got.Metadata["app_namespace"] != "demo" {
		t.Errorf("namespace not stamped via facade: %v", got.Metadata)
	}
	if inv.StripeID != "in_test" {
		t.Errorf("invoice not returned: %+v", inv)
	}
	_ = conn.Shutdown(t.Context())
}
