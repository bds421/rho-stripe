package connectortest_test

// This is the "namespace stamping convention" regression test.
//
// Every Stripe object the connector creates on behalf of the app MUST
// carry metadata.app_namespace = <connector AppNamespace>. The webhook
// router relies on this stamp to decide which app should handle each
// inbound event when multiple apps share a Stripe account.
//
// This test exercises every public mutating operation that creates a
// Stripe object and asserts the stamp landed on the backend call.
// When a new mutating operation is added to the lib, add a case here —
// the failure mode (forgot to stamp) becomes a red test immediately.

import (
	"context"
	"testing"
	"time"

	"github.com/bds421/rho-stripe/catalog"
	"github.com/bds421/rho-stripe/checkout"
	"github.com/bds421/rho-stripe/coupons"
	"github.com/bds421/rho-stripe/invoices"
	"github.com/bds421/rho-stripe/subscriptions"
)

const testNS = "stamp_demo"

func stampSpec() *catalog.Spec {
	return catalog.MustSpec(catalog.Spec{
		Namespace: testNS,
		Products: map[string]catalog.Product{
			"pro": {
				Name:        "Pro",
				TaxCategory: catalog.TaxCategorySaaS,
				Prices: map[string]catalog.Price{
					"monthly_eur": {Amount: 4900, Currency: "eur", Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
				},
			},
		},
		Coupons: map[string]catalog.Coupon{
			"SAVE20": {PercentOff: 20, Duration: catalog.CouponDurationOnce},
		},
	})
}

// --- recording fake backends ---

type recCheckoutBackend struct {
	customerMeta map[string]string
	sessionMeta  map[string]string
}

func (r *recCheckoutBackend) CreateCustomer(_ context.Context, p checkout.CustomerCreate) (checkout.StripeCustomerID, error) {
	r.customerMeta = p.Metadata
	return "cus_stamp", nil
}
func (r *recCheckoutBackend) CreateCheckoutSession(_ context.Context, p checkout.SessionCreate) (checkout.Session, error) {
	r.sessionMeta = p.Metadata
	return checkout.Session{ID: "cs_stamp", URL: "https://example/checkout"}, nil
}
func (r *recCheckoutBackend) CreatePortalSession(_ context.Context, _ checkout.PortalSessionCreate) (checkout.PortalSession, error) {
	return checkout.PortalSession{ID: "bps_stamp", URL: "https://example/portal"}, nil
}

type recSubsBackend struct {
	invoicedMeta map[string]string
	scheduleMeta map[string]string
}

func (r *recSubsBackend) CancelAtPeriodEnd(_ context.Context, _ string, _ bool) error { return nil }
func (r *recSubsBackend) CancelNow(_ context.Context, _ string, _ subscriptions.CancelNowOptions) error {
	return nil
}
func (r *recSubsBackend) UpdateItems(_ context.Context, _ string, _ []subscriptions.ItemChange, _ subscriptions.UpdateOptions) error {
	return nil
}
func (r *recSubsBackend) ApplyCoupon(_ context.Context, _, _ string) error { return nil }
func (r *recSubsBackend) RemoveCoupon(_ context.Context, _ string) error   { return nil }
func (r *recSubsBackend) ListSince(_ context.Context, _ time.Time) ([]subscriptions.ReconciledSubscription, error) {
	return nil, nil
}
func (r *recSubsBackend) CreateInvoiced(_ context.Context, in subscriptions.InvoicedSubscriptionInput) (*subscriptions.Subscription, error) {
	r.invoicedMeta = in.Metadata
	return &subscriptions.Subscription{StripeID: "sub_invoiced_stamp"}, nil
}
func (r *recSubsBackend) CreateSubscriptionSchedule(_ context.Context, in subscriptions.ScheduleBackendInput) (*subscriptions.Schedule, error) {
	r.scheduleMeta = in.Metadata
	return &subscriptions.Schedule{StripeID: "sub_sched_stamp"}, nil
}
func (r *recSubsBackend) GetSubscriptionItems(_ context.Context, _ string) ([]subscriptions.SubscriptionItem, error) {
	return nil, nil
}
func (r *recSubsBackend) PreviewItemChange(_ context.Context, _ string, _ []subscriptions.ItemChange) (subscriptions.MigratePreview, error) {
	return subscriptions.MigratePreview{}, nil
}
func (r *recSubsBackend) Pause(_ context.Context, _ string, _ subscriptions.PauseOptions) error {
	return nil
}
func (r *recSubsBackend) Resume(_ context.Context, _ string) error { return nil }

type recCouponBackend struct {
	createMeta map[string]string
}

func (r *recCouponBackend) CreatePromoCode(_ context.Context, p coupons.CreateParams) (coupons.PromoCode, error) {
	r.createMeta = p.Metadata
	return coupons.PromoCode{ID: "promo_stamp", Code: p.Code}, nil
}
func (r *recCouponBackend) ListPromoCodes(_ context.Context, _ coupons.ListParams) ([]coupons.PromoCode, error) {
	return nil, nil
}
func (r *recCouponBackend) DeactivatePromoCode(_ context.Context, _ string) error { return nil }

type recInvoiceBackend struct {
	draftMeta map[string]string
}

func (r *recInvoiceBackend) ListInvoices(_ context.Context, _ invoices.Period) ([]invoices.AuditEntry, error) {
	return nil, nil
}
func (r *recInvoiceBackend) CreateDraft(_ context.Context, in invoices.CreateInput) (invoices.Invoice, error) {
	r.draftMeta = in.Metadata
	return invoices.Invoice{StripeID: "in_stamp", Status: "draft", Customer: in.Customer}, nil
}
func (r *recInvoiceBackend) Finalize(_ context.Context, id string) (invoices.Invoice, error) {
	return invoices.Invoice{StripeID: id, Status: "open"}, nil
}
func (r *recInvoiceBackend) FinalizeAndSend(_ context.Context, id string) (invoices.Invoice, error) {
	return invoices.Invoice{StripeID: id, Status: "open"}, nil
}
func (r *recInvoiceBackend) Void(_ context.Context, _ string) error              { return nil }
func (r *recInvoiceBackend) MarkUncollectible(_ context.Context, _ string) error { return nil }
func (r *recInvoiceBackend) IssueCreditNote(_ context.Context, _ invoices.CreditNoteInput) error {
	return nil
}
func (r *recInvoiceBackend) ListOverdue(_ context.Context) ([]invoices.Invoice, error) {
	return nil, nil
}
func (r *recInvoiceBackend) ListByCustomer(_ context.Context, _ invoices.StripeCustomerID, _ string, _ int) ([]invoices.Invoice, error) {
	return nil, nil
}
func (r *recInvoiceBackend) ListByCustomerPage(_ context.Context, _ invoices.InvoicePageRequest) (invoices.InvoicePage, error) {
	return invoices.InvoicePage{}, nil
}

// --- the actual stamp assertion ---

func assertStamp(t *testing.T, where string, meta map[string]string) {
	t.Helper()
	v, ok := meta["app_namespace"]
	if !ok {
		t.Errorf("%s: metadata.app_namespace is missing — every connector-created Stripe object must carry the namespace stamp", where)
		return
	}
	if v != testNS {
		t.Errorf("%s: metadata.app_namespace = %q, want %q", where, v, testNS)
	}
}

// resolver passthrough for the checkout test.
type stampResolver struct{}

func (stampResolver) Lookup(k string) (string, bool) {
	if k == "stamp_demo.pro.monthly_eur" {
		return "price_stamp", true
	}
	return "", false
}

// warmCache populates the cache without hitting Stripe.
func warmCache(t *testing.T, spec *catalog.Spec) *catalog.Cache {
	t.Helper()
	cache := catalog.NewCache(stampLister{})
	if err := cache.Warm(t.Context(), spec); err != nil {
		t.Fatalf("warm: %v", err)
	}
	return cache
}

type stampLister struct{}

func (stampLister) ListByLookupKeys(_ context.Context, keys []string) ([]catalog.ResolvedPrice, error) {
	out := make([]catalog.ResolvedPrice, 0, len(keys))
	for _, k := range keys {
		out = append(out, catalog.ResolvedPrice{LookupKey: k, PriceID: "price_" + k, Active: true})
	}
	return out, nil
}

func TestNamespaceStamping_Checkout(t *testing.T) {
	be := &recCheckoutBackend{}
	c := checkout.New(checkout.Config{
		Namespace: testNS,
		Spec:      stampSpec(),
		Resolver:  stampResolver{},
		Customers: checkout.NewMemoryCustomerRepo(),
		Backend:   be,
	})
	_, err := c.CreateSession(t.Context(), checkout.Input{
		SubjectID:    "org_acme",
		LineItems:  []checkout.LineItem{{PriceKey: "pro.monthly_eur"}},
		SuccessURL: "https://example/s",
		CancelURL:  "https://example/c",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	assertStamp(t, "Checkout.CreateCustomer", be.customerMeta)
	assertStamp(t, "Checkout.CreateCheckoutSession", be.sessionMeta)
}

func TestNamespaceStamping_SubscriptionsCreateInvoiced(t *testing.T) {
	be := &recSubsBackend{}
	spec := stampSpec()
	cache := warmCache(t, spec)
	ops := subscriptions.New(subscriptions.Config{Backend: be, Repo: subscriptions.NewMemoryRepo(), Spec: spec, Cache: cache})
	if _, err := ops.CreateInvoiced(t.Context(), subscriptions.InvoicedInput{
		StripeCustomerID: "cus_x",
		PriceKey:         "pro.monthly_eur",
		DueIn:            30 * 24 * time.Hour,
	}); err != nil {
		t.Fatalf("CreateInvoiced: %v", err)
	}
	assertStamp(t, "Subscriptions.CreateInvoiced", be.invoicedMeta)
}

func TestNamespaceStamping_SubscriptionsCreateSchedule(t *testing.T) {
	be := &recSubsBackend{}
	spec := stampSpec()
	cache := warmCache(t, spec)
	ops := subscriptions.New(subscriptions.Config{Backend: be, Repo: subscriptions.NewMemoryRepo(), Spec: spec, Cache: cache})
	if _, err := ops.CreateSchedule(t.Context(), subscriptions.ScheduleInput{
		StripeCustomerID: "cus_x",
		Phases:           []subscriptions.SchedulePhase{{PriceKey: "pro.monthly_eur", Iterations: 3}},
	}); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	assertStamp(t, "Subscriptions.CreateSchedule", be.scheduleMeta)
}

func TestNamespaceStamping_CouponsCreatePromoCode(t *testing.T) {
	be := &recCouponBackend{}
	ops := coupons.New(coupons.Config{Backend: be, Spec: stampSpec()})
	if _, err := ops.CreatePromoCode(t.Context(), coupons.CreateInput{
		CouponKey: "SAVE20",
		Code:      "WELCOME",
	}); err != nil {
		t.Fatalf("CreatePromoCode: %v", err)
	}
	assertStamp(t, "Coupons.CreatePromoCode", be.createMeta)
}

func TestNamespaceStamping_InvoicesCreateDraft(t *testing.T) {
	be := &recInvoiceBackend{}
	ops := invoices.New(be, invoices.WithNamespace(testNS))
	if _, err := ops.CreateDraft(t.Context(), invoices.CreateInput{
		Customer:  "cus_x",
		LineItems: []invoices.CreateLineItem{{Amount: 1000, Currency: "eur"}},
	}); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	assertStamp(t, "Invoices.CreateDraft", be.draftMeta)
}
