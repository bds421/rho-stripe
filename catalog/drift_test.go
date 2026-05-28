package catalog_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bds421/rho-stripe/catalog"
)

func driftSpec() *catalog.Spec {
	return catalog.MustSpec(catalog.Spec{
		Namespace: "drift_demo",
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

func TestCheckDrift_EmptyStripeReportsDrift(t *testing.T) {
	be := &fakeBackend{}
	rep, err := catalog.CheckDrift(t.Context(), be, driftSpec())
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if !rep.HasDrift {
		t.Error("empty Stripe vs non-empty spec: expected drift")
	}
	if rep.Namespace != "drift_demo" {
		t.Errorf("Namespace = %q, want drift_demo", rep.Namespace)
	}
	if rep.CheckedAt.IsZero() {
		t.Error("CheckedAt should be set")
	}
	if len(rep.Items) == 0 {
		t.Error("drift items should be non-empty")
	}
}

func TestCheckDrift_SyncedStripeReportsNoDrift(t *testing.T) {
	spec := driftSpec()
	// Build a fake whose ListProductsByNamespace returns the exact
	// shape the spec expects, so the diff is empty.
	be := &fakeBackend{
		listResult: []catalog.ExistingProduct{
			{
				ID:          spec.NamespacedProductID("pro"),
				Name:        "Pro",
				Active:      true,
				TaxCode:     "txcd_10103000",
				Description: "",
				Prices: []catalog.ExistingPrice{
					{
						ID:            "price_existing",
						LookupKey:     spec.NamespacedPriceKey("pro", "monthly_eur"),
						Active:        true,
						Amount:        4900,
						Currency:      "eur",
						Type:          catalog.PriceTypeRecurring,
						Interval:      catalog.IntervalMonth,
						IntervalCount: 1,
					},
				},
			},
		},
	}
	rep, err := catalog.CheckDrift(t.Context(), be, spec)
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if rep.HasDrift {
		t.Errorf("expected no drift; got items=%+v", rep.Items)
	}
}

func TestCheckDrift_ListProductsErrorPropagates(t *testing.T) {
	be := &erroringDriftBackend{err: contextErr("list products boom")}
	_, err := catalog.CheckDrift(t.Context(), be, driftSpec())
	if err == nil {
		t.Error("expected error to propagate")
	}
}

func TestRunDriftDetector_TickInvokesOnReport(t *testing.T) {
	var calls atomic.Int32
	done := make(chan struct{}, 1)
	be := &fakeBackend{} // empty Stripe → guaranteed drift
	stop := catalog.RunDriftDetector(t.Context(), be, driftSpec(), 20*time.Millisecond, func(rep catalog.DriftReport) {
		if calls.Add(1) == 1 {
			if !rep.HasDrift {
				t.Error("expected drift in first report")
			}
			select {
			case done <- struct{}{}:
			default:
			}
		}
	}, nil)
	defer stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drift detector never invoked onReport")
	}
}

func TestRunDriftDetector_StopHaltsLoop(t *testing.T) {
	var calls atomic.Int32
	be := &fakeBackend{}
	stop := catalog.RunDriftDetector(t.Context(), be, driftSpec(), 10*time.Millisecond, func(_ catalog.DriftReport) {
		calls.Add(1)
	}, nil)
	time.Sleep(35 * time.Millisecond)
	stop()
	snapshot := calls.Load()
	time.Sleep(60 * time.Millisecond)
	if calls.Load() != snapshot {
		t.Errorf("calls kept incrementing after stop: %d → %d", snapshot, calls.Load())
	}
}

func TestRunDriftDetector_StopIsIdempotent(t *testing.T) {
	// Pre-slice-55 bug: stop() called close(stopCh) directly →
	// second call panicked on close-of-closed-channel. Verify the
	// sync.Once fix makes double-stop a no-op.
	be := &fakeBackend{}
	stop := catalog.RunDriftDetector(t.Context(), be, driftSpec(), 10*time.Millisecond, nil, nil)
	stop()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("second stop() panicked: %v", r)
		}
	}()
	stop()
}

func TestRunDriftDetector_StopWaitsForGoroutine(t *testing.T) {
	// Pre-slice-55 bug: stop() didn't wait for the goroutine, so an
	// in-flight CheckDrift could call onReport AFTER stop() returned.
	// Verify the done-channel makes stop() synchronous.
	var calls atomic.Int32
	be := &slowDriftBackend{delay: 30 * time.Millisecond}
	stop := catalog.RunDriftDetector(t.Context(), be, driftSpec(), 10*time.Millisecond, func(_ catalog.DriftReport) {
		calls.Add(1)
	}, nil)
	time.Sleep(15 * time.Millisecond) // let one tick fire
	stop()                            // must block until in-flight CheckDrift completes
	snapshot := calls.Load()
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != snapshot {
		t.Errorf("onReport fired after stop() returned: %d → %d", snapshot, calls.Load())
	}
}

// slowDriftBackend makes ListProductsByNamespace block briefly so the
// goroutine has in-flight work when stop() fires.
type slowDriftBackend struct {
	fakeBackend
	delay time.Duration
}

func (b *slowDriftBackend) ListProductsByNamespace(ctx context.Context, ns string) ([]catalog.ExistingProduct, error) {
	select {
	case <-time.After(b.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return b.fakeBackend.ListProductsByNamespace(ctx, ns)
}

// --- helpers ---

type contextErr string

func (e contextErr) Error() string { return string(e) }

type erroringDriftBackend struct {
	err error
}

func (b *erroringDriftBackend) ListProductsByNamespace(_ context.Context, _ string) ([]catalog.ExistingProduct, error) {
	return nil, b.err
}
func (b *erroringDriftBackend) CreateProduct(_ context.Context, _ catalog.NewProduct) (catalog.ExistingProduct, error) {
	return catalog.ExistingProduct{}, nil
}
func (b *erroringDriftBackend) UpdateProduct(_ context.Context, _ string, _ catalog.ProductUpdate) error {
	return nil
}
func (b *erroringDriftBackend) UpdateProductActive(_ context.Context, _ string, _ bool) error {
	return nil
}
func (b *erroringDriftBackend) CreatePrice(_ context.Context, _ catalog.NewPrice) (catalog.ExistingPrice, error) {
	return catalog.ExistingPrice{}, nil
}
func (b *erroringDriftBackend) UpdatePriceActive(_ context.Context, _ string, _ bool) error {
	return nil
}
func (b *erroringDriftBackend) ListMetersByNamespace(_ context.Context, _ string) ([]catalog.ExistingMeter, error) {
	return nil, nil
}
func (b *erroringDriftBackend) CreateMeter(_ context.Context, _ catalog.NewMeter) (catalog.ExistingMeter, error) {
	return catalog.ExistingMeter{}, nil
}
func (b *erroringDriftBackend) ArchiveMeter(_ context.Context, _ string) error { return nil }

// --- Slice 36 follow-up: DriftReport.Apply + FormatHuman ---

func TestDriftReport_FormatHuman_EmptyForNoDrift(t *testing.T) {
	rep := catalog.DriftReport{Namespace: "x", HasDrift: false}
	if rep.FormatHuman() != "" {
		t.Errorf("expected empty string when HasDrift=false, got %q", rep.FormatHuman())
	}
}

func TestDriftReport_FormatHuman_MentionsNamespaceAndItems(t *testing.T) {
	be := &fakeBackend{}
	rep, _ := catalog.CheckDrift(t.Context(), be, driftSpec())
	out := rep.FormatHuman()
	if !strings.Contains(out, "drift_demo") {
		t.Errorf("namespace missing from FormatHuman: %q", out)
	}
	if !strings.Contains(out, "need attention") {
		t.Errorf("FormatHuman wording broke: %q", out)
	}
}

func TestDriftReport_Apply_NoDriftIsNoop(t *testing.T) {
	be := &fakeBackend{}
	rep := catalog.DriftReport{Namespace: "demo", HasDrift: false}
	if err := rep.Apply(t.Context(), be); err != nil {
		t.Errorf("Apply with no drift should no-op, got %v", err)
	}
	if len(be.createdProducts) != 0 {
		t.Errorf("expected no backend calls, got %d", len(be.createdProducts))
	}
}

func TestDriftReport_Apply_DrivesBackendFromReport(t *testing.T) {
	be := &fakeBackend{}
	rep, err := catalog.CheckDrift(t.Context(), be, driftSpec())
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if !rep.HasDrift {
		t.Skip("setup expected drift")
	}
	if err := rep.Apply(t.Context(), be); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(be.createdProducts) == 0 {
		t.Error("Apply did not drive any backend creates")
	}
}
