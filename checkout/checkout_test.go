package checkout_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/bds421/rho-stripe/catalog"
	"github.com/bds421/rho-stripe/checkout"
)

// fakeResolver implements PriceResolver from a map.
type fakeResolver struct{ m map[string]string }

func (f fakeResolver) Lookup(k string) (string, bool) { id, ok := f.m[k]; return id, ok }

// fakeBackend records calls.
type fakeBackend struct {
	mu                sync.Mutex
	customersCreated  []checkout.CustomerCreate
	sessionsCreated   []checkout.SessionCreate
	portalCreated     []checkout.PortalSessionCreate
	createCustomerErr error
	createSessionErr  error
	nextCustomerID    string
	nextSessionURL    string
}

func (b *fakeBackend) CreateCustomer(_ context.Context, p checkout.CustomerCreate) (checkout.StripeCustomerID, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.createCustomerErr != nil {
		return "", b.createCustomerErr
	}
	b.customersCreated = append(b.customersCreated, p)
	id := b.nextCustomerID
	if id == "" {
		id = "cus_fake"
	}
	return checkout.StripeCustomerID(id), nil
}

func (b *fakeBackend) CreateCheckoutSession(_ context.Context, p checkout.SessionCreate) (checkout.Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.createSessionErr != nil {
		return checkout.Session{}, b.createSessionErr
	}
	b.sessionsCreated = append(b.sessionsCreated, p)
	if p.UIMode == checkout.UIModeEmbedded {
		return checkout.Session{ID: "cs_emb_fake", ClientSecret: "cs_secret_xyz"}, nil
	}
	url := b.nextSessionURL
	if url == "" {
		url = "https://checkout.stripe.com/c/cs_fake"
	}
	return checkout.Session{ID: "cs_fake", URL: url}, nil
}

func (b *fakeBackend) CreatePortalSession(_ context.Context, p checkout.PortalSessionCreate) (checkout.PortalSession, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.portalCreated = append(b.portalCreated, p)
	return checkout.PortalSession{ID: "bps_fake", URL: "https://billing.stripe.com/p/bps_fake"}, nil
}

func newCheckout(t *testing.T, resolver map[string]string, be *fakeBackend) (*checkout.Checkout, checkout.CustomerRepo) {
	t.Helper()
	spec := catalog.MustSpec(catalog.Spec{
		Namespace: "demo",
		Products: map[string]catalog.Product{
			"pro_plan": {
				Name:        "Pro Plan",
				TaxCategory: catalog.TaxCategorySaaS,
				Prices: map[string]catalog.Price{
					"monthly_eur": {Amount: 4900, Currency: "eur", Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
					"yearly_eur":  {Amount: 49000, Currency: "eur", Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalYear},
				},
			},
			"credit_pack": {
				Name:        "Credit Pack",
				TaxCategory: catalog.TaxCategorySaaS,
				Prices: map[string]catalog.Price{
					"default": {Amount: 1000, Currency: "eur", Type: catalog.PriceTypeOneTime},
				},
			},
		},
	})
	repo := checkout.NewMemoryCustomerRepo()
	c := checkout.New(checkout.Config{
		Namespace: "demo",
		Spec:      spec,
		Resolver:  fakeResolver{m: resolver},
		Customers: repo,
		Backend:   be,
	})
	return c, repo
}

func validInput() checkout.Input {
	return checkout.Input{
		SubjectID:    "org_acme",
		Actor:      "user_alice",
		LineItems:  []checkout.LineItem{{PriceKey: "pro_plan.monthly_eur"}},
		SuccessURL: "https://app.example.com/success",
		CancelURL:  "https://app.example.com/cancel",
	}
}

func TestCreateSession_HappyPath(t *testing.T) {
	be := &fakeBackend{}
	c, _ := newCheckout(t, map[string]string{"demo.pro_plan.monthly_eur": "price_m1"}, be)

	sess, err := c.CreateSession(t.Context(), validInput())
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sess.URL == "" {
		t.Error("session URL is empty")
	}
	if len(be.sessionsCreated) != 1 {
		t.Fatalf("expected 1 session, got %d", len(be.sessionsCreated))
	}
	s := be.sessionsCreated[0]
	if s.Mode != checkout.ModeSubscription {
		t.Errorf("Mode = %q, want subscription", s.Mode)
	}
	if len(s.LineItems) != 1 || s.LineItems[0].StripePriceID != "price_m1" || s.LineItems[0].Quantity != 1 {
		t.Errorf("LineItems wrong: %+v", s.LineItems)
	}
	if s.ClientReference != "user_alice" {
		t.Errorf("ClientReference = %q", s.ClientReference)
	}
	if s.Metadata["app_namespace"] != "demo" {
		t.Errorf("metadata.app_namespace = %q", s.Metadata["app_namespace"])
	}
	if s.Metadata["subject_id"] != "org_acme" {
		t.Errorf("metadata.subject_id = %q", s.Metadata["subject_id"])
	}
	if s.Metadata["actor_id"] != "user_alice" {
		t.Errorf("metadata.actor_id = %q", s.Metadata["actor_id"])
	}
}

func TestCreateSession_OneTimeInfersPaymentMode(t *testing.T) {
	be := &fakeBackend{}
	c, _ := newCheckout(t, map[string]string{"demo.credit_pack.default": "price_cp"}, be)

	in := validInput()
	in.LineItems = []checkout.LineItem{{PriceKey: "credit_pack.default"}}
	if _, err := c.CreateSession(t.Context(), in); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if be.sessionsCreated[0].Mode != checkout.ModePayment {
		t.Errorf("Mode = %q, want payment", be.sessionsCreated[0].Mode)
	}
}

func TestCreateSession_MixedItemTypesRejected(t *testing.T) {
	be := &fakeBackend{}
	c, _ := newCheckout(t, map[string]string{
		"demo.pro_plan.monthly_eur": "price_m1",
		"demo.credit_pack.default":  "price_cp",
	}, be)

	in := validInput()
	in.LineItems = []checkout.LineItem{
		{PriceKey: "pro_plan.monthly_eur"},
		{PriceKey: "credit_pack.default"},
	}
	_, err := c.CreateSession(t.Context(), in)
	if !errors.Is(err, checkout.ErrMixedItemTypes) {
		t.Errorf("expected ErrMixedItemTypes, got %v", err)
	}
}

func TestCreateSession_UnknownPriceKey(t *testing.T) {
	be := &fakeBackend{}
	c, _ := newCheckout(t, map[string]string{}, be) // resolver empty

	in := validInput()
	in.LineItems = []checkout.LineItem{{PriceKey: "pro_plan.monthly_eur"}}
	_, err := c.CreateSession(t.Context(), in)
	if !errors.Is(err, checkout.ErrPriceKeyNotFound) {
		t.Errorf("expected ErrPriceKeyNotFound, got %v", err)
	}
}

func TestCreateSession_PriceNotInSpec(t *testing.T) {
	be := &fakeBackend{}
	c, _ := newCheckout(t, map[string]string{"demo.ghost.x": "price_x"}, be)

	in := validInput()
	in.LineItems = []checkout.LineItem{{PriceKey: "ghost.x"}}
	_, err := c.CreateSession(t.Context(), in)
	if !errors.Is(err, checkout.ErrUnknownPriceInSpec) {
		t.Errorf("expected ErrUnknownPriceInSpec, got %v", err)
	}
}

func TestCreateSession_MissingFields(t *testing.T) {
	be := &fakeBackend{}
	c, _ := newCheckout(t, map[string]string{"demo.pro_plan.monthly_eur": "price_m1"}, be)

	for name, mutate := range map[string]func(*checkout.Input){
		"subject":     func(i *checkout.Input) { i.SubjectID = "" },
		"line_items":  func(i *checkout.Input) { i.LineItems = nil },
		"success_url": func(i *checkout.Input) { i.SuccessURL = "" },
		"cancel_url":  func(i *checkout.Input) { i.CancelURL = "" },
	} {
		t.Run(name, func(t *testing.T) {
			in := validInput()
			mutate(&in)
			_, err := c.CreateSession(t.Context(), in)
			if err == nil {
				t.Fatal("expected error for missing field, got nil")
			}
		})
	}
}

func TestCreateSession_ReusesExistingCustomer(t *testing.T) {
	be := &fakeBackend{}
	c, repo := newCheckout(t, map[string]string{"demo.pro_plan.monthly_eur": "price_m1"}, be)

	// Pre-seed the repo as if a previous session had created the customer.
	_ = repo.Upsert(t.Context(), "org_acme", "cus_prefab")

	if _, err := c.CreateSession(t.Context(), validInput()); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if len(be.customersCreated) != 0 {
		t.Errorf("expected 0 new customers (reuse), got %d", len(be.customersCreated))
	}
	if be.sessionsCreated[0].StripeCustomerID != "cus_prefab" {
		t.Errorf("session used customer %q, want cus_prefab", be.sessionsCreated[0].StripeCustomerID)
	}
}

func TestCreateSession_CreatesAndPersistsNewCustomer(t *testing.T) {
	be := &fakeBackend{nextCustomerID: "cus_new"}
	c, repo := newCheckout(t, map[string]string{"demo.pro_plan.monthly_eur": "price_m1"}, be)

	if _, err := c.CreateSession(t.Context(), validInput()); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if len(be.customersCreated) != 1 {
		t.Fatalf("expected 1 customer created, got %d", len(be.customersCreated))
	}
	id, ok, _ := repo.Get(t.Context(), "org_acme")
	if !ok || id != "cus_new" {
		t.Errorf("expected repo to have cus_new for org_acme, got (%q, ok=%v)", id, ok)
	}
	// Customer metadata should carry namespace + subject_id.
	c0 := be.customersCreated[0]
	if c0.Metadata["app_namespace"] != "demo" || c0.Metadata["subject_id"] != "org_acme" {
		t.Errorf("customer metadata stamps wrong: %+v", c0.Metadata)
	}
}

func TestCreateSession_QuantityDefaultsToOne(t *testing.T) {
	be := &fakeBackend{}
	c, _ := newCheckout(t, map[string]string{"demo.pro_plan.monthly_eur": "price_m1"}, be)

	if _, err := c.CreateSession(t.Context(), validInput()); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if be.sessionsCreated[0].LineItems[0].Quantity != 1 {
		t.Errorf("Quantity defaulted to %d, want 1", be.sessionsCreated[0].LineItems[0].Quantity)
	}
}

func TestCreateSession_PromoCodePassedThrough(t *testing.T) {
	be := &fakeBackend{}
	c, _ := newCheckout(t, map[string]string{"demo.pro_plan.monthly_eur": "price_m1"}, be)

	in := validInput()
	in.PromoCode = "SAVE20"
	if _, err := c.CreateSession(t.Context(), in); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if be.sessionsCreated[0].PromoCode != "SAVE20" {
		t.Errorf("PromoCode = %q, want SAVE20", be.sessionsCreated[0].PromoCode)
	}
}

func TestCreateSession_BackendErrorPropagates(t *testing.T) {
	be := &fakeBackend{createSessionErr: errors.New("stripe boom")}
	c, _ := newCheckout(t, map[string]string{"demo.pro_plan.monthly_eur": "price_m1"}, be)

	_, err := c.CreateSession(t.Context(), validInput())
	if err == nil || !strings.Contains(err.Error(), "stripe boom") {
		t.Errorf("expected wrapped stripe boom, got %v", err)
	}
}

func TestCreateSession_MalformedPriceKey(t *testing.T) {
	be := &fakeBackend{}
	c, _ := newCheckout(t, map[string]string{}, be)

	in := validInput()
	in.LineItems = []checkout.LineItem{{PriceKey: "nodot"}}
	_, err := c.CreateSession(t.Context(), in)
	if !errors.Is(err, checkout.ErrPriceKeyNotFound) {
		t.Errorf("expected ErrPriceKeyNotFound for malformed key, got %v", err)
	}
}

func TestCreateSession_EmbeddedReturnsClientSecret(t *testing.T) {
	be := &fakeBackend{}
	c, _ := newCheckout(t, map[string]string{"demo.pro_plan.monthly_eur": "price_m1"}, be)

	in := validInput()
	in.SuccessURL = ""
	in.CancelURL = ""
	in.UIMode = checkout.UIModeEmbedded
	in.ReturnURL = "https://app.example.com/return?cs={CHECKOUT_SESSION_ID}"

	sess, err := c.CreateSession(t.Context(), in)
	if err != nil {
		t.Fatalf("CreateSession (embedded): %v", err)
	}
	if sess.URL != "" {
		t.Errorf("URL should be empty in embedded mode, got %q", sess.URL)
	}
	if sess.ClientSecret == "" {
		t.Error("expected ClientSecret for embedded session")
	}
	if len(be.sessionsCreated) != 1 || be.sessionsCreated[0].UIMode != checkout.UIModeEmbedded {
		t.Errorf("backend did not receive embedded UIMode: %+v", be.sessionsCreated)
	}
	if be.sessionsCreated[0].ReturnURL != in.ReturnURL {
		t.Errorf("ReturnURL not propagated: got %q", be.sessionsCreated[0].ReturnURL)
	}
}

func TestCreateSession_EmbeddedRequiresReturnURL(t *testing.T) {
	be := &fakeBackend{}
	c, _ := newCheckout(t, map[string]string{"demo.pro_plan.monthly_eur": "price_m1"}, be)

	in := validInput()
	in.SuccessURL = ""
	in.CancelURL = ""
	in.UIMode = checkout.UIModeEmbedded

	_, err := c.CreateSession(t.Context(), in)
	if !errors.Is(err, checkout.ErrMissingReturnURL) {
		t.Errorf("want ErrMissingReturnURL, got %v", err)
	}
}

func TestCreateSession_HostedStillRequiresSuccessAndCancel(t *testing.T) {
	be := &fakeBackend{}
	c, _ := newCheckout(t, map[string]string{"demo.pro_plan.monthly_eur": "price_m1"}, be)

	in := validInput()
	in.SuccessURL = ""
	if _, err := c.CreateSession(t.Context(), in); !errors.Is(err, checkout.ErrMissingSuccessURL) {
		t.Errorf("want ErrMissingSuccessURL, got %v", err)
	}
}

// --- Trial support ---

func TestCreateSession_TrialDaysPlumbsThrough(t *testing.T) {
	be := &fakeBackend{}
	c, _ := newCheckout(t, map[string]string{"demo.pro_plan.monthly_eur": "price_m1"}, be)
	in := validInput()
	in.TrialDays = 14
	if _, err := c.CreateSession(t.Context(), in); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if be.sessionsCreated[0].TrialDays != 14 {
		t.Errorf("TrialDays = %d, want 14", be.sessionsCreated[0].TrialDays)
	}
}

func TestCreateSession_TrialDaysOutOfRangeRejected(t *testing.T) {
	be := &fakeBackend{}
	c, _ := newCheckout(t, map[string]string{"demo.pro_plan.monthly_eur": "price_m1"}, be)
	for _, n := range []int{-1, 731, 9999} {
		in := validInput()
		in.TrialDays = n
		_, err := c.CreateSession(t.Context(), in)
		if !errors.Is(err, checkout.ErrTrialDaysOutOfRange) {
			t.Errorf("TrialDays=%d: want ErrTrialDaysOutOfRange, got %v", n, err)
		}
	}
}

func TestCreateSession_TrialDaysOnOneTimeRejected(t *testing.T) {
	be := &fakeBackend{}
	c, _ := newCheckout(t, map[string]string{"demo.credit_pack.default": "price_cp"}, be)
	in := validInput()
	in.LineItems = []checkout.LineItem{{PriceKey: "credit_pack.default"}}
	in.TrialDays = 7
	_, err := c.CreateSession(t.Context(), in)
	if !errors.Is(err, checkout.ErrTrialOnPayment) {
		t.Errorf("want ErrTrialOnPayment, got %v", err)
	}
}

// --- Custom-amount (pay-what-you-want) ---

func TestCreateSession_CustomAmountInvalid(t *testing.T) {
	be := &fakeBackend{}
	c, _ := newCheckout(t, map[string]string{}, be)
	cases := []*checkout.CustomAmount{
		nil, // (won't hit, but sanity)
		{Currency: "", Name: "x", Min: 1, Max: 1},                   // missing currency
		{Currency: "eur", Name: "", Min: 1, Max: 1},                 // missing name
		{Currency: "eur", Name: "x", Min: 0, Max: 1},                // min < 1
		{Currency: "eur", Name: "x", Min: 5, Max: 1},                // max < min
		{Currency: "eur", Name: "x", Min: 1, Max: 100, Preset: 999}, // preset out of range
	}
	for i, ca := range cases {
		if ca == nil {
			continue
		}
		in := checkout.Input{
			SubjectID:    "s",
			LineItems:  []checkout.LineItem{{CustomAmount: ca}},
			SuccessURL: "https://x", CancelURL: "https://x",
		}
		_, err := c.CreateSession(t.Context(), in)
		if !errors.Is(err, checkout.ErrCustomAmountInvalid) {
			t.Errorf("case[%d] %+v → want ErrCustomAmountInvalid, got %v", i, ca, err)
		}
	}
}

func TestCreateSession_CustomAmountWithPriceKeyRejected(t *testing.T) {
	be := &fakeBackend{}
	c, _ := newCheckout(t, map[string]string{}, be)
	in := checkout.Input{
		SubjectID: "s",
		LineItems: []checkout.LineItem{{
			PriceKey:     "pro_plan.monthly_eur",
			CustomAmount: &checkout.CustomAmount{Currency: "eur", Name: "x", Min: 1, Max: 1},
		}},
		SuccessURL: "https://x", CancelURL: "https://x",
	}
	_, err := c.CreateSession(t.Context(), in)
	if !errors.Is(err, checkout.ErrCustomAmountAndPrice) {
		t.Errorf("want ErrCustomAmountAndPrice, got %v", err)
	}
}

func TestCreateSession_CustomAmountFlowsThroughAsOneTime(t *testing.T) {
	be := &fakeBackend{}
	c, _ := newCheckout(t, map[string]string{}, be)
	in := checkout.Input{
		SubjectID: "s",
		LineItems: []checkout.LineItem{{
			CustomAmount: &checkout.CustomAmount{
				Currency: "eur", Name: "Tip the artist", Min: 100, Max: 10000, Preset: 500,
			},
		}},
		SuccessURL: "https://x", CancelURL: "https://x",
	}
	if _, err := c.CreateSession(t.Context(), in); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if len(be.sessionsCreated) != 1 {
		t.Fatal("backend not called")
	}
	got := be.sessionsCreated[0]
	if got.Mode != checkout.ModePayment {
		t.Errorf("Mode = %q, want payment (custom_unit_amount is always one-time)", got.Mode)
	}
	if got.LineItems[0].CustomAmount == nil {
		t.Error("CustomAmount not plumbed through")
	}
	if got.LineItems[0].StripePriceID != "" {
		t.Errorf("StripePriceID should be empty for custom-amount line, got %q", got.LineItems[0].StripePriceID)
	}
}
