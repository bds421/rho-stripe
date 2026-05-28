package cli_test

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bds421/rho-stripe/catalog"
	"github.com/bds421/rho-stripe/checkout"
	"github.com/bds421/rho-stripe/cli"
	"github.com/bds421/rho-stripe/subscriptions"
)

// fakeBackend is a minimal Backend impl that returns canned state and
// records mutation calls.
type fakeBackend struct {
	mu               sync.Mutex
	listResult       []catalog.ExistingProduct
	listErr          error
	createdProducts  int
	createdPrices    int
	updatedProducts  int
	archivedPrices   int
	archivedProducts int
}

func (f *fakeBackend) ListProductsByNamespace(_ context.Context, _ string) ([]catalog.ExistingProduct, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listResult, f.listErr
}
func (f *fakeBackend) CreateProduct(_ context.Context, p catalog.NewProduct) (catalog.ExistingProduct, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createdProducts++
	return catalog.ExistingProduct{ID: p.ID, Name: p.Name, Active: true}, nil
}
func (f *fakeBackend) UpdateProduct(_ context.Context, _ string, _ catalog.ProductUpdate) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updatedProducts++
	return nil
}
func (f *fakeBackend) UpdateProductActive(_ context.Context, _ string, active bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !active {
		f.archivedProducts++
	}
	return nil
}
func (f *fakeBackend) CreatePrice(_ context.Context, p catalog.NewPrice) (catalog.ExistingPrice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createdPrices++
	return catalog.ExistingPrice{ID: "price_" + p.LookupKey, LookupKey: p.LookupKey, Active: true}, nil
}
func (f *fakeBackend) UpdatePriceActive(_ context.Context, _ string, active bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !active {
		f.archivedPrices++
	}
	return nil
}
func (f *fakeBackend) ListMetersByNamespace(_ context.Context, _ string) ([]catalog.ExistingMeter, error) {
	return nil, nil
}
func (f *fakeBackend) CreateMeter(_ context.Context, _ catalog.NewMeter) (catalog.ExistingMeter, error) {
	return catalog.ExistingMeter{}, nil
}
func (f *fakeBackend) ArchiveMeter(_ context.Context, _ string) error { return nil }

func tinySpec() *catalog.Spec {
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

func TestRun_DefaultsToDiff(t *testing.T) {
	var out, errOut bytes.Buffer
	be := &fakeBackend{}
	code := cli.Run(tinySpec(), cli.Options{
		Args:       nil,
		Stdout:     &out,
		Stderr:     &errOut,
		SecretKey:  "sk_test_fake",
		BackendFor: func(string) catalog.Backend { return be },
	})
	if code != 0 {
		t.Errorf("Run returned %d (stderr: %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Sync plan") {
		t.Errorf("expected plan output, got: %s", out.String())
	}
	if be.createdProducts != 0 {
		t.Error("diff should not create anything")
	}
}

func TestRun_SyncWithoutApplyIsDryRun(t *testing.T) {
	var out, errOut bytes.Buffer
	be := &fakeBackend{}
	code := cli.Run(tinySpec(), cli.Options{
		Args:       []string{"sync"},
		Stdout:     &out,
		Stderr:     &errOut,
		SecretKey:  "sk_test_fake",
		BackendFor: func(string) catalog.Backend { return be },
	})
	if code != 0 {
		t.Errorf("Run returned %d (stderr: %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "dry-run") {
		t.Errorf("expected dry-run notice, got: %s", out.String())
	}
	if be.createdProducts != 0 {
		t.Error("sync without --apply must not create anything")
	}
}

func TestRun_SyncApplyCreatesProducts(t *testing.T) {
	var out, errOut bytes.Buffer
	be := &fakeBackend{}
	code := cli.Run(tinySpec(), cli.Options{
		Args:       []string{"sync", "--apply"},
		Stdout:     &out,
		Stderr:     &errOut,
		SecretKey:  "sk_test_fake",
		BackendFor: func(string) catalog.Backend { return be },
	})
	if code != 0 {
		t.Errorf("Run returned %d (stderr: %s)", code, errOut.String())
	}
	if be.createdProducts != 1 || be.createdPrices != 1 {
		t.Errorf("expected 1 product + 1 price created, got %d/%d",
			be.createdProducts, be.createdPrices)
	}
	if !strings.Contains(out.String(), "applied") {
		t.Errorf("expected 'applied.' confirmation, got: %s", out.String())
	}
}

func TestRun_MissingKeyFailsCleanly(t *testing.T) {
	var out, errOut bytes.Buffer
	code := cli.Run(tinySpec(), cli.Options{
		Args:      []string{"diff"},
		Stdout:    &out,
		Stderr:    &errOut,
		SecretKey: "",
	})
	if code == 0 {
		t.Error("Run should fail when SecretKey is empty")
	}
	if !strings.Contains(errOut.String(), "STRIPE_SECRET_KEY") {
		t.Errorf("error should mention STRIPE_SECRET_KEY, got: %s", errOut.String())
	}
}

func TestRun_UnknownSubcommand(t *testing.T) {
	var out, errOut bytes.Buffer
	code := cli.Run(tinySpec(), cli.Options{
		Args:      []string{"frobnicate"},
		Stdout:    &out,
		Stderr:    &errOut,
		SecretKey: "sk_test_fake",
	})
	if code != 2 {
		t.Errorf("unknown subcommand should return exit 2, got %d", code)
	}
	if !strings.Contains(errOut.String(), "unknown subcommand") {
		t.Errorf("expected error message, got: %s", errOut.String())
	}
}

// --- Checkout / Portal subcommand tests via injection seam ---

type fakeListerForCli struct{}

func (fakeListerForCli) ListByLookupKeys(_ context.Context, keys []string) ([]catalog.ResolvedPrice, error) {
	out := make([]catalog.ResolvedPrice, 0, len(keys))
	for _, k := range keys {
		out = append(out, catalog.ResolvedPrice{LookupKey: k, PriceID: "price_" + k, Active: true})
	}
	return out, nil
}

type fakeCheckoutBackend struct {
	mu           sync.Mutex
	sessions     int
	customers    int
	portals      int
	lastSessionP map[string]string // captured session metadata for assertions
}

// Implement catalog.Backend's required methods on fakeListerForCli so a
// single value satisfies both interfaces (mirrors stripeapi.Backend).
func (fakeListerForCli) ListProductsByNamespace(context.Context, string) ([]catalog.ExistingProduct, error) {
	return nil, nil
}
func (fakeListerForCli) CreateProduct(context.Context, catalog.NewProduct) (catalog.ExistingProduct, error) {
	return catalog.ExistingProduct{}, nil
}
func (fakeListerForCli) UpdateProduct(context.Context, string, catalog.ProductUpdate) error {
	return nil
}
func (fakeListerForCli) UpdateProductActive(context.Context, string, bool) error { return nil }
func (fakeListerForCli) CreatePrice(context.Context, catalog.NewPrice) (catalog.ExistingPrice, error) {
	return catalog.ExistingPrice{}, nil
}
func (fakeListerForCli) UpdatePriceActive(context.Context, string, bool) error { return nil }
func (fakeListerForCli) ListMetersByNamespace(context.Context, string) ([]catalog.ExistingMeter, error) {
	return nil, nil
}
func (fakeListerForCli) CreateMeter(context.Context, catalog.NewMeter) (catalog.ExistingMeter, error) {
	return catalog.ExistingMeter{}, nil
}
func (fakeListerForCli) ArchiveMeter(context.Context, string) error { return nil }

// fakeCheckoutBackend methods
func (b *fakeCheckoutBackend) CreateCustomer(_ context.Context, p checkout.CustomerCreate) (checkout.StripeCustomerID, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.customers++
	return checkout.StripeCustomerID("cus_" + string(p.SubjectID)), nil
}
func (b *fakeCheckoutBackend) CreateCheckoutSession(_ context.Context, p checkout.SessionCreate) (checkout.Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sessions++
	b.lastSessionP = p.Metadata
	return checkout.Session{ID: "cs_fake", URL: "https://checkout.example/cs_fake"}, nil
}
func (b *fakeCheckoutBackend) CreatePortalSession(_ context.Context, _ checkout.PortalSessionCreate) (checkout.PortalSession, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.portals++
	return checkout.PortalSession{ID: "bps_fake", URL: "https://billing.example/bps_fake"}, nil
}

func TestRun_CheckoutCreatesSessionWithFakes(t *testing.T) {
	var out, errOut bytes.Buffer
	fakeCheckout := &fakeCheckoutBackend{}
	code := cli.Run(tinySpec(), cli.Options{
		Args:               []string{"checkout", "pro_plan.monthly_eur"},
		Stdout:             &out,
		Stderr:             &errOut,
		SecretKey:          "sk_test_fake",
		PriceListerFor:     func(string) catalog.PriceLister { return fakeListerForCli{} },
		CheckoutBackendFor: func(string) checkout.Backend { return fakeCheckout },
	})
	if code != 0 {
		t.Fatalf("Run returned %d (stderr: %s)", code, errOut.String())
	}
	if fakeCheckout.sessions != 1 {
		t.Errorf("sessions created = %d, want 1", fakeCheckout.sessions)
	}
	if fakeCheckout.customers != 1 {
		t.Errorf("customers created = %d, want 1", fakeCheckout.customers)
	}
	if !strings.Contains(out.String(), "cs_fake") {
		t.Errorf("expected session id in output, got: %s", out.String())
	}
	if fakeCheckout.lastSessionP["app_namespace"] != "demo" {
		t.Errorf("session metadata missing app_namespace: %+v", fakeCheckout.lastSessionP)
	}
}

func TestRun_PortalRejectsMissingCustomer(t *testing.T) {
	var out, errOut bytes.Buffer
	code := cli.Run(tinySpec(), cli.Options{
		Args:               []string{"portal"},
		Stdout:             &out,
		Stderr:             &errOut,
		SecretKey:          "sk_test_fake",
		CheckoutBackendFor: func(string) checkout.Backend { return &fakeCheckoutBackend{} },
	})
	if code != 2 {
		t.Errorf("Run returned %d, want 2 (missing --customer)", code)
	}
	if !strings.Contains(errOut.String(), "--customer") {
		t.Errorf("error should mention --customer, got: %s", errOut.String())
	}
}

func TestRun_PortalCreatesSessionWithFakes(t *testing.T) {
	var out, errOut bytes.Buffer
	fakeCheckout := &fakeCheckoutBackend{}
	code := cli.Run(tinySpec(), cli.Options{
		Args:               []string{"portal", "--customer", "cus_demo_123"},
		Stdout:             &out,
		Stderr:             &errOut,
		SecretKey:          "sk_test_fake",
		CheckoutBackendFor: func(string) checkout.Backend { return fakeCheckout },
	})
	if code != 0 {
		t.Fatalf("Run returned %d (stderr: %s)", code, errOut.String())
	}
	if fakeCheckout.portals != 1 {
		t.Errorf("portals created = %d, want 1", fakeCheckout.portals)
	}
	if !strings.Contains(out.String(), "bps_fake") {
		t.Errorf("expected portal session id in output, got: %s", out.String())
	}
}

func TestRun_HelpReturnsZero(t *testing.T) {
	var out, errOut bytes.Buffer
	code := cli.Run(tinySpec(), cli.Options{
		Args:      []string{"help"},
		Stdout:    &out,
		Stderr:    &errOut,
		SecretKey: "", // help doesn't require key
	})
	if code != 0 {
		t.Errorf("help should return 0, got %d", code)
	}
	if !strings.Contains(errOut.String(), "Usage:") {
		t.Errorf("expected usage text, got: %s", errOut.String())
	}
}

// --- Slice 44 follow-up: drift-check + subs subcommands ---

func TestRun_DriftCheckReportsNoDriftWhenInSync(t *testing.T) {
	spec := singleProductSpec()
	be := &fakeBackend{
		// Return the same product the spec declares so Diff produces no items.
		listResult: stripeReadyFromSpec(spec),
	}
	var stdout bytes.Buffer
	code := cli.Run(spec, cli.Options{
		Args:       []string{"drift-check"},
		SecretKey:  "sk_test_x",
		Stdout:     &stdout,
		Stderr:     io.Discard,
		BackendFor: func(string) catalog.Backend { return be },
	})
	if code != 0 {
		t.Fatalf("exit=%d, want 0; stdout=%q", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "no drift") {
		t.Errorf("stdout=%q, want 'no drift'", stdout.String())
	}
}

func TestRun_DriftCheckReturns1WhenDriftAndNoFailGivesZero(t *testing.T) {
	spec := singleProductSpec()
	be := &fakeBackend{} // empty Stripe → drift guaranteed
	var stdout bytes.Buffer
	code := cli.Run(spec, cli.Options{
		Args:       []string{"drift-check"},
		SecretKey:  "sk_test_x",
		Stdout:     &stdout,
		Stderr:     io.Discard,
		BackendFor: func(string) catalog.Backend { return be },
	})
	if code != 1 {
		t.Errorf("expected exit 1 on drift, got %d", code)
	}
	if !strings.Contains(stdout.String(), "drift detected") {
		t.Errorf("stdout missing drift message: %q", stdout.String())
	}

	stdout.Reset()
	code = cli.Run(spec, cli.Options{
		Args:       []string{"drift-check", "--no-fail"},
		SecretKey:  "sk_test_x",
		Stdout:     &stdout,
		Stderr:     io.Discard,
		BackendFor: func(string) catalog.Backend { return be },
	})
	if code != 0 {
		t.Errorf("--no-fail should exit 0, got %d", code)
	}
}

func TestRun_DriftCheckApplyDrivesBackend(t *testing.T) {
	spec := singleProductSpec()
	be := &fakeBackend{}
	var stdout bytes.Buffer
	code := cli.Run(spec, cli.Options{
		Args:       []string{"drift-check", "--apply"},
		SecretKey:  "sk_test_x",
		Stdout:     &stdout,
		Stderr:     io.Discard,
		BackendFor: func(string) catalog.Backend { return be },
	})
	if code != 0 {
		t.Errorf("exit=%d, want 0; stdout=%q", code, stdout.String())
	}
	if be.createdProducts == 0 {
		t.Error("--apply did not drive any backend creates")
	}
}

func TestRun_SubsSeats_GetActionReachesBackend(t *testing.T) {
	spec := singleProductSpec()
	be := &fakeSubBackend{} // mirror is empty → falls back to Stripe (returns nothing)
	var stdout bytes.Buffer
	code := cli.Run(spec, cli.Options{
		Args:      []string{"subs", "seats", "--sub", "sub_x", "--price", "pro.monthly_eur", "--action", "get"},
		SecretKey: "sk_test_x",
		Stdout:    &stdout,
		Stderr:    io.Discard,
		BackendFor: func(string) catalog.Backend {
			return &fakeBackend{}
		},
		PriceListerFor: func(string) catalog.PriceLister {
			return fakeListerForCli{}
		},
		SubscriptionBackendFor: func(string) subscriptions.Backend {
			return be
		},
	})
	if code != 0 {
		t.Fatalf("exit=%d, want 0; stdout=%q", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "seats: 0") {
		t.Errorf("stdout=%q, want 'seats: 0'", stdout.String())
	}
}

// singleProductSpec is a minimal spec the new tests use.
func singleProductSpec() *catalog.Spec {
	return catalog.MustSpec(catalog.Spec{
		Namespace: "demo",
		Products: map[string]catalog.Product{
			"pro": {
				Name:        "Pro",
				TaxCategory: catalog.TaxCategorySaaS,
				Prices: map[string]catalog.Price{
					"monthly_eur": {Amount: 4900, Currency: "eur", Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
				},
			},
		},
	})
}

// stripeReadyFromSpec projects a spec into the ExistingProduct shape
// the diff sees as in-sync (no drift).
func stripeReadyFromSpec(spec *catalog.Spec) []catalog.ExistingProduct {
	var out []catalog.ExistingProduct
	for pk, p := range spec.Products {
		var prices []catalog.ExistingPrice
		for prk, pr := range p.Prices {
			ep := catalog.ExistingPrice{
				ID:        "price_" + prk,
				LookupKey: spec.NamespacedPriceKey(pk, prk),
				Active:    true,
				Amount:    pr.Amount,
				Currency:  pr.Currency,
				Type:      pr.Type,
			}
			if pr.Type == catalog.PriceTypeRecurring {
				ep.Interval = pr.Interval
				if pr.IntervalCount == 0 {
					ep.IntervalCount = 1
				} else {
					ep.IntervalCount = pr.IntervalCount
				}
			}
			prices = append(prices, ep)
		}
		out = append(out, catalog.ExistingProduct{
			ID:      spec.NamespacedProductID(pk),
			Name:    p.Name,
			Active:  true,
			TaxCode: "txcd_10103000",
			Prices:  prices,
		})
	}
	return out
}

// fakeSubBackend is the minimal subscriptions.Backend the CLI seat tests
// need. All methods are no-ops returning empties.
type fakeSubBackend struct{}

func (fakeSubBackend) CancelAtPeriodEnd(context.Context, string, bool) error { return nil }
func (fakeSubBackend) CancelNow(context.Context, string, subscriptions.CancelNowOptions) error {
	return nil
}
func (fakeSubBackend) UpdateItems(context.Context, string, []subscriptions.ItemChange, subscriptions.UpdateOptions) error {
	return nil
}
func (fakeSubBackend) ApplyCoupon(context.Context, string, string) error { return nil }
func (fakeSubBackend) RemoveCoupon(context.Context, string) error        { return nil }
func (fakeSubBackend) ListSince(context.Context, time.Time) ([]subscriptions.ReconciledSubscription, error) {
	return nil, nil
}
func (fakeSubBackend) CreateInvoiced(context.Context, subscriptions.InvoicedSubscriptionInput) (*subscriptions.Subscription, error) {
	return &subscriptions.Subscription{StripeID: "sub_test"}, nil
}
func (fakeSubBackend) CreateSubscriptionSchedule(context.Context, subscriptions.ScheduleBackendInput) (*subscriptions.Schedule, error) {
	return &subscriptions.Schedule{StripeID: "sub_sched_test"}, nil
}
func (fakeSubBackend) GetSubscriptionItems(context.Context, string) ([]subscriptions.SubscriptionItem, error) {
	return nil, nil
}
func (fakeSubBackend) PreviewItemChange(context.Context, string, []subscriptions.ItemChange) (subscriptions.MigratePreview, error) {
	return subscriptions.MigratePreview{}, nil
}
func (fakeSubBackend) Pause(context.Context, string, subscriptions.PauseOptions) error { return nil }
func (fakeSubBackend) Resume(context.Context, string) error                            { return nil }
