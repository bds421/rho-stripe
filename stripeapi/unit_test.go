package stripeapi_test

// Unit tests for the stripeapi backends. These stand up a local
// httptest.Server that pretends to be Stripe, point a stripe-go client
// at it, and assert that the lib's Backend methods send the right
// requests + project responses correctly. Live-mode integration tests
// (live_stripe_test.go at repo root) cover the real-API path; these
// tests cover request-shape and projection details without burning
// quota on every CI run.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/bds421/rho-stripe/catalog"
	"github.com/bds421/rho-stripe/checkout"
	"github.com/bds421/rho-stripe/stripeapi"
	stripe "github.com/stripe/stripe-go/v82"
)

// fakeStripe wires an httptest.Server into a stripe-go *stripe.Client.
// Tests register per-path handlers via fakeStripe.Handle.
type fakeStripe struct {
	srv      *httptest.Server
	mu       sync.Mutex
	handlers map[string]http.HandlerFunc
	calls    []recordedCall
}

type recordedCall struct {
	Method string
	Path   string
	Form   url.Values
}

func newFakeStripe(t *testing.T) (*fakeStripe, *stripe.Client) {
	t.Helper()
	fs := &fakeStripe{handlers: map[string]http.HandlerFunc{}}
	fs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		fs.mu.Lock()
		fs.calls = append(fs.calls, recordedCall{Method: r.Method, Path: r.URL.Path, Form: form})
		// Match handlers by exact path first, then prefix.
		var matched http.HandlerFunc
		if h, ok := fs.handlers[r.Method+" "+r.URL.Path]; ok {
			matched = h
		} else {
			for key, h := range fs.handlers {
				if strings.HasPrefix(key, r.Method+" ") &&
					strings.HasSuffix(key, "*") &&
					strings.HasPrefix(r.URL.Path, strings.TrimSuffix(strings.TrimPrefix(key, r.Method+" "), "*")) {
					matched = h
					break
				}
			}
		}
		fs.mu.Unlock()
		if matched == nil {
			t.Errorf("fakeStripe: no handler for %s %s", r.Method, r.URL.Path)
			http.Error(w, `{"error":{"message":"unhandled"}}`, http.StatusNotFound)
			return
		}
		// Re-populate body for the handler to read if it wants.
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		matched(w, r)
	}))
	t.Cleanup(fs.srv.Close)

	backends := &stripe.Backends{
		API: stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
			URL: stripe.String(fs.srv.URL),
		}),
	}
	sc := stripe.NewClient("sk_test_fake_unit", stripe.WithBackends(backends))
	return fs, sc
}

func (fs *fakeStripe) Handle(methodPath string, h http.HandlerFunc) {
	fs.mu.Lock()
	fs.handlers[methodPath] = h
	fs.mu.Unlock()
}

func (fs *fakeStripe) CallsTo(method, path string) []recordedCall {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var out []recordedCall
	for _, c := range fs.calls {
		if c.Method == method && c.Path == path {
			out = append(out, c)
		}
	}
	return out
}

func writeJSON(t *testing.T, w http.ResponseWriter, payload any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Errorf("encode: %v", err)
	}
}

// --- Backend.ListProductsByNamespace ---

func TestUnit_BackendListProductsByNamespace(t *testing.T) {
	fs, sc := newFakeStripe(t)
	be := stripeapi.NewBackend(sc)

	fs.Handle("GET /v1/products", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{
			"object":   "list",
			"has_more": false,
			"data": []any{
				map[string]any{
					"id":     "prod_demo_x",
					"object": "product",
					"name":   "Demo X",
					"active": true,
					"tax_code": map[string]any{
						"id":     "txcd_10103000",
						"object": "tax_code",
					},
					"metadata": map[string]string{"app_namespace": "demo"},
				},
				map[string]any{
					"id":       "prod_other",
					"object":   "product",
					"name":     "Other App",
					"active":   true,
					"metadata": map[string]string{"app_namespace": "other"},
				},
			},
		})
	})
	// Prices list (per product) — Backend issues one call per product
	// to enumerate prices. Return empty for both.
	fs.Handle("GET /v1/prices", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"object": "list", "data": []any{}})
	})

	got, err := be.ListProductsByNamespace(t.Context(), "demo")
	if err != nil {
		t.Fatalf("ListProductsByNamespace: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 product (namespace filter), got %d (%+v)", len(got), got)
	}
	if got[0].ID != "prod_demo_x" {
		t.Errorf("ID = %q", got[0].ID)
	}
}

// --- Backend.CreateProduct stamps metadata ---

func TestUnit_BackendCreateProductSendsMetadataAndTax(t *testing.T) {
	fs, sc := newFakeStripe(t)
	be := stripeapi.NewBackend(sc)

	fs.Handle("POST /v1/products", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{
			"id":       "prod_demo_pro",
			"object":   "product",
			"name":     "Pro",
			"active":   true,
			"tax_code": map[string]any{"id": "txcd_10103000", "object": "tax_code"},
		})
	})

	_, err := be.CreateProduct(t.Context(), catalog.NewProduct{
		ID:       "prod_demo_pro",
		Name:     "Pro",
		TaxCode:  "txcd_10103000",
		Metadata: map[string]string{"app_namespace": "demo"},
	})
	if err != nil {
		t.Fatalf("CreateProduct: %v", err)
	}
	calls := fs.CallsTo("POST", "/v1/products")
	if len(calls) != 1 {
		t.Fatalf("expected 1 POST /v1/products, got %d", len(calls))
	}
	form := calls[0].Form
	if form.Get("name") != "Pro" {
		t.Errorf("name field = %q", form.Get("name"))
	}
	if form.Get("metadata[app_namespace]") != "demo" {
		t.Errorf("metadata stamp missing in form: %v", form)
	}
	if form.Get("tax_code") != "txcd_10103000" {
		t.Errorf("tax_code = %q", form.Get("tax_code"))
	}
}

// --- CheckoutBackend.CreateCheckoutSession plumbing ---

func TestUnit_CheckoutBackendEmbeddedSessionRequestsReturnURL(t *testing.T) {
	fs, sc := newFakeStripe(t)
	be := stripeapi.NewCheckoutBackend(sc)

	fs.Handle("POST /v1/checkout/sessions", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{
			"id":            "cs_test_xyz",
			"object":        "checkout.session",
			"client_secret": "cs_secret_xyz",
		})
	})

	_, err := be.CreateCheckoutSession(t.Context(), checkout.SessionCreate{
		Mode:             checkout.ModeSubscription,
		StripeCustomerID: "cus_x",
		UIMode:           checkout.UIModeEmbedded,
		ReturnURL:        "https://example.com/return",
		LineItems:        []checkout.SessionLineItem{{StripePriceID: "price_a", Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	calls := fs.CallsTo("POST", "/v1/checkout/sessions")
	if len(calls) != 1 {
		t.Fatalf("expected 1 POST, got %d", len(calls))
	}
	form := calls[0].Form
	if form.Get("ui_mode") != "embedded" {
		t.Errorf("ui_mode = %q", form.Get("ui_mode"))
	}
	if form.Get("return_url") != "https://example.com/return" {
		t.Errorf("return_url = %q", form.Get("return_url"))
	}
	if form.Get("success_url") != "" {
		t.Errorf("success_url should be empty in embedded mode, got %q", form.Get("success_url"))
	}
}

func TestUnit_CheckoutBackendHostedSessionRequestsSuccessAndCancel(t *testing.T) {
	fs, sc := newFakeStripe(t)
	be := stripeapi.NewCheckoutBackend(sc)

	fs.Handle("POST /v1/checkout/sessions", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{
			"id":     "cs_test_h",
			"object": "checkout.session",
			"url":    "https://checkout.stripe.com/c/cs_test_h",
		})
	})

	sess, err := be.CreateCheckoutSession(t.Context(), checkout.SessionCreate{
		Mode:             checkout.ModePayment,
		StripeCustomerID: "cus_x",
		SuccessURL:       "https://app.example.com/s",
		CancelURL:        "https://app.example.com/c",
		LineItems:        []checkout.SessionLineItem{{StripePriceID: "price_b", Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	if sess.URL == "" {
		t.Error("hosted session should return URL")
	}
	form := fs.CallsTo("POST", "/v1/checkout/sessions")[0].Form
	if form.Get("success_url") != "https://app.example.com/s" {
		t.Errorf("success_url = %q", form.Get("success_url"))
	}
	if form.Get("ui_mode") != "" {
		t.Errorf("ui_mode should be empty for hosted, got %q", form.Get("ui_mode"))
	}
}

// --- CheckoutBackend honors SessionDefaults overrides ---

func TestUnit_CheckoutBackendHonorsB2CDefaults(t *testing.T) {
	fs, sc := newFakeStripe(t)
	be := stripeapi.NewCheckoutBackend(sc)

	fs.Handle("POST /v1/checkout/sessions", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"id": "cs_test_b2c", "object": "checkout.session"})
	})

	noTax := false
	noPromos := false
	_, err := be.CreateCheckoutSession(t.Context(), checkout.SessionCreate{
		Mode:             checkout.ModePayment,
		StripeCustomerID: "cus_b2c",
		SuccessURL:       "https://x", CancelURL: "https://x",
		LineItems: []checkout.SessionLineItem{{StripePriceID: "p", Quantity: 1}},
		Defaults: checkout.SessionDefaults{
			AutomaticTax:             &noTax,
			TaxIDCollection:          &noTax,
			BillingAddressCollection: "auto",
			AllowPromotionCodes:      &noPromos,
		},
	})
	if err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	form := fs.CallsTo("POST", "/v1/checkout/sessions")[0].Form
	if form.Get("automatic_tax[enabled]") != "false" {
		t.Errorf("automatic_tax[enabled] = %q, want false", form.Get("automatic_tax[enabled]"))
	}
	if form.Get("tax_id_collection[enabled]") != "false" {
		t.Errorf("tax_id_collection[enabled] = %q, want false", form.Get("tax_id_collection[enabled]"))
	}
	if form.Get("billing_address_collection") != "auto" {
		t.Errorf("billing_address_collection = %q", form.Get("billing_address_collection"))
	}
	if form.Get("allow_promotion_codes") != "false" {
		t.Errorf("allow_promotion_codes = %q", form.Get("allow_promotion_codes"))
	}
}

// --- Trial support ---

func TestUnit_CheckoutBackendSubscriptionWithTrial(t *testing.T) {
	fs, sc := newFakeStripe(t)
	be := stripeapi.NewCheckoutBackend(sc)

	fs.Handle("POST /v1/checkout/sessions", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"id": "cs_trial", "object": "checkout.session", "url": "https://x"})
	})

	_, err := be.CreateCheckoutSession(t.Context(), checkout.SessionCreate{
		Mode:             checkout.ModeSubscription,
		StripeCustomerID: "cus_x",
		SuccessURL:       "https://x", CancelURL: "https://x",
		LineItems: []checkout.SessionLineItem{{StripePriceID: "price_a", Quantity: 1}},
		TrialDays: 14,
	})
	if err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	form := fs.CallsTo("POST", "/v1/checkout/sessions")[0].Form
	if form.Get("subscription_data[trial_period_days]") != "14" {
		t.Errorf("trial_period_days form field = %q, want 14", form.Get("subscription_data[trial_period_days]"))
	}
	// payment_method_collection should NOT be set (default keeps card capture upfront)
	if form.Get("payment_method_collection") != "" {
		t.Errorf("default RequirePaymentMethodForTrial should leave field unset; got %q", form.Get("payment_method_collection"))
	}
}

func TestUnit_CheckoutBackendNoCardTrialUsesIfRequired(t *testing.T) {
	fs, sc := newFakeStripe(t)
	be := stripeapi.NewCheckoutBackend(sc)

	fs.Handle("POST /v1/checkout/sessions", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"id": "cs_ncc", "object": "checkout.session"})
	})

	noCardUpfront := false
	_, err := be.CreateCheckoutSession(t.Context(), checkout.SessionCreate{
		Mode:             checkout.ModeSubscription,
		StripeCustomerID: "cus_x",
		SuccessURL:       "https://x", CancelURL: "https://x",
		LineItems: []checkout.SessionLineItem{{StripePriceID: "price_a", Quantity: 1}},
		TrialDays: 14,
		Defaults: checkout.SessionDefaults{
			RequirePaymentMethodForTrial: &noCardUpfront,
		},
	})
	if err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	form := fs.CallsTo("POST", "/v1/checkout/sessions")[0].Form
	if form.Get("payment_method_collection") != "if_required" {
		t.Errorf("RequirePaymentMethodForTrial=false → want payment_method_collection=if_required, got %q",
			form.Get("payment_method_collection"))
	}
}
