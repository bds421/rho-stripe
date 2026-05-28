package coupons_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bds421/rho-stripe/catalog"
	"github.com/bds421/rho-stripe/coupons"
)

type fakeBackend struct {
	mu          sync.Mutex
	created     []coupons.CreateParams
	listed      []coupons.ListParams
	deactivated []string

	listResult []coupons.PromoCode
	err        error
}

func (b *fakeBackend) CreatePromoCode(_ context.Context, p coupons.CreateParams) (coupons.PromoCode, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return coupons.PromoCode{}, b.err
	}
	b.created = append(b.created, p)
	return coupons.PromoCode{ID: "promo_fake", Code: p.Code, CouponID: p.CouponID, Active: true}, nil
}
func (b *fakeBackend) ListPromoCodes(_ context.Context, p coupons.ListParams) ([]coupons.PromoCode, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.listed = append(b.listed, p)
	return b.listResult, b.err
}
func (b *fakeBackend) DeactivatePromoCode(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deactivated = append(b.deactivated, id)
	return b.err
}

func couponSpec() *catalog.Spec {
	return catalog.MustSpec(catalog.Spec{
		Namespace: "demo",
		Products: map[string]catalog.Product{
			"pro_plan": {
				Name:        "Pro",
				TaxCategory: catalog.TaxCategorySaaS,
				Prices:      map[string]catalog.Price{"monthly_eur": {Amount: 4900, Currency: "eur", Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth}},
			},
		},
		Coupons: map[string]catalog.Coupon{
			"SAVE20": {PercentOff: 20, Duration: catalog.CouponDurationOnce},
		},
	})
}

func TestCreatePromoCode_NamespacesCoupon(t *testing.T) {
	be := &fakeBackend{}
	ops := coupons.New(coupons.Config{Backend: be, Spec: couponSpec()})
	_, err := ops.CreatePromoCode(t.Context(), coupons.CreateInput{
		CouponKey: "SAVE20", Code: "SUMMER",
	})
	if err != nil {
		t.Fatalf("CreatePromoCode: %v", err)
	}
	if be.created[0].CouponID != "SAVE20_demo" {
		t.Errorf("coupon should be namespaced; got %q want SAVE20_demo", be.created[0].CouponID)
	}
}

func TestCreatePromoCode_RejectsUnknownCoupon(t *testing.T) {
	ops := coupons.New(coupons.Config{Backend: &fakeBackend{}, Spec: couponSpec()})
	_, err := ops.CreatePromoCode(t.Context(), coupons.CreateInput{CouponKey: "NOPE"})
	if !errors.Is(err, coupons.ErrCouponNotInCatalog) {
		t.Errorf("expected ErrCouponNotInCatalog, got %v", err)
	}
}

func TestCreatePromoCode_RequiresCouponKey(t *testing.T) {
	ops := coupons.New(coupons.Config{Backend: &fakeBackend{}, Spec: couponSpec()})
	_, err := ops.CreatePromoCode(t.Context(), coupons.CreateInput{})
	if err == nil {
		t.Error("expected error when CouponKey is empty")
	}
}

func TestCreatePromoCode_ExpiresAtConverted(t *testing.T) {
	be := &fakeBackend{}
	ops := coupons.New(coupons.Config{Backend: be, Spec: couponSpec()})
	expiry := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
	_, _ = ops.CreatePromoCode(t.Context(), coupons.CreateInput{CouponKey: "SAVE20", ExpiresAt: expiry})
	if be.created[0].ExpiresAt != expiry.Unix() {
		t.Errorf("ExpiresAt not converted to unix; got %d want %d", be.created[0].ExpiresAt, expiry.Unix())
	}
}

func TestListPromoCodes_NamespacesCouponFilter(t *testing.T) {
	be := &fakeBackend{listResult: []coupons.PromoCode{{ID: "promo_x"}}}
	ops := coupons.New(coupons.Config{Backend: be, Spec: couponSpec()})
	got, _ := ops.ListPromoCodes(t.Context(), coupons.ListInput{CouponKey: "SAVE20", ActiveOnly: true})
	if be.listed[0].CouponID != "SAVE20_demo" {
		t.Errorf("filter CouponID should namespace; got %q", be.listed[0].CouponID)
	}
	if !be.listed[0].ActiveOnly {
		t.Error("ActiveOnly not passed through")
	}
	if len(got) != 1 {
		t.Errorf("got %d codes, want 1", len(got))
	}
}

func TestDeactivatePromoCode(t *testing.T) {
	be := &fakeBackend{}
	ops := coupons.New(coupons.Config{Backend: be, Spec: couponSpec()})
	if err := ops.DeactivatePromoCode(t.Context(), "promo_abc"); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if be.deactivated[0] != "promo_abc" {
		t.Errorf("deactivated %v", be.deactivated)
	}
}

func TestDeactivatePromoCode_RequiresID(t *testing.T) {
	ops := coupons.New(coupons.Config{Backend: &fakeBackend{}, Spec: couponSpec()})
	if err := ops.DeactivatePromoCode(t.Context(), ""); err == nil {
		t.Error("expected error for empty promoID")
	}
}
