package stripeapi

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/bds421/rho-stripe/catalog"
	stripe "github.com/stripe/stripe-go/v82"
)

// Backend implements catalog.Backend against the live Stripe API via
// stripe-go. Construct it with NewBackend after creating a *stripe.Client
// via NewClient.
type Backend struct {
	sc *stripe.Client
}

// NewBackend wraps a stripe-go client as a catalog.Backend.
func NewBackend(sc *stripe.Client) *Backend {
	if sc == nil {
		panic("stripeapi.NewBackend: StripeClient is required")
	}
	return &Backend{sc: sc}
}

// Compile-time interface checks.
var (
	_ catalog.Backend     = (*Backend)(nil)
	_ catalog.PriceLister = (*Backend)(nil)
)

// ListByLookupKeys satisfies catalog.PriceLister so the same Backend
// can warm the catalog cache. Stripe's price list accepts up to 10
// lookup_keys per call, so larger spec catalogs are batched.
func (b *Backend) ListByLookupKeys(ctx context.Context, keys []string) ([]catalog.ResolvedPrice, error) {
	const batchSize = 10
	var out []catalog.ResolvedPrice
	for start := 0; start < len(keys); start += batchSize {
		end := start + batchSize
		if end > len(keys) {
			end = len(keys)
		}
		batch, err := b.listByLookupKeysBatch(ctx, keys[start:end])
		if err != nil {
			return nil, fmt.Errorf("list lookup_keys batch %d..%d: %w", start, end, err)
		}
		out = append(out, batch...)
	}
	return out, nil
}

func (b *Backend) listByLookupKeysBatch(ctx context.Context, keys []string) ([]catalog.ResolvedPrice, error) {
	params := &stripe.PriceListParams{
		LookupKeys: stripe.StringSlice(keys),
	}
	params.Filters.AddFilter("limit", "", "100")

	var out []catalog.ResolvedPrice
	for p, err := range b.sc.V1Prices.List(ctx, params) {
		if err != nil {
			return nil, err
		}
		productID := ""
		if p.Product != nil {
			productID = p.Product.ID
		}
		out = append(out, catalog.ResolvedPrice{
			LookupKey: p.LookupKey,
			PriceID:   p.ID,
			ProductID: productID,
			Active:    p.Active,
		})
	}
	return out, nil
}

// ListProductsByNamespace fetches every Product whose ID is prefixed by
// "prod_<namespace>_" (both active and archived), along with all of
// each product's Prices. Stripe's API does not filter by ID prefix
// server-side, so we paginate over all products and filter client-side.
func (b *Backend) ListProductsByNamespace(ctx context.Context, namespace string) ([]catalog.ExistingProduct, error) {
	prefix := "prod_" + namespace + "_"

	params := &stripe.ProductListParams{
		Active: nil, // unset → include both active and archived
	}
	params.Filters.AddFilter("limit", "", "100")

	var out []catalog.ExistingProduct
	for p, err := range b.sc.V1Products.List(ctx, params) {
		if err != nil {
			return nil, fmt.Errorf("list products: %w", err)
		}
		if !strings.HasPrefix(p.ID, prefix) {
			continue
		}
		prices, err := b.listPricesForProduct(ctx, p.ID)
		if err != nil {
			return nil, fmt.Errorf("list prices for %s: %w", p.ID, err)
		}
		out = append(out, catalog.ExistingProduct{
			ID:          p.ID,
			Name:        p.Name,
			TaxCode:     productTaxCode(p),
			Active:      p.Active,
			Description: p.Description,
			Metadata:    p.Metadata,
			Prices:      prices,
		})
	}
	return out, nil
}

func (b *Backend) listPricesForProduct(ctx context.Context, productID string) ([]catalog.ExistingPrice, error) {
	params := &stripe.PriceListParams{
		Product: stripe.String(productID),
	}
	params.Filters.AddFilter("limit", "", "100")
	// REGRESSION GUARD (do not re-add an `active` filter here):
	//
	// Stripe's API rejects empty-string booleans with HTTP 400
	// `Invalid boolean: `. An earlier version of this code had:
	//
	//   params.Filters.AddFilter("active", "", "") // explicit empty
	//
	// thinking it meant "no filter on active." Stripe interpreted it
	// as `active=""` and returned 400 on the SECOND sync — when
	// products existed and listPricesForProduct was actually called.
	// Unit tests with fake stripe-go clients passed both ways.
	//
	// To express "include both active and archived prices," OMIT the
	// `active` parameter entirely. See CONTRIBUTING.md "never pass
	// empty-string booleans to Stripe."

	var prices []catalog.ExistingPrice
	for p, err := range b.sc.V1Prices.List(ctx, params) {
		if err != nil {
			return nil, err
		}
		prices = append(prices, catalog.ExistingPrice{
			ID:            p.ID,
			LookupKey:     p.LookupKey,
			Active:        p.Active,
			Amount:        p.UnitAmount,
			Currency:      string(p.Currency),
			Type:          catalog.PriceType(p.Type),
			Interval:      recurringInterval(p),
			IntervalCount: recurringIntervalCount(p),
			Metadata:      p.Metadata,
		})
	}
	return prices, nil
}

// CreateProduct creates the product in Stripe with a custom ID.
func (b *Backend) CreateProduct(ctx context.Context, p catalog.NewProduct) (catalog.ExistingProduct, error) {
	params := &stripe.ProductCreateParams{
		ID:          stripe.String(p.ID),
		Name:        stripe.String(p.Name),
		Description: nonEmptyStringPtr(p.Description),
		TaxCode:     nonEmptyStringPtr(p.TaxCode),
	}
	for k, v := range p.Metadata {
		params.AddMetadata(k, v)
	}
	applyIdem(params, ctx, "product.create", p.ID, p.Name, p.TaxCode, canonicalMap(p.Metadata))

	created, err := b.sc.V1Products.Create(ctx, params)
	if err != nil {
		return catalog.ExistingProduct{}, fmt.Errorf("stripe.Products.Create: %w", err)
	}
	return catalog.ExistingProduct{
		ID:          created.ID,
		Name:        created.Name,
		TaxCode:     productTaxCode(created),
		Active:      created.Active,
		Description: created.Description,
		Metadata:    created.Metadata,
	}, nil
}

// UpdateProduct applies a metadata-only update (Name/Description/Metadata).
func (b *Backend) UpdateProduct(ctx context.Context, productID string, u catalog.ProductUpdate) error {
	params := &stripe.ProductUpdateParams{}
	if u.Name != "" {
		params.Name = stripe.String(u.Name)
	}
	if u.Description != "" {
		params.Description = stripe.String(u.Description)
	}
	for k, v := range u.Metadata {
		params.AddMetadata(k, v)
	}
	applyIdem(params, ctx, "product.update", productID, u.Name, u.Description, canonicalMap(u.Metadata))
	if _, err := b.sc.V1Products.Update(ctx, productID, params); err != nil {
		return fmt.Errorf("stripe.Products.Update(%s): %w", productID, err)
	}
	return nil
}

// UpdateProductActive flips the product's active flag.
func (b *Backend) UpdateProductActive(ctx context.Context, productID string, active bool) error {
	params := &stripe.ProductUpdateParams{Active: stripe.Bool(active)}
	applyIdem(params, ctx, "product.set_active", productID, strconv.FormatBool(active))
	if _, err := b.sc.V1Products.Update(ctx, productID, params); err != nil {
		return fmt.Errorf("stripe.Products.Update(%s, active=%v): %w", productID, active, err)
	}
	return nil
}

// CreatePrice creates a Price, honoring TransferLookupKey for REPLACE.
func (b *Backend) CreatePrice(ctx context.Context, p catalog.NewPrice) (catalog.ExistingPrice, error) {
	params := &stripe.PriceCreateParams{
		Product:    stripe.String(p.ProductID),
		LookupKey:  stripe.String(p.LookupKey),
		Currency:   stripe.String(p.Currency),
		UnitAmount: stripe.Int64(p.Amount),
	}
	if p.TransferLookupKey {
		params.TransferLookupKey = stripe.Bool(true)
	}
	if p.Type == catalog.PriceTypeRecurring {
		params.Recurring = &stripe.PriceCreateRecurringParams{
			Interval:      stripe.String(string(p.Interval)),
			IntervalCount: stripe.Int64(int64(intervalCountOrOne(p.IntervalCount))),
		}
	}
	if p.TaxBehavior != "" {
		params.TaxBehavior = stripe.String(p.TaxBehavior)
	}
	for k, v := range p.Metadata {
		params.AddMetadata(k, v)
	}
	applyIdem(params, ctx, "price.create",
		p.ProductID, p.LookupKey, p.Currency, canonicalInt64(p.Amount),
		string(p.Type), string(p.Interval), canonicalInt64(int64(p.IntervalCount)),
		strconv.FormatBool(p.TransferLookupKey),
		p.TaxBehavior,
		canonicalMap(p.Metadata),
	)
	// Note: TaxRateOverrides aren't a Price-level field; they're
	// applied per Checkout/Subscription line item. The lib forwards
	// them via PlanItem → consumed by checkout / subscription
	// creation sites that read p.TaxRateOverrides off the catalog
	// Price (not via this Backend.CreatePrice call). Documented to
	// avoid confusion.

	created, err := b.sc.V1Prices.Create(ctx, params)
	if err != nil {
		return catalog.ExistingPrice{}, fmt.Errorf("stripe.Prices.Create(%s): %w", p.LookupKey, err)
	}
	return catalog.ExistingPrice{
		ID:            created.ID,
		LookupKey:     created.LookupKey,
		Active:        created.Active,
		Amount:        created.UnitAmount,
		Currency:      string(created.Currency),
		Type:          catalog.PriceType(created.Type),
		Interval:      recurringInterval(created),
		IntervalCount: recurringIntervalCount(created),
		Metadata:      created.Metadata,
	}, nil
}

// UpdatePriceActive flips a Price's active flag (archive / unarchive).
func (b *Backend) UpdatePriceActive(ctx context.Context, priceID string, active bool) error {
	params := &stripe.PriceUpdateParams{Active: stripe.Bool(active)}
	applyIdem(params, ctx, "price.set_active", priceID, strconv.FormatBool(active))
	if _, err := b.sc.V1Prices.Update(ctx, priceID, params); err != nil {
		return fmt.Errorf("stripe.Prices.Update(%s, active=%v): %w", priceID, active, err)
	}
	return nil
}

// --- helpers ---

func productTaxCode(p *stripe.Product) string {
	if p == nil || p.TaxCode == nil {
		return ""
	}
	return p.TaxCode.ID
}

func recurringInterval(p *stripe.Price) catalog.Interval {
	if p == nil || p.Recurring == nil {
		return ""
	}
	return catalog.Interval(p.Recurring.Interval)
}

func recurringIntervalCount(p *stripe.Price) int {
	if p == nil || p.Recurring == nil {
		return 0
	}
	return int(p.Recurring.IntervalCount)
}

func intervalCountOrOne(n int) int {
	if n <= 0 {
		return 1
	}
	return n
}

func nonEmptyStringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return stripe.String(s)
}

// --- Meter sync (phase 7) ---

// ListMetersByNamespace returns every Billing Meter whose event_name
// starts with `<namespace>.`. Stripe's API has no server-side prefix
// filter; we list all and filter client-side.
func (b *Backend) ListMetersByNamespace(ctx context.Context, namespace string) ([]catalog.ExistingMeter, error) {
	prefix := namespace + "."
	params := &stripe.BillingMeterListParams{}
	params.Filters.AddFilter("limit", "", "100")
	var out []catalog.ExistingMeter
	for m, err := range b.sc.V1BillingMeters.List(ctx, params) {
		if err != nil {
			return nil, fmt.Errorf("stripe.BillingMeters.List: %w", err)
		}
		if !strings.HasPrefix(m.EventName, prefix) {
			continue
		}
		out = append(out, projectMeter(m))
	}
	return out, nil
}

// CreateMeter creates a Stripe Billing Meter.
func (b *Backend) CreateMeter(ctx context.Context, m catalog.NewMeter) (catalog.ExistingMeter, error) {
	params := &stripe.BillingMeterCreateParams{
		DisplayName: stripe.String(m.DisplayName),
		EventName:   stripe.String(m.EventName),
		DefaultAggregation: &stripe.BillingMeterCreateDefaultAggregationParams{
			Formula: stripe.String(aggregationFormula(m.AggregateBy)),
		},
	}
	applyIdem(params, ctx, "meter.create", m.DisplayName, m.EventName, m.AggregateBy)
	created, err := b.sc.V1BillingMeters.Create(ctx, params)
	if err != nil {
		return catalog.ExistingMeter{}, fmt.Errorf("stripe.BillingMeters.Create(%s): %w", m.EventName, err)
	}
	return projectMeter(created), nil
}

// ArchiveMeter deactivates a Stripe Billing Meter.
func (b *Backend) ArchiveMeter(ctx context.Context, meterID string) error {
	params := &stripe.BillingMeterDeactivateParams{}
	applyIdem(params, ctx, "meter.archive", meterID)
	if _, err := b.sc.V1BillingMeters.Deactivate(ctx, meterID, params); err != nil {
		return fmt.Errorf("stripe.BillingMeters.Deactivate(%s): %w", meterID, err)
	}
	return nil
}

func projectMeter(m *stripe.BillingMeter) catalog.ExistingMeter {
	out := catalog.ExistingMeter{
		ID:          m.ID,
		EventName:   m.EventName,
		DisplayName: m.DisplayName,
		Active:      m.Status == "active",
	}
	if m.DefaultAggregation != nil {
		out.AggregateBy = string(m.DefaultAggregation.Formula)
	}
	return out
}

func aggregationFormula(s string) string {
	switch s {
	case "count", "sum", "last", "last_value":
		if s == "last_value" {
			return "last"
		}
		return s
	default:
		return "sum"
	}
}
