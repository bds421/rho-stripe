package stripeapi

import (
	"context"
	"fmt"

	"github.com/bds421/rho-stripe/coupons"
	stripe "github.com/stripe/stripe-go/v82"
)

// CouponBackend implements coupons.Backend against stripe-go's
// promotion_code API.
type CouponBackend struct {
	sc *stripe.Client
}

// NewCouponBackend wraps a stripe-go client.
func NewCouponBackend(sc *stripe.Client) *CouponBackend {
	if sc == nil {
		panic("stripeapi.NewCouponBackend: StripeClient is required")
	}
	return &CouponBackend{sc: sc}
}

var _ coupons.Backend = (*CouponBackend)(nil)

func (b *CouponBackend) CreatePromoCode(ctx context.Context, p coupons.CreateParams) (coupons.PromoCode, error) {
	params := &stripe.PromotionCodeCreateParams{
		Coupon: stripe.String(p.CouponID),
	}
	if p.Code != "" {
		params.Code = stripe.String(p.Code)
	}
	if p.MaxRedemptions > 0 {
		params.MaxRedemptions = stripe.Int64(p.MaxRedemptions)
	}
	if p.ExpiresAt > 0 {
		params.ExpiresAt = stripe.Int64(p.ExpiresAt)
	}
	if p.BoundCustomer != "" {
		params.Customer = stripe.String(p.BoundCustomer)
	}
	if p.Restrictions != nil {
		params.Restrictions = &stripe.PromotionCodeCreateRestrictionsParams{}
		if p.Restrictions.FirstTimeTransaction {
			params.Restrictions.FirstTimeTransaction = stripe.Bool(true)
		}
		if p.Restrictions.MinimumAmount > 0 {
			params.Restrictions.MinimumAmount = stripe.Int64(p.Restrictions.MinimumAmount)
			if p.Restrictions.MinimumAmountCurrency != "" {
				params.Restrictions.MinimumAmountCurrency = stripe.String(p.Restrictions.MinimumAmountCurrency)
			}
		}
	}
	for k, v := range p.Metadata {
		params.AddMetadata(k, v)
	}
	applyIdem(params, ctx, "promo_code.create",
		p.CouponID, p.Code, canonicalInt64(p.MaxRedemptions),
		canonicalInt64(p.ExpiresAt), p.BoundCustomer, canonicalMap(p.Metadata))

	created, err := b.sc.V1PromotionCodes.Create(ctx, params)
	if err != nil {
		return coupons.PromoCode{}, fmt.Errorf("stripe.PromotionCodes.Create: %w", err)
	}
	return projectPromoCode(created), nil
}

func (b *CouponBackend) ListPromoCodes(ctx context.Context, p coupons.ListParams) ([]coupons.PromoCode, error) {
	params := &stripe.PromotionCodeListParams{}
	if p.CouponID != "" {
		params.Coupon = stripe.String(p.CouponID)
	}
	if p.ActiveOnly {
		params.Active = stripe.Bool(true)
	}
	if p.Limit > 0 {
		params.Filters.AddFilter("limit", "", fmt.Sprintf("%d", p.Limit))
	}

	var out []coupons.PromoCode
	for pc, err := range b.sc.V1PromotionCodes.List(ctx, params) {
		if err != nil {
			return nil, fmt.Errorf("stripe.PromotionCodes.List: %w", err)
		}
		out = append(out, projectPromoCode(pc))
	}
	return out, nil
}

func (b *CouponBackend) DeactivatePromoCode(ctx context.Context, promoID string) error {
	params := &stripe.PromotionCodeUpdateParams{Active: stripe.Bool(false)}
	applyIdem(params, ctx, "promo_code.deactivate", promoID)
	if _, err := b.sc.V1PromotionCodes.Update(ctx, promoID, params); err != nil {
		return fmt.Errorf("stripe.PromotionCodes.Update(%s, active=false): %w", promoID, err)
	}
	return nil
}

func projectPromoCode(p *stripe.PromotionCode) coupons.PromoCode {
	pc := coupons.PromoCode{
		ID:             p.ID,
		Code:           p.Code,
		Active:         p.Active,
		MaxRedemptions: p.MaxRedemptions,
		TimesRedeemed:  p.TimesRedeemed,
		Metadata:       p.Metadata,
		Created:        unixOrZero(p.Created),
	}
	if p.Coupon != nil {
		pc.CouponID = p.Coupon.ID
	}
	if p.Customer != nil {
		pc.BoundCustomer = p.Customer.ID
	}
	if p.ExpiresAt > 0 {
		t := unixOrZero(p.ExpiresAt)
		pc.ExpiresAt = &t
	}
	if p.Restrictions != nil {
		pc.Restrictions = &coupons.Restrictions{
			FirstTimeTransaction:  p.Restrictions.FirstTimeTransaction,
			MinimumAmount:         p.Restrictions.MinimumAmount,
			MinimumAmountCurrency: string(p.Restrictions.MinimumAmountCurrency),
		}
	}
	return pc
}

