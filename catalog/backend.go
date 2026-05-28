package catalog

import "context"

// Backend is the Stripe-side surface the sync algorithm depends on. The
// default implementation backed by stripe-go lives in the stripeapi
// package; tests inject fakes. Restricting the surface to these methods
// keeps the sync algorithm portable and trivially mockable.
type Backend interface {
	// ListProductsByNamespace returns every Stripe Product whose id is
	// prefixed by "prod_<namespace>_", along with all of its Prices
	// (active and archived).
	ListProductsByNamespace(ctx context.Context, namespace string) ([]ExistingProduct, error)

	CreateProduct(ctx context.Context, p NewProduct) (ExistingProduct, error)
	UpdateProduct(ctx context.Context, productID string, u ProductUpdate) error
	UpdateProductActive(ctx context.Context, productID string, active bool) error

	CreatePrice(ctx context.Context, p NewPrice) (ExistingPrice, error)
	UpdatePriceActive(ctx context.Context, priceID string, active bool) error

	// Meter management (phase 7).
	ListMetersByNamespace(ctx context.Context, namespace string) ([]ExistingMeter, error)
	CreateMeter(ctx context.Context, m NewMeter) (ExistingMeter, error)
	ArchiveMeter(ctx context.Context, meterID string) error
}

// ExistingMeter projects a Stripe billing meter.
type ExistingMeter struct {
	ID          string
	EventName   string // namespaced ("<ns>.<event>")
	DisplayName string
	AggregateBy string
	Active      bool
}

// NewMeter describes a meter to create.
type NewMeter struct {
	EventName   string // namespaced
	DisplayName string
	AggregateBy string
}

// ProductUpdate describes a partial update to an existing Product. Only
// non-zero fields are applied; metadata fully replaces (per Stripe's
// metadata semantics, set a key to empty string to delete it).
type ProductUpdate struct {
	Name        string
	Description string
	Metadata    map[string]string
}

// ExistingProduct is the projection of a Stripe Product the sync
// algorithm reads. Only the fields sync actually examines are present.
type ExistingProduct struct {
	ID          string
	Name        string
	TaxCode     string
	Active      bool
	Description string
	Metadata    map[string]string
	Prices      []ExistingPrice
}

// ExistingPrice is the projection of a Stripe Price the sync algorithm
// reads. LookupKey is the source-of-truth identifier the sync uses to
// match Stripe state to spec entries.
type ExistingPrice struct {
	ID            string
	LookupKey     string
	Active        bool
	Amount        int64
	Currency      string
	Type          PriceType
	Interval      Interval
	IntervalCount int
	Metadata      map[string]string
}

// NewProduct describes a Product to create in Stripe. ID is the
// pre-computed namespaced id (e.g. "prod_demo_pro_plan"); Stripe
// accepts custom Product ids.
type NewProduct struct {
	ID          string
	Name        string
	TaxCode     string
	Description string
	Metadata    map[string]string
}

// NewPrice describes a Price to create in Stripe.
type NewPrice struct {
	ProductID     string
	LookupKey     string
	Amount        int64
	Currency      string
	Type          PriceType
	Interval      Interval
	IntervalCount int
	Metadata      map[string]string

	// TransferLookupKey instructs Stripe to atomically move the
	// LookupKey from any existing Price with the same key to this new
	// one. Used by REPLACE plan items when price values change (Stripe
	// Prices are immutable so the only way to "change" is to create
	// new + transfer key + archive old).
	TransferLookupKey bool

	// TaxBehavior + TaxRateOverrides are forwarded from Spec.Price
	// to Stripe at create time. See Price docs for semantics.
	TaxBehavior      string
	TaxRateOverrides []string
}
