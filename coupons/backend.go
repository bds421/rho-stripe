package coupons

import "context"

// Backend is the Stripe-side surface for promo-code management. The
// default implementation lives in stripeapi; tests inject fakes.
type Backend interface {
	CreatePromoCode(ctx context.Context, p CreateParams) (PromoCode, error)
	ListPromoCodes(ctx context.Context, p ListParams) ([]PromoCode, error)
	DeactivatePromoCode(ctx context.Context, promoID string) error
}

// CreateParams is the Backend-level input — all fields are already
// resolved (CouponID namespaced, restrictions normalized).
type CreateParams struct {
	CouponID       string
	Code           string
	MaxRedemptions int64
	ExpiresAt      int64 // unix seconds; 0 = no expiry
	BoundCustomer  string
	Restrictions   *Restrictions
	Metadata       map[string]string
}

// ListParams is the Backend-level filter.
type ListParams struct {
	CouponID   string
	ActiveOnly bool
	Limit      int64
}
