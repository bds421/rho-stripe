package catalog_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/bds421/rho-stripe/catalog"
)

// fakeBackend records calls and returns canned results.
type fakeBackend struct {
	mu sync.Mutex

	productsListed   bool
	createdProducts  []catalog.NewProduct
	updatedProducts  []productUpdateRecord
	createdPrices    []catalog.NewPrice
	archivedProducts []string
	archivedPrices   []string

	listResult []catalog.ExistingProduct
	createErr  error
}

type productUpdateRecord struct {
	ID     string
	Update catalog.ProductUpdate
}

func (f *fakeBackend) ListProductsByNamespace(_ context.Context, _ string) ([]catalog.ExistingProduct, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.productsListed = true
	return f.listResult, nil
}

func (f *fakeBackend) CreateProduct(_ context.Context, p catalog.NewProduct) (catalog.ExistingProduct, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createdProducts = append(f.createdProducts, p)
	if f.createErr != nil {
		return catalog.ExistingProduct{}, f.createErr
	}
	return catalog.ExistingProduct{ID: p.ID, Name: p.Name, Active: true}, nil
}

func (f *fakeBackend) UpdateProduct(_ context.Context, id string, u catalog.ProductUpdate) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updatedProducts = append(f.updatedProducts, productUpdateRecord{ID: id, Update: u})
	return nil
}

func (f *fakeBackend) UpdateProductActive(_ context.Context, id string, active bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !active {
		f.archivedProducts = append(f.archivedProducts, id)
	}
	return nil
}

func (f *fakeBackend) CreatePrice(_ context.Context, p catalog.NewPrice) (catalog.ExistingPrice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createdPrices = append(f.createdPrices, p)
	if f.createErr != nil {
		return catalog.ExistingPrice{}, f.createErr
	}
	return catalog.ExistingPrice{
		ID: "price_fake_" + p.LookupKey, LookupKey: p.LookupKey, Active: true,
	}, nil
}

func (f *fakeBackend) UpdatePriceActive(_ context.Context, id string, active bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !active {
		f.archivedPrices = append(f.archivedPrices, id)
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

func TestApply_EmptyStripeCreatesAll(t *testing.T) {
	spec := twoProductSpec()
	be := &fakeBackend{}
	plan := catalog.Diff(spec, nil)

	if err := catalog.Apply(t.Context(), be, plan); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if len(be.createdProducts) != 2 {
		t.Errorf("expected 2 products created, got %d", len(be.createdProducts))
	}
	if len(be.createdPrices) != 3 {
		t.Errorf("expected 3 prices created, got %d", len(be.createdPrices))
	}
	if len(be.archivedProducts) != 0 || len(be.archivedPrices) != 0 {
		t.Errorf("expected 0 archives, got product=%d price=%d",
			len(be.archivedProducts), len(be.archivedPrices))
	}
}

func TestApply_OrderProductsBeforePrices(t *testing.T) {
	spec := twoProductSpec()
	be := &orderTrackingBackend{}
	if err := catalog.Apply(t.Context(), be, catalog.Diff(spec, nil)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Every "create price" call must come AFTER the matching product was created.
	for i, op := range be.calls {
		if !strings.HasPrefix(op, "create_price:") {
			continue
		}
		productID := strings.TrimPrefix(op, "create_price:")
		productID = strings.SplitN(productID, ":", 2)[0]
		// Find the matching create_product earlier in the list.
		found := false
		for j := 0; j < i; j++ {
			if be.calls[j] == "create_product:"+productID {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("price for %q was created before its product: %v", productID, be.calls)
		}
	}
}

func TestApply_ArchivesPricesBeforeProducts(t *testing.T) {
	spec := twoProductSpec()
	current := stripeStateMatching(spec)
	delete(spec.Products, "starter")

	be := &orderTrackingBackend{}
	if err := catalog.Apply(t.Context(), be, catalog.Diff(spec, current)); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	var firstProductArchive, lastPriceArchive int = -1, -1
	for i, op := range be.calls {
		if strings.HasPrefix(op, "archive_price:") {
			lastPriceArchive = i
		}
		if firstProductArchive < 0 && strings.HasPrefix(op, "archive_product:") {
			firstProductArchive = i
		}
	}
	if firstProductArchive < 0 || lastPriceArchive < 0 {
		t.Fatalf("expected both price and product archives; calls=%v", be.calls)
	}
	if lastPriceArchive > firstProductArchive {
		t.Errorf("price archive after product archive: %v", be.calls)
	}
}

func TestApply_IdempotentReRun(t *testing.T) {
	spec := twoProductSpec()
	be := &fakeBackend{}

	if err := catalog.Apply(t.Context(), be, catalog.Diff(spec, nil)); err != nil {
		t.Fatalf("first Apply: %v", err)
	}

	// Simulate post-sync Stripe state and re-run.
	be.listResult = stripeStateMatching(spec)
	before := len(be.createdProducts) + len(be.createdPrices)
	if err := catalog.Apply(t.Context(), be, catalog.Diff(spec, be.listResult)); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	after := len(be.createdProducts) + len(be.createdPrices)
	if after != before {
		t.Errorf("idempotent re-run created more items: before=%d after=%d", before, after)
	}
}

func TestApply_ReplaceCreatesNewThenArchivesOld(t *testing.T) {
	spec := twoProductSpec()
	current := stripeStateMatching(spec)
	// Bump pro_plan.monthly_eur amount from 4900 to 5900.
	pro := spec.Products["pro_plan"]
	monthly := pro.Prices["monthly_eur"]
	monthly.Amount = 5900
	pro.Prices["monthly_eur"] = monthly
	spec.Products["pro_plan"] = pro

	be := &fakeBackend{}
	plan := catalog.Diff(spec, current)
	if err := catalog.Apply(t.Context(), be, plan); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if len(be.createdPrices) != 1 {
		t.Fatalf("expected 1 new price created, got %d", len(be.createdPrices))
	}
	if !be.createdPrices[0].TransferLookupKey {
		t.Error("created price should have TransferLookupKey=true")
	}
	if be.createdPrices[0].Amount != 5900 {
		t.Errorf("new price amount = %d, want 5900", be.createdPrices[0].Amount)
	}
	if len(be.archivedPrices) != 1 {
		t.Errorf("expected 1 price archived, got %d", len(be.archivedPrices))
	}
}

func TestApply_UpdateProductCallsBackend(t *testing.T) {
	spec := twoProductSpec()
	current := stripeStateMatching(spec)
	// Rename pro_plan in the spec.
	pro := spec.Products["pro_plan"]
	pro.Name = "Pro Plan (Renamed)"
	spec.Products["pro_plan"] = pro

	be := &fakeBackend{}
	plan := catalog.Diff(spec, current)
	if err := catalog.Apply(t.Context(), be, plan); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if len(be.updatedProducts) != 1 {
		t.Fatalf("expected 1 product update, got %d", len(be.updatedProducts))
	}
	if be.updatedProducts[0].Update.Name != "Pro Plan (Renamed)" {
		t.Errorf("update.Name = %q, want %q", be.updatedProducts[0].Update.Name, "Pro Plan (Renamed)")
	}
}

func TestApply_StopsOnError(t *testing.T) {
	spec := twoProductSpec()
	be := &fakeBackend{createErr: errors.New("rate limited")}
	plan := catalog.Diff(spec, nil)

	err := catalog.Apply(t.Context(), be, plan)
	if err == nil {
		t.Fatal("expected error from Apply, got nil")
	}
	if !errors.Is(err, be.createErr) {
		t.Errorf("error not wrapped: %v", err)
	}
}

// orderTrackingBackend records the sequence of mutation calls for
// ordering-assertion tests.
type orderTrackingBackend struct {
	calls []string
}

func (b *orderTrackingBackend) ListProductsByNamespace(_ context.Context, _ string) ([]catalog.ExistingProduct, error) {
	return nil, nil
}
func (b *orderTrackingBackend) CreateProduct(_ context.Context, p catalog.NewProduct) (catalog.ExistingProduct, error) {
	b.calls = append(b.calls, "create_product:"+p.ID)
	return catalog.ExistingProduct{ID: p.ID, Active: true}, nil
}
func (b *orderTrackingBackend) UpdateProduct(_ context.Context, id string, _ catalog.ProductUpdate) error {
	b.calls = append(b.calls, "update_product:"+id)
	return nil
}
func (b *orderTrackingBackend) UpdateProductActive(_ context.Context, id string, active bool) error {
	if !active {
		b.calls = append(b.calls, "archive_product:"+id)
	}
	return nil
}
func (b *orderTrackingBackend) CreatePrice(_ context.Context, p catalog.NewPrice) (catalog.ExistingPrice, error) {
	b.calls = append(b.calls, "create_price:"+p.ProductID+":"+p.LookupKey)
	return catalog.ExistingPrice{ID: "price_x_" + p.LookupKey, LookupKey: p.LookupKey, Active: true}, nil
}
func (b *orderTrackingBackend) UpdatePriceActive(_ context.Context, id string, active bool) error {
	if !active {
		b.calls = append(b.calls, "archive_price:"+id)
	}
	return nil
}
func (b *orderTrackingBackend) ListMetersByNamespace(_ context.Context, _ string) ([]catalog.ExistingMeter, error) {
	return nil, nil
}
func (b *orderTrackingBackend) CreateMeter(_ context.Context, _ catalog.NewMeter) (catalog.ExistingMeter, error) {
	return catalog.ExistingMeter{}, nil
}
func (b *orderTrackingBackend) ArchiveMeter(_ context.Context, _ string) error { return nil }
