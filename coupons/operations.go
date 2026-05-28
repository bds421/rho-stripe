package coupons

import (
	"context"
	"errors"
	"fmt"

	"github.com/bds421/rho-stripe/catalog"
	"github.com/bds421/rho-stripe/meta"
)

// Operations is the public surface apps use to mint and manage promo
// codes. Constructed via New; the connector facade builds one
// automatically when SecretKey is set.
type Operations struct {
	backend Backend
	spec    *catalog.Spec
}

// Config wires the package's dependencies (lib-wide convention:
// typed Config for any constructor with >1 dep).
type Config struct {
	// Backend is the Stripe-side adapter. Required.
	Backend Backend
	// Spec is the catalog used to namespace CouponKeys
	// ("SAVE20" → "SAVE20_<namespace>"). Required.
	Spec *catalog.Spec
}

// New constructs Operations from cfg.
func New(cfg Config) *Operations {
	if cfg.Backend == nil {
		panic("coupons.New: Backend is required")
	}
	if cfg.Spec == nil {
		panic("coupons.New: Spec is required")
	}
	return &Operations{backend: cfg.Backend, spec: cfg.Spec}
}

var (
	ErrCouponNotInCatalog = errors.New("coupons: coupon key not declared in catalog spec")
)

// CreatePromoCode creates a Stripe promotion code referencing the
// catalog-declared coupon with key CouponKey.
func (o *Operations) CreatePromoCode(ctx context.Context, in CreateInput) (PromoCode, error) {
	if in.CouponKey == "" {
		return PromoCode{}, errors.New("coupons: CreateInput.CouponKey is required")
	}
	if _, ok := o.spec.Coupons[in.CouponKey]; !ok {
		return PromoCode{}, fmt.Errorf("%w: %q", ErrCouponNotInCatalog, in.CouponKey)
	}

	params := CreateParams{
		CouponID:       o.spec.NamespacedCouponID(in.CouponKey),
		Code:           in.Code,
		MaxRedemptions: in.MaxRedemptions,
		BoundCustomer:  in.BoundCustomer,
		Restrictions:   in.Restrictions,
		Metadata:       stampNamespace(o.spec.Namespace, in.Metadata),
	}
	if !in.ExpiresAt.IsZero() {
		params.ExpiresAt = in.ExpiresAt.Unix()
	}
	return o.backend.CreatePromoCode(ctx, params)
}

// ListPromoCodes returns promo codes matching the filter. Pass empty
// ListInput to list everything (paginated up to Limit / Stripe default).
func (o *Operations) ListPromoCodes(ctx context.Context, in ListInput) ([]PromoCode, error) {
	params := ListParams{
		ActiveOnly: in.ActiveOnly,
		Limit:      in.Limit,
	}
	if in.CouponKey != "" {
		if _, ok := o.spec.Coupons[in.CouponKey]; !ok {
			return nil, fmt.Errorf("%w: %q", ErrCouponNotInCatalog, in.CouponKey)
		}
		params.CouponID = o.spec.NamespacedCouponID(in.CouponKey)
	}
	return o.backend.ListPromoCodes(ctx, params)
}

// DeactivatePromoCode sets the code's active flag to false. Stripe
// doesn't allow deletion — deactivation is the close equivalent.
// Customers attempting to use a deactivated code at checkout see
// "code not valid."
func (o *Operations) DeactivatePromoCode(ctx context.Context, promoID string) error {
	if promoID == "" {
		return errors.New("coupons: promoID is required")
	}
	return o.backend.DeactivatePromoCode(ctx, promoID)
}

// stampNamespace is a package-local alias of [meta.StampNamespace]
// kept for receiver-method-style call-site ergonomics inside this
// package. The wire-format key + stamping logic live in stripeapi so
// every writer agrees with the webhook dispatcher's reader.
func stampNamespace(ns string, base map[string]string) map[string]string {
	return meta.StampNamespace(base, ns)
}
