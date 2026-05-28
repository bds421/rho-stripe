// Package climate wraps Stripe's Climate Orders API for apps that
// want to programmatically purchase carbon removal contributions.
//
// Stripe Climate has two adoption shapes:
//
//  1. **Dashboard-configured** — set a % of revenue toward Climate
//     in Stripe Dashboard → Climate; Stripe handles it
//     automatically. No code needed; the library doesn't interfere.
//
//  2. **API-driven** — purchase specific tonnage / amounts per
//     action (e.g. "for every paid order, also buy €0.10 of carbon
//     removal"). This package wraps that flow.
//
// Apps wire option (2) inside their OnPaymentSucceeded handler:
//
//	conn.Climate.PurchaseAmount(ctx, 10, "eur", climate.Beneficiary{
//	    PublicName: "Acme Customers",
//	})
//
// Each purchase becomes a Stripe Climate Order — apps see them in
// Dashboard → Climate → Orders, and Stripe sends `climate.order.*`
// events on lifecycle changes (delivered, canceled, etc.).
package climate

import (
	"context"
	"errors"
	"fmt"
	"time"

	stripe "github.com/stripe/stripe-go/v82"
)

// Operations is the public surface. Constructed by the connector.
type Operations struct {
	sc *stripe.Client
}

// New wraps a Stripe client. Panics if sc is nil — matches lib-wide
// constructor convention so adopters can rely on a non-nil return.
func New(sc *stripe.Client) *Operations {
	if sc == nil {
		panic("climate.New: StripeClient is required")
	}
	return &Operations{sc: sc}
}

// Beneficiary is the public-facing record of who the carbon removal
// is being purchased for. Optional; defaults to your Stripe account.
type Beneficiary struct {
	// PublicName appears in Stripe's Climate dashboard + on the
	// public Climate.com page. Apps usually pass their customer's
	// org name or "<App> Users".
	PublicName string
}

// Order is the projection of a created Climate Order.
type Order struct {
	StripeID   string
	Amount     int64
	Currency   string
	MetricTons float64
	CreatedAt  time.Time
	// Status mirrors Stripe's climate order status; "pending",
	// "confirmed", "canceled", "fulfilled".
	Status string
}

// PurchaseAmount buys carbon removal worth `amount` in the smallest
// unit of `currency`. Stripe converts to metric tons internally
// using the current rate. Use this when your app wants
// "spend N cents on carbon for each transaction" semantics.
func (o *Operations) PurchaseAmount(ctx context.Context, amount int64, currency string, b Beneficiary) (Order, error) {
	if amount <= 0 {
		return Order{}, errors.New("climate.PurchaseAmount: amount must be > 0")
	}
	if currency == "" {
		return Order{}, errors.New("climate.PurchaseAmount: currency required")
	}
	params := &stripe.ClimateOrderCreateParams{
		Amount:   stripe.Int64(amount),
		Currency: stripe.String(currency),
	}
	if b.PublicName != "" {
		params.Beneficiary = &stripe.ClimateOrderCreateBeneficiaryParams{
			PublicName: stripe.String(b.PublicName),
		}
	}
	order, err := o.sc.V1ClimateOrders.Create(ctx, params)
	if err != nil {
		return Order{}, fmt.Errorf("stripe.climate.Order.Create: %w", err)
	}
	return projectOrder(order), nil
}

// PurchaseMetricTons buys an exact tonnage of carbon removal,
// regardless of cost. Useful for sustainability commitments
// ("we offset every employee's commute → 0.5 tons/month/employee").
func (o *Operations) PurchaseMetricTons(ctx context.Context, tons float64, b Beneficiary) (Order, error) {
	if tons <= 0 {
		return Order{}, errors.New("climate.PurchaseMetricTons: tons must be > 0")
	}
	params := &stripe.ClimateOrderCreateParams{
		MetricTons: stripe.Float64(tons),
	}
	if b.PublicName != "" {
		params.Beneficiary = &stripe.ClimateOrderCreateBeneficiaryParams{
			PublicName: stripe.String(b.PublicName),
		}
	}
	order, err := o.sc.V1ClimateOrders.Create(ctx, params)
	if err != nil {
		return Order{}, fmt.Errorf("stripe.climate.Order.Create: %w", err)
	}
	return projectOrder(order), nil
}

func projectOrder(o *stripe.ClimateOrder) Order {
	return Order{
		StripeID:   o.ID,
		Amount:     o.AmountTotal,
		Currency:   string(o.Currency),
		MetricTons: o.MetricTons,
		CreatedAt:  time.Unix(o.Created, 0).UTC(),
		Status:     string(o.Status),
	}
}
