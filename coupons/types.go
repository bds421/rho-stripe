// Package coupons exposes runtime management of customer-facing
// promotion codes. Coupons themselves are declared in the catalog
// and synced via `rho-stripe sync`; promo codes are
// operationally fluid (campaigns, customer-support-issued codes,
// etc.) so they live here, not in the catalog.
package coupons

import "time"

// PromoCode is the public projection of a Stripe promotion_code.
type PromoCode struct {
	ID             string // "promo_…" — Stripe's internal id
	Code           string // the customer-facing string ("SAVE20"); auto-generated if absent at create-time
	CouponID       string // resolved Stripe coupon id (e.g. "SAVE20_demo")
	Active         bool
	MaxRedemptions int64 // 0 = unlimited
	TimesRedeemed  int64
	ExpiresAt      *time.Time
	Restrictions   *Restrictions
	BoundCustomer  string // Stripe Customer id (cus_…); same underlying value as checkout.StripeCustomerID (both are typedef strings). Assign via string(myStripeCustomerID).
	Metadata       map[string]string
	Created        time.Time
}

// CreateInput is the input shape for Operations.CreatePromoCode.
// CouponKey is catalog-relative (e.g. "SAVE20"); the lib namespaces
// it via the spec.
type CreateInput struct {
	CouponKey      string
	Code           string // empty → Stripe generates a random code
	MaxRedemptions int64
	ExpiresAt      time.Time
	BoundCustomer  string // Stripe Customer id; empty = unrestricted
	Restrictions   *Restrictions
	Metadata       map[string]string
}

// Restrictions narrow when/how a promo code can be redeemed.
type Restrictions struct {
	FirstTimeTransaction  bool
	MinimumAmount         int64  // smallest currency unit
	MinimumAmountCurrency string // required if MinimumAmount > 0
}

// ListInput filters Operations.ListPromoCodes results.
type ListInput struct {
	CouponKey  string // empty = all coupons
	ActiveOnly bool
	Limit      int64 // 0 = stripe-go default (10), max 100
}
