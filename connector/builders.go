package connector

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bds421/rho-stripe/catalog"
	"github.com/bds421/rho-stripe/checkout"
	"github.com/bds421/rho-stripe/checkout/portalconfig"
	"github.com/bds421/rho-stripe/climate"
	"github.com/bds421/rho-stripe/coupons"
	"github.com/bds421/rho-stripe/credits"
	"github.com/bds421/rho-stripe/customers"
	"github.com/bds421/rho-stripe/disputes"
	"github.com/bds421/rho-stripe/invoices"
	"github.com/bds421/rho-stripe/metering"
	"github.com/bds421/rho-stripe/paymentmethods"
	"github.com/bds421/rho-stripe/payments"
	"github.com/bds421/rho-stripe/plans"
	"github.com/bds421/rho-stripe/quotes"
	"github.com/bds421/rho-stripe/stripeapi"
	"github.com/bds421/rho-stripe/subscriptions"
	stripe "github.com/stripe/stripe-go/v82"
)

// buildStripeClient resolves the Stripe-backed clients OR honors the
// per-test BackendOverride hatches. Returns (sc, catalogBackend,
// checkoutBackend) where sc may be nil iff overrides supply both.
func buildStripeClient(cfg Config) (*stripe.Client, catalog.Backend, checkout.Backend) {
	if cfg.BackendOverride != nil && cfg.CheckoutBackendOverride != nil {
		return nil, cfg.BackendOverride, cfg.CheckoutBackendOverride
	}
	sc := stripeapi.NewClient(stripeapi.Config{
		SecretKey: cfg.SecretKey,
		Timeout:   cfg.HTTPTimeout,
	})
	return sc, stripeapi.NewBackend(sc), stripeapi.NewCheckoutBackend(sc)
}

// warmCatalogWithTimeout calls cache.Warm under a bounded context so
// a slow or unreachable Stripe doesn't block connector.New forever
// (k8s pods stuck in starting indefinitely). Caller picks the
// timeout; we wrap the deadline-exceeded error with operator-friendly
// context.
func warmCatalogWithTimeout(ctx context.Context, cache *catalog.Cache, spec *catalog.Spec, timeout time.Duration) error {
	warmCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := cache.Warm(warmCtx, spec); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("connector: warm catalog cache exceeded %s timeout (Stripe slow or unreachable): %w", timeout, err)
		}
		return fmt.Errorf("connector: warm catalog cache: %w", err)
	}
	return nil
}

// buildInvoiceOps returns nil when there's no backend to construct
// one against. cfg.InvoiceBackendOverride wins over the sc-backed
// default (test seam).
func buildInvoiceOps(cfg Config, sc *stripe.Client) *invoices.Operations {
	var invoiceBackend invoices.Backend
	switch {
	case cfg.InvoiceBackendOverride != nil:
		invoiceBackend = cfg.InvoiceBackendOverride
	case sc != nil:
		invoiceBackend = stripeapi.NewInvoiceBackend(sc)
	}
	if invoiceBackend == nil {
		return nil
	}
	opts := []invoices.Option{invoices.WithNamespace(cfg.AppNamespace)}
	if cfg.InvoiceNumberRepo != nil {
		opts = append(opts, invoices.WithNumberRepo(cfg.InvoiceNumberRepo))
	}
	if cfg.InvoiceOrphanHook != nil {
		opts = append(opts, invoices.WithOrphanHook(cfg.InvoiceOrphanHook))
	}
	return invoices.New(invoiceBackend, opts...)
}

// customerAggregate bundles every Stripe-client-backed operations
// struct that only exists when sc != nil — keeps the New body
// compact.
type customerAggregate struct {
	Customers      *customers.Operations
	PaymentMethods *paymentmethods.Operations
	Disputes       *disputes.Operations
	Quotes         *quotes.Operations
	PortalConfig   *portalconfig.Operations
	Climate        *climate.Operations
	Coupons        *coupons.Operations
	Refunds        *payments.Operations
}

// buildCustomerAggregate constructs every per-Stripe-client subsystem
// in one place. Returns an aggregate with all fields nil when sc is
// nil (test-override path).
//
// subOps may be nil; when set, customers.Forget routes subscription
// cancellation through the lib's Operations (preserving namespace
// stamp + idempotency key) instead of the raw Stripe client.
func buildCustomerAggregate(cfg Config, sc *stripe.Client, subOps *subscriptions.Operations) customerAggregate {
	if sc == nil {
		return customerAggregate{}
	}
	var canceler customers.SubscriptionCanceler
	if subOps != nil {
		canceler = subOps
	}
	return customerAggregate{
		Customers: customers.New(customers.Config{
			StripeClient:         sc,
			CustomerRepo:         cfg.Customers,
			SubscriptionRepo:     cfg.Subscriptions,
			CreditRepo:           cfg.Credits,
			AppDataExporter:      cfg.AppDataExporter,
			AppDataForgetter:     cfg.AppDataForgetter,
			SubscriptionCanceler: canceler,
		}),
		PaymentMethods: paymentmethods.New(paymentmethods.Config{
			StripeClient: sc,
			CustomerRepo: cfg.Customers,
		}),
		Disputes:     disputes.New(sc),
		Quotes:       quotes.New(quotes.Config{StripeClient: sc, Namespace: cfg.AppNamespace}),
		PortalConfig: portalconfig.New(sc),
		Climate:      climate.New(sc),
		Coupons:      coupons.New(coupons.Config{Backend: stripeapi.NewCouponBackend(sc), Spec: cfg.Catalog}),
		Refunds: payments.New(
			stripeapi.NewRefundBackend(sc),
			payments.WithNamespace(cfg.AppNamespace),
		),
	}
}

// buildOptionalSubsystems wires the per-app-config-driven ops that
// don't depend on sc directly: credits / metering / plans.
func buildOptionalSubsystems(cfg Config) (creditOps *credits.Operations, meteringOps *metering.Operations, planOps *plans.Operations) {
	if cfg.Credits != nil {
		creditOps = credits.New(cfg.Credits)
	}
	if cfg.Usage != nil {
		meteringOps = metering.New(cfg.Usage)
	}
	if cfg.Subscriptions != nil {
		planOps = plans.New(plans.Config{Spec: cfg.Catalog, SubRepo: cfg.Subscriptions, CreditRepo: cfg.Credits})
	}
	return
}

// buildSubscriptionOps may return (nil, nil) when Config.Subscriptions
// is unset, or an error when the backend can't be constructed.
func buildSubscriptionOps(cfg Config, sc *stripe.Client, cache *catalog.Cache) (*subscriptions.Operations, error) {
	if cfg.Subscriptions == nil {
		return nil, nil
	}
	subBackend := cfg.SubscriptionBackendOverride
	if subBackend == nil {
		if sc == nil {
			return nil, errors.New("connector: Subscriptions set but no Stripe client to back it (provide SecretKey or SubscriptionBackendOverride)")
		}
		subBackend = stripeapi.NewSubscriptionBackend(sc)
	}
	return subscriptions.New(subscriptions.Config{Backend: subBackend, Repo: cfg.Subscriptions, Spec: cfg.Catalog, Cache: cache}), nil
}
