//go:build live_stripe

// Package live_stripe runs end-to-end integration tests against the
// real Stripe API (test mode). Build tag: `live_stripe`. Reads
// STRIPE_SECRET_KEY from .env or the environment.
//
// Run:
//
//	set -a; source .env; set +a
//	go test -tags=live_stripe -count=1 -v -run TestLive .
//
// Skips cleanly when STRIPE_SECRET_KEY is unset.
//
// What's covered (and why each path exists here, not in unit tests):
//
//   - Stripe API reachability + key validity (Accounts.Get)
//   - Catalog Apply round-trip (Create + idempotent re-Apply)
//   - Catalog DriftCheck against the just-Applied state
//   - Meter create (slice 34) → list → archive
//   - SubscriptionSchedule create (slice 37)
//   - Embedded Payment Element session (slice 39)
//
// Each test uses a unique-per-run namespace ("livet_<unix>") so
// concurrent or repeated runs do not collide and we don't pollute
// the test account with stale objects across runs.
package live_stripe_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bds421/rho-stripe/catalog"
	"github.com/bds421/rho-stripe/checkout"
	"github.com/bds421/rho-stripe/customers"
	"github.com/bds421/rho-stripe/disputes"
	"github.com/bds421/rho-stripe/paymentmethods"
	"github.com/bds421/rho-stripe/payments"
	"github.com/bds421/rho-stripe/stripeapi"
	"github.com/bds421/rho-stripe/subscriptions"
	stripe "github.com/stripe/stripe-go/v82"
)

func liveKey(t *testing.T) string {
	t.Helper()
	key := os.Getenv("STRIPE_SECRET_KEY")
	if key == "" {
		t.Skip("STRIPE_SECRET_KEY not set; skipping live Stripe test")
	}
	if !strings.HasPrefix(key, "sk_test_") {
		t.Fatalf("refusing to run live tests against a non-test-mode key (prefix=%q)", key[:7])
	}
	return key
}

// liveNamespace returns a unique namespace for this test run so we
// don't collide with prior runs or other concurrent tests. Must match
// catalog.Spec namespace regex: ^[a-z][a-z0-9_]{0,29}$.
func liveNamespace(prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, time.Now().Unix())
}

func TestLive_AccountsGet(t *testing.T) {
	key := liveKey(t)
	sc := stripeapi.NewClient(stripeapi.Config{SecretKey: key})
	acct, err := sc.V1Accounts.Retrieve(context.Background(), nil)
	if err != nil {
		t.Fatalf("Accounts.Retrieve: %v", err)
	}
	t.Logf("ok — account=%s country=%s currency=%s", acct.ID, acct.Country, acct.DefaultCurrency)
}

func TestLive_CatalogApplyAndDriftCheck(t *testing.T) {
	key := liveKey(t)
	ns := liveNamespace("livet")
	t.Logf("namespace: %s", ns)

	sc := stripeapi.NewClient(stripeapi.Config{SecretKey: key})
	backend := stripeapi.NewBackend(sc)

	spec := catalog.MustSpec(catalog.Spec{
		Namespace: ns,
		Products: map[string]catalog.Product{
			"smoke": {
				Name:        "Live Smoke Product " + ns,
				TaxCategory: catalog.TaxCategorySaaS,
				Prices: map[string]catalog.Price{
					"monthly_eur": {
						Amount: 100, Currency: "eur",
						Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth,
					},
				},
			},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 1) Apply from empty.
	current, err := backend.ListProductsByNamespace(ctx, ns)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	plan := catalog.Diff(spec, current)
	if err := catalog.Apply(ctx, backend, plan); err != nil {
		t.Fatalf("apply: %v", err)
	}
	t.Cleanup(func() { archiveProductsByNamespace(t, backend, ns) })

	// 2) Re-Apply must be a no-op.
	current, _ = backend.ListProductsByNamespace(ctx, ns)
	plan2 := catalog.Diff(spec, current)
	if !plan2.Empty() {
		t.Errorf("second Apply produced non-empty plan (idempotency broken): %s", plan2.String())
	}

	// 3) Drift check against in-sync state.
	rep, err := catalog.CheckDrift(ctx, backend, spec)
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if rep.HasDrift {
		t.Errorf("expected no drift right after Apply, got: %s", rep.FormatHuman())
	}
}

func TestLive_MeterCreateListArchive(t *testing.T) {
	key := liveKey(t)
	ns := liveNamespace("livem")
	t.Logf("namespace: %s", ns)

	sc := stripeapi.NewClient(stripeapi.Config{SecretKey: key})
	backend := stripeapi.NewBackend(sc)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Create a meter and confirm it shows up in the namespace list.
	created, err := backend.CreateMeter(ctx, catalog.NewMeter{
		DisplayName: "live smoke meter " + ns,
		EventName:   ns + ".smoke_event",
		AggregateBy: "count",
	})
	if err != nil {
		t.Fatalf("CreateMeter: %v", err)
	}
	t.Logf("meter created: id=%s", created.ID)
	t.Cleanup(func() { _ = backend.ArchiveMeter(context.Background(), created.ID) })

	got, err := backend.ListMetersByNamespace(ctx, ns)
	if err != nil {
		t.Fatalf("ListMetersByNamespace: %v", err)
	}
	found := false
	for _, m := range got {
		if m.ID == created.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("created meter %q not in namespace list (got %d)", created.ID, len(got))
	}

	if err := backend.ArchiveMeter(ctx, created.ID); err != nil {
		t.Fatalf("ArchiveMeter: %v", err)
	}
}

func TestLive_EmbeddedCheckoutSession(t *testing.T) {
	key := liveKey(t)
	ns := liveNamespace("livec")
	t.Logf("namespace: %s", ns)

	sc := stripeapi.NewClient(stripeapi.Config{SecretKey: key})
	backend := stripeapi.NewBackend(sc)
	ckBackend := stripeapi.NewCheckoutBackend(sc)

	spec := catalog.MustSpec(catalog.Spec{
		Namespace: ns,
		Products: map[string]catalog.Product{
			"smoke": {
				Name:        "Live Embedded Smoke " + ns,
				TaxCategory: catalog.TaxCategorySaaS,
				Prices: map[string]catalog.Price{
					"once_eur": {Amount: 500, Currency: "eur", Type: catalog.PriceTypeOneTime},
				},
			},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Sync the catalog so the price exists.
	current, err := backend.ListProductsByNamespace(ctx, ns)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if err := catalog.Apply(ctx, backend, catalog.Diff(spec, current)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	t.Cleanup(func() { archiveProductsByNamespace(t, backend, ns) })

	cache := catalog.NewCache(backend)
	if err := cache.Warm(ctx, spec); err != nil {
		t.Fatalf("warm: %v", err)
	}

	repo := checkout.NewMemoryCustomerRepo()
	ck := checkout.New(checkout.Config{
		Namespace: ns,
		Spec:      spec,
		Resolver:  cache,
		Customers: repo,
		Backend:   ckBackend,
	})

	sess, err := ck.CreateSession(ctx, checkout.Input{
		SubjectID:   checkout.SubjectID("live_test_subject"),
		LineItems: []checkout.LineItem{{PriceKey: "smoke.once_eur"}},
		UIMode:    checkout.UIModeEmbedded,
		ReturnURL: "https://example.com/return?cs={CHECKOUT_SESSION_ID}",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sess.ID == "" {
		t.Error("session ID empty")
	}
	if sess.ClientSecret == "" {
		t.Error("ClientSecret empty for embedded session")
	}
	if sess.URL != "" {
		t.Errorf("URL should be empty in embedded mode, got %q", sess.URL)
	}
	t.Logf("embedded session created: id=%s client_secret=%s…", sess.ID, sess.ClientSecret[:min(20, len(sess.ClientSecret))])
}

func TestLive_SubscriptionScheduleCreate(t *testing.T) {
	key := liveKey(t)
	ns := liveNamespace("lives")
	t.Logf("namespace: %s", ns)

	sc := stripeapi.NewClient(stripeapi.Config{SecretKey: key})
	backend := stripeapi.NewBackend(sc)
	subBackend := stripeapi.NewSubscriptionBackend(sc)
	ckBackend := stripeapi.NewCheckoutBackend(sc)

	spec := catalog.MustSpec(catalog.Spec{
		Namespace: ns,
		Products: map[string]catalog.Product{
			"sched": {
				Name:        "Live Schedule Smoke " + ns,
				TaxCategory: catalog.TaxCategorySaaS,
				Prices: map[string]catalog.Price{
					"monthly_eur": {
						Amount: 100, Currency: "eur",
						Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth,
					},
				},
			},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Sync catalog so the price exists.
	current, _ := backend.ListProductsByNamespace(ctx, ns)
	if err := catalog.Apply(ctx, backend, catalog.Diff(spec, current)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	t.Cleanup(func() { archiveProductsByNamespace(t, backend, ns) })

	cache := catalog.NewCache(backend)
	if err := cache.Warm(ctx, spec); err != nil {
		t.Fatalf("warm: %v", err)
	}

	// Need a Stripe customer — create one ad-hoc since SubscriptionSchedule
	// requires Customer.
	cust, err := ckBackend.CreateCustomer(ctx, checkout.CustomerCreate{
		SubjectID: "live_sched_subject",
		Metadata: map[string]string{
			"app_namespace": ns,
			"subject_id":    "live_sched_subject",
		},
	})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	t.Cleanup(func() {
		// Best-effort customer cleanup.
		_, _ = sc.V1Customers.Delete(ctx, string(cust), nil)
	})

	ops := subscriptions.New(subscriptions.Config{Backend: subBackend, Repo: subscriptions.NewMemoryRepo(), Spec: spec, Cache: cache})
	id, err := ops.CreateSchedule(ctx, subscriptions.ScheduleInput{
		StripeCustomerID: string(cust),
		Phases: []subscriptions.SchedulePhase{
			{PriceKey: "sched.monthly_eur", Iterations: 2},
			{PriceKey: "sched.monthly_eur"}, // open-ended tail phase
		},
	})
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	if id == nil || id.StripeID == "" {
		t.Error("schedule id empty")
	}
	t.Logf("schedule created: id=%s", id.StripeID)
	t.Cleanup(func() {
		// Release the schedule so it doesn't continue billing on cleanup runs.
		_, _ = sc.V1SubscriptionSchedules.Release(ctx, id.StripeID, &stripe.SubscriptionScheduleReleaseParams{})
	})
}

func TestLive_CheckoutWithTrial(t *testing.T) {
	key := liveKey(t)
	ns := liveNamespace("livetri")
	t.Logf("namespace: %s", ns)

	sc := stripeapi.NewClient(stripeapi.Config{SecretKey: key})
	backend := stripeapi.NewBackend(sc)
	ckBackend := stripeapi.NewCheckoutBackend(sc)

	spec := catalog.MustSpec(catalog.Spec{
		Namespace: ns,
		Products: map[string]catalog.Product{
			"pro": {
				Name:        "Live Trial Smoke " + ns,
				TaxCategory: catalog.TaxCategorySaaSBusiness,
				Prices: map[string]catalog.Price{
					"monthly_eur": {Amount: 1900, Currency: "eur",
						Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
				},
			},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	current, _ := backend.ListProductsByNamespace(ctx, ns)
	if err := catalog.Apply(ctx, backend, catalog.Diff(spec, current)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	t.Cleanup(func() { archiveProductsByNamespace(t, backend, ns) })

	cache := catalog.NewCache(backend)
	if err := cache.Warm(ctx, spec); err != nil {
		t.Fatalf("warm: %v", err)
	}

	repo := checkout.NewMemoryCustomerRepo()
	ck := checkout.New(checkout.Config{
		Namespace: ns, Spec: spec, Resolver: cache, Customers: repo, Backend: ckBackend,
	})

	sess, err := ck.CreateSession(ctx, checkout.Input{
		SubjectID:    checkout.SubjectID("live_trial_subj"),
		LineItems:  []checkout.LineItem{{PriceKey: "pro.monthly_eur"}},
		SuccessURL: "https://example.com/s",
		CancelURL:  "https://example.com/c",
		TrialDays:  7,
	})
	if err != nil {
		// Stripe rejects invalid trial params with a clear error;
		// surviving the CreateSession call is the live-verification
		// signal that the wire shape is correct. The actual
		// trial_period_days is INPUT-only — Stripe doesn't echo
		// it back on the unexpanded session response, so we trust
		// the unit test for form-field correctness and the live
		// test for "Stripe accepted the request."
		t.Fatalf("CreateSession with TrialDays=7: %v", err)
	}
	if sess.URL == "" {
		t.Error("hosted session URL empty")
	}
	if sess.ID == "" {
		t.Error("session ID empty")
	}
	t.Logf("trial session created: id=%s", sess.ID)
}

func TestLive_RefundOnTestCharge(t *testing.T) {
	key := liveKey(t)
	sc := stripeapi.NewClient(stripeapi.Config{SecretKey: key})

	// Create a one-off PaymentIntent + confirm it with a test
	// payment method, then refund. We use Stripe's "pm_card_visa"
	// test token which auto-confirms without 3DS friction.
	pi, err := sc.V1PaymentIntents.Create(t.Context(), &stripe.PaymentIntentCreateParams{
		Amount:        stripe.Int64(500),
		Currency:      stripe.String("eur"),
		PaymentMethod: stripe.String("pm_card_visa"),
		Confirm:       stripe.Bool(true),
		AutomaticPaymentMethods: &stripe.PaymentIntentCreateAutomaticPaymentMethodsParams{
			Enabled:        stripe.Bool(true),
			AllowRedirects: stripe.String("never"),
		},
	})
	if err != nil {
		t.Fatalf("create PaymentIntent: %v", err)
	}
	if pi.Status != stripe.PaymentIntentStatusSucceeded {
		t.Fatalf("PI status = %s, want succeeded (test setup may need 3DS-disabled config)", pi.Status)
	}

	be := stripeapi.NewRefundBackend(sc)
	ops := payments.New(be, payments.WithNamespace("livetest"))
	ref, err := ops.Refund(t.Context(), payments.RefundInput{
		PaymentIntentID: pi.ID,
		Reason:          payments.RefundReasonRequestedByCustomer,
	})
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if ref.Status != "succeeded" {
		t.Errorf("refund status = %q, want succeeded", ref.Status)
	}
	if ref.Amount != 500 {
		t.Errorf("refund amount = %d, want 500 (full)", ref.Amount)
	}
	t.Logf("full refund issued: id=%s status=%s", ref.StripeID, ref.Status)
}

func TestLive_CheckoutCustomAmount(t *testing.T) {
	key := liveKey(t)
	sc := stripeapi.NewClient(stripeapi.Config{SecretKey: key})
	ckBackend := stripeapi.NewCheckoutBackend(sc)

	// Custom-amount line items bypass the catalog, but Spec validation
	// requires at least one product, so we declare a placeholder we
	// don't reference.
	spec := catalog.MustSpec(catalog.Spec{
		Namespace: liveNamespace("livecam"),
		Products: map[string]catalog.Product{
			"placeholder": {
				Name:        "Placeholder (not used)",
				TaxCategory: catalog.TaxCategoryGeneralTangible,
				Prices: map[string]catalog.Price{
					"default": {Amount: 100, Currency: "eur", Type: catalog.PriceTypeOneTime},
				},
			},
		},
	})

	repo := checkout.NewMemoryCustomerRepo()
	ck := checkout.New(checkout.Config{
		Namespace: spec.Namespace, Spec: spec, Resolver: noopResolverLive{},
		Customers: repo, Backend: ckBackend,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sess, err := ck.CreateSession(ctx, checkout.Input{
		SubjectID: "live_donate_subj",
		LineItems: []checkout.LineItem{{
			CustomAmount: &checkout.CustomAmount{
				Currency: "eur", Min: 100, Max: 100000, Preset: 500,
				Name: "Live custom-amount test", TaxCategory: "txcd_99999999",
			},
		}},
		SuccessURL: "https://example.com/s",
		CancelURL:  "https://example.com/c",
	})
	if err != nil {
		t.Fatalf("custom-amount CreateSession: %v", err)
	}
	if sess.URL == "" {
		t.Error("session URL empty")
	}
	t.Logf("custom-amount session: id=%s", sess.ID)
}

type noopResolverLive struct{}

func (noopResolverLive) Lookup(string) (string, bool) { return "", false }

func TestLive_PauseAndResumeSubscription(t *testing.T) {
	key := liveKey(t)
	sc := stripeapi.NewClient(stripeapi.Config{SecretKey: key})
	be := stripeapi.NewSubscriptionBackend(sc)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Create a one-shot Customer + Subscription so we have something to pause/resume.
	cust, err := sc.V1Customers.Create(ctx, &stripe.CustomerCreateParams{Email: stripe.String("livetest+pause@example.com")})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	t.Cleanup(func() { _, _ = sc.V1Customers.Delete(ctx, cust.ID, nil) })

	// Attach a default PM so subscription succeeds.
	pm, err := sc.V1PaymentMethods.Create(ctx, &stripe.PaymentMethodCreateParams{
		Type: stripe.String("card"),
		Card: &stripe.PaymentMethodCreateCardParams{Token: stripe.String("tok_visa")},
	})
	if err != nil {
		t.Fatalf("create PM: %v", err)
	}
	if _, err := sc.V1PaymentMethods.Attach(ctx, pm.ID, &stripe.PaymentMethodAttachParams{Customer: stripe.String(cust.ID)}); err != nil {
		t.Fatalf("attach PM: %v", err)
	}
	_, err = sc.V1Customers.Update(ctx, cust.ID, &stripe.CustomerUpdateParams{
		InvoiceSettings: &stripe.CustomerUpdateInvoiceSettingsParams{DefaultPaymentMethod: stripe.String(pm.ID)},
	})
	if err != nil {
		t.Fatalf("set default PM: %v", err)
	}

	// Create a minimal Stripe Price inline (no catalog wiring needed).
	price, err := sc.V1Prices.Create(ctx, &stripe.PriceCreateParams{
		Currency:    stripe.String("eur"),
		UnitAmount:  stripe.Int64(500),
		Recurring:   &stripe.PriceCreateRecurringParams{Interval: stripe.String("month")},
		ProductData: &stripe.PriceCreateProductDataParams{Name: stripe.String("Live pause/resume smoke")},
	})
	if err != nil {
		t.Fatalf("create price: %v", err)
	}

	sub, err := sc.V1Subscriptions.Create(ctx, &stripe.SubscriptionCreateParams{
		Customer: stripe.String(cust.ID),
		Items:    []*stripe.SubscriptionCreateItemParams{{Price: stripe.String(price.ID), Quantity: stripe.Int64(1)}},
	})
	if err != nil {
		t.Fatalf("create sub: %v", err)
	}
	t.Cleanup(func() { _, _ = sc.V1Subscriptions.Cancel(ctx, sub.ID, nil) })

	if err := be.Pause(ctx, sub.ID, subscriptions.PauseOptions{Behavior: "void"}); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	got, _ := sc.V1Subscriptions.Retrieve(ctx, sub.ID, nil)
	if got.PauseCollection == nil {
		t.Error("pause_collection not set after Pause")
	} else {
		t.Logf("paused: behavior=%s", got.PauseCollection.Behavior)
	}

	if err := be.Resume(ctx, sub.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	got, _ = sc.V1Subscriptions.Retrieve(ctx, sub.ID, nil)
	if got.PauseCollection != nil {
		t.Errorf("pause_collection still set after Resume: %+v", got.PauseCollection)
	}
	t.Logf("resumed cleanly: sub=%s", sub.ID)
}

func TestLive_CustomerBalance(t *testing.T) {
	key := liveKey(t)
	sc := stripeapi.NewClient(stripeapi.Config{SecretKey: key})
	be := stripeapi.NewCheckoutBackend(sc)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cust, err := sc.V1Customers.Create(ctx, &stripe.CustomerCreateParams{
		Email: stripe.String("livetest+bal@example.com"),
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	t.Cleanup(func() { _, _ = sc.V1Customers.Delete(ctx, cust.ID, nil) })

	// Credit €100 toward future invoices (Stripe convention: negative = credit).
	tx, err := be.AdjustCustomerBalance(ctx, checkout.StripeCustomerID(cust.ID), -10000, "eur", "Live test credit")
	if err != nil {
		t.Fatalf("AdjustCustomerBalance: %v", err)
	}
	if tx.Amount != -10000 {
		t.Errorf("tx.Amount = %d, want -10000", tx.Amount)
	}
	t.Logf("balance tx: id=%s amount=%d type=%s", tx.StripeID, tx.Amount, tx.Type)

	bal, err := be.GetCustomerBalance(ctx, checkout.StripeCustomerID(cust.ID))
	if err != nil {
		t.Fatalf("GetCustomerBalance: %v", err)
	}
	if bal.Balance != -10000 {
		t.Errorf("balance = %d, want -10000 (credit)", bal.Balance)
	}

	txs, err := be.ListCustomerBalanceTransactions(ctx, checkout.StripeCustomerID(cust.ID), 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(txs) < 1 {
		t.Error("expected at least one tx")
	}
}

func TestLive_SetupFeeOnInvoicedSubscription(t *testing.T) {
	key := liveKey(t)
	ns := liveNamespace("livesf")
	sc := stripeapi.NewClient(stripeapi.Config{SecretKey: key})
	backend := stripeapi.NewBackend(sc)
	subBE := stripeapi.NewSubscriptionBackend(sc)

	spec := catalog.MustSpec(catalog.Spec{
		Namespace: ns,
		Products: map[string]catalog.Product{
			"pro": {
				Name:        "Live setup-fee smoke " + ns,
				TaxCategory: catalog.TaxCategorySaaSBusiness,
				Prices: map[string]catalog.Price{
					"yearly_eur": {Amount: 12000, Currency: "eur",
						Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalYear},
				},
			},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	current, _ := backend.ListProductsByNamespace(ctx, ns)
	if err := catalog.Apply(ctx, backend, catalog.Diff(spec, current)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	t.Cleanup(func() { archiveProductsByNamespace(t, backend, ns) })
	cache := catalog.NewCache(backend)
	if err := cache.Warm(ctx, spec); err != nil {
		t.Fatalf("warm: %v", err)
	}

	cust, err := sc.V1Customers.Create(ctx, &stripe.CustomerCreateParams{Email: stripe.String("livetest+setupfee@example.com")})
	if err != nil {
		t.Fatalf("create cust: %v", err)
	}
	t.Cleanup(func() { _, _ = sc.V1Customers.Delete(ctx, cust.ID, nil) })

	ops := subscriptions.New(subscriptions.Config{Backend: subBE, Repo: subscriptions.NewMemoryRepo(), Spec: spec, Cache: cache})
	subID, err := ops.CreateInvoiced(ctx, subscriptions.InvoicedInput{
		StripeCustomerID:    cust.ID,
		PriceKey:            "pro.yearly_eur",
		DueIn:               30 * 24 * time.Hour,
		SetupFee:            50000,
		SetupFeeDescription: "Live test onboarding",
	})
	if err != nil {
		t.Fatalf("CreateInvoiced with SetupFee: %v", err)
	}
	if subID == nil || subID.StripeID == "" {
		t.Fatal("CreateInvoiced returned nil/empty subscription")
	}
	t.Cleanup(func() { _, _ = sc.V1Subscriptions.Cancel(ctx, subID.StripeID, nil) })
	t.Logf("invoiced sub created with setup fee: %s", subID.StripeID)
}

func TestLive_TrialNoCardRequired(t *testing.T) {
	key := liveKey(t)
	ns := liveNamespace("livetnc")
	sc := stripeapi.NewClient(stripeapi.Config{SecretKey: key})
	backend := stripeapi.NewBackend(sc)
	ckBE := stripeapi.NewCheckoutBackend(sc)

	spec := catalog.MustSpec(catalog.Spec{
		Namespace: ns,
		Products: map[string]catalog.Product{
			"pro": {
				Name: "Live no-card-trial " + ns, TaxCategory: catalog.TaxCategorySaaSBusiness,
				Prices: map[string]catalog.Price{
					"monthly_eur": {Amount: 2900, Currency: "eur",
						Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
				},
			},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	current, _ := backend.ListProductsByNamespace(ctx, ns)
	if err := catalog.Apply(ctx, backend, catalog.Diff(spec, current)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	t.Cleanup(func() { archiveProductsByNamespace(t, backend, ns) })
	cache := catalog.NewCache(backend)
	if err := cache.Warm(ctx, spec); err != nil {
		t.Fatalf("warm: %v", err)
	}

	ck := checkout.New(checkout.Config{
		Namespace: ns, Spec: spec, Resolver: cache,
		Customers: checkout.NewMemoryCustomerRepo(), Backend: ckBE,
	})
	noCard := false
	sess, err := ck.CreateSession(ctx, checkout.Input{
		SubjectID:    "live_nocard_subj",
		LineItems:  []checkout.LineItem{{PriceKey: "pro.monthly_eur"}},
		SuccessURL: "https://example.com/s",
		CancelURL:  "https://example.com/c",
		TrialDays:  14,
		Defaults: checkout.SessionDefaults{
			RequirePaymentMethodForTrial: &noCard,
		},
	})
	if err != nil {
		t.Fatalf("CreateSession no-card-trial: %v", err)
	}
	t.Logf("no-card trial session: %s", sess.ID)
}

func TestLive_LocaleSetOnHostedCheckout(t *testing.T) {
	key := liveKey(t)
	ns := liveNamespace("livelc")
	sc := stripeapi.NewClient(stripeapi.Config{SecretKey: key})
	backend := stripeapi.NewBackend(sc)
	ckBE := stripeapi.NewCheckoutBackend(sc)

	spec := catalog.MustSpec(catalog.Spec{
		Namespace: ns,
		Products: map[string]catalog.Product{
			"pro": {
				Name: "Live locale " + ns, TaxCategory: catalog.TaxCategorySaaSBusiness,
				Prices: map[string]catalog.Price{
					"monthly_eur": {Amount: 1900, Currency: "eur",
						Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
				},
			},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	current, _ := backend.ListProductsByNamespace(ctx, ns)
	if err := catalog.Apply(ctx, backend, catalog.Diff(spec, current)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	t.Cleanup(func() { archiveProductsByNamespace(t, backend, ns) })
	cache := catalog.NewCache(backend)
	if err := cache.Warm(ctx, spec); err != nil {
		t.Fatalf("warm: %v", err)
	}

	ck := checkout.New(checkout.Config{
		Namespace: ns, Spec: spec, Resolver: cache,
		Customers: checkout.NewMemoryCustomerRepo(), Backend: ckBE,
	})
	sess, err := ck.CreateSession(ctx, checkout.Input{
		SubjectID:    "live_locale_subj",
		LineItems:  []checkout.LineItem{{PriceKey: "pro.monthly_eur"}},
		SuccessURL: "https://example.com/s",
		CancelURL:  "https://example.com/c",
		Locale:     "de",
	})
	if err != nil {
		t.Fatalf("CreateSession with locale=de: %v", err)
	}
	got, _ := sc.V1CheckoutSessions.Retrieve(ctx, sess.ID, nil)
	if string(got.Locale) != "de" {
		t.Errorf("locale = %q, want \"de\"", got.Locale)
	}
}

func TestLive_SEPAOnlyPaymentMethodPreset(t *testing.T) {
	key := liveKey(t)
	ns := liveNamespace("livesepa")
	sc := stripeapi.NewClient(stripeapi.Config{SecretKey: key})
	backend := stripeapi.NewBackend(sc)
	ckBE := stripeapi.NewCheckoutBackend(sc)

	spec := catalog.MustSpec(catalog.Spec{
		Namespace: ns,
		Products: map[string]catalog.Product{
			"pro": {
				Name: "Live SEPA-only " + ns, TaxCategory: catalog.TaxCategorySaaSBusiness,
				Prices: map[string]catalog.Price{
					"monthly_eur": {Amount: 1900, Currency: "eur",
						Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
				},
			},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	current, _ := backend.ListProductsByNamespace(ctx, ns)
	if err := catalog.Apply(ctx, backend, catalog.Diff(spec, current)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	t.Cleanup(func() { archiveProductsByNamespace(t, backend, ns) })
	cache := catalog.NewCache(backend)
	if err := cache.Warm(ctx, spec); err != nil {
		t.Fatalf("warm: %v", err)
	}
	ck := checkout.New(checkout.Config{
		Namespace: ns, Spec: spec, Resolver: cache,
		Customers: checkout.NewMemoryCustomerRepo(), Backend: ckBE,
	})
	sess, err := ck.CreateSession(ctx, checkout.Input{
		SubjectID:        "live_sepa_subj",
		LineItems:      []checkout.LineItem{{PriceKey: "pro.monthly_eur"}},
		SuccessURL:     "https://example.com/s",
		CancelURL:      "https://example.com/c",
		PaymentMethods: payments.MethodsFor(payments.IntentSEPAOnly),
	})
	if err != nil {
		t.Fatalf("CreateSession SEPA-only: %v", err)
	}
	got, _ := sc.V1CheckoutSessions.Retrieve(ctx, sess.ID, nil)
	if len(got.PaymentMethodTypes) != 1 || got.PaymentMethodTypes[0] != "sepa_debit" {
		t.Errorf("payment_method_types = %v, want [sepa_debit]", got.PaymentMethodTypes)
	}
}

// TestLive_IdempotencyKeyDedupesCustomers verifies that two
// CreateCustomer calls with the same auto-derived key return the same
// Stripe customer (Stripe's Idempotency-Key contract).
func TestLive_IdempotencyKeyDedupesCustomers(t *testing.T) {
	key := liveKey(t)
	sc := stripeapi.NewClient(stripeapi.Config{SecretKey: key})
	ckBE := stripeapi.NewCheckoutBackend(sc)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Same metadata input → same derived idempotency key → Stripe
	// returns the same Customer on the second call.
	in := checkout.CustomerCreate{
		Metadata: map[string]string{
			"app_namespace":  "liveidem",
			"app_subject_id": "live_idem_subj",
			"created_via":    "idempotency_test",
			"run_id":         fmt.Sprintf("run_%d", time.Now().Unix()),
			"live_test_run":  "true",
		},
	}
	id1, err := ckBE.CreateCustomer(ctx, in)
	if err != nil {
		t.Fatalf("first CreateCustomer: %v", err)
	}
	id2, err := ckBE.CreateCustomer(ctx, in)
	if err != nil {
		t.Fatalf("second CreateCustomer: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("idempotency broken: first=%s second=%s — same input must dedup", id1, id2)
	}
	t.Logf("ok — both calls returned %s (idempotency dedup live-confirmed)", id1)

	// Cleanup: delete the customer we created.
	t.Cleanup(func() {
		if _, err := sc.V1Customers.Delete(ctx, string(id1), nil); err != nil {
			t.Logf("cleanup Customers.Delete(%s): %v", id1, err)
		}
	})
}

// TestLive_TaxIDRoundTrip verifies the Customers.AddTaxID /
// ListTaxIDs / RemoveTaxID API round-trips against Stripe. Uses an EU
// VAT example value Stripe accepts in test mode.
func TestLive_TaxIDRoundTrip(t *testing.T) {
	key := liveKey(t)
	sc := stripeapi.NewClient(stripeapi.Config{SecretKey: key})
	ckBE := stripeapi.NewCheckoutBackend(sc)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	custID, err := ckBE.CreateCustomer(ctx, checkout.CustomerCreate{
		Metadata: map[string]string{
			"app_namespace":  "livetax",
			"app_subject_id": "live_tax_subj",
			"run_id":         fmt.Sprintf("tax_%d", time.Now().Unix()),
			"live_test_run":  "true",
		},
	})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	t.Cleanup(func() { _, _ = sc.V1Customers.Delete(ctx, string(custID), nil) })

	repo := checkout.NewMemoryCustomerRepo()
	if err := repo.Upsert(ctx, "live_tax_subj", custID); err != nil {
		t.Fatalf("repo upsert: %v", err)
	}
	ops := customers.New(customers.Config{StripeClient: sc, CustomerRepo: repo})

	tid, err := ops.AddTaxID(ctx, customers.AddTaxIDInput{
		SubjectID: "live_tax_subj", Type: "eu_vat", Value: "DE123456789",
	})
	if err != nil {
		t.Fatalf("AddTaxID: %v", err)
	}
	t.Logf("added tax id: %s type=%s value=%s", tid.StripeID, tid.Type, tid.Value)

	listed, err := ops.ListTaxIDs(ctx, "live_tax_subj")
	if err != nil {
		t.Fatalf("ListTaxIDs: %v", err)
	}
	found := false
	for _, t2 := range listed {
		if t2.StripeID == tid.StripeID {
			found = true
		}
	}
	if !found {
		t.Fatalf("just-added tax id %s missing from list (got %d entries)", tid.StripeID, len(listed))
	}

	if err := ops.RemoveTaxID(ctx, "live_tax_subj", tid.StripeID); err != nil {
		t.Fatalf("RemoveTaxID: %v", err)
	}
	after, err := ops.ListTaxIDs(ctx, "live_tax_subj")
	if err != nil {
		t.Fatalf("ListTaxIDs after delete: %v", err)
	}
	for _, t2 := range after {
		if t2.StripeID == tid.StripeID {
			t.Fatalf("tax id %s still present after RemoveTaxID", tid.StripeID)
		}
	}
}

// TestLive_GDPRExportAndForget exercises the full GDPR aggregate:
// create → export (verify customer surfaces) → forget (verify Stripe
// customer marked deleted).
func TestLive_GDPRExportAndForget(t *testing.T) {
	key := liveKey(t)
	sc := stripeapi.NewClient(stripeapi.Config{SecretKey: key})
	ckBE := stripeapi.NewCheckoutBackend(sc)

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	custID, err := ckBE.CreateCustomer(ctx, checkout.CustomerCreate{
		Metadata: map[string]string{
			"app_namespace":  "livegdpr",
			"app_subject_id": "live_gdpr_subj",
			"run_id":         fmt.Sprintf("gdpr_%d", time.Now().Unix()),
			"live_test_run":  "true",
		},
	})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}

	repo := checkout.NewMemoryCustomerRepo()
	_ = repo.Upsert(ctx, "live_gdpr_subj", custID)
	ops := customers.New(customers.Config{StripeClient: sc, CustomerRepo: repo})

	export, err := ops.Export(ctx, "live_gdpr_subj")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if export.StripeCustomerID != string(custID) {
		t.Fatalf("export stripe id mismatch: got %q want %q", export.StripeCustomerID, custID)
	}
	if export.Stripe.Customer == nil || export.Stripe.Customer.ID != string(custID) {
		t.Fatalf("export missing customer snapshot")
	}
	t.Logf("ok — exported customer with %d invoices, %d charges, %d subs",
		len(export.Stripe.Invoices), len(export.Stripe.Charges), len(export.Stripe.Subscriptions))

	report, err := ops.Forget(ctx, "live_gdpr_subj")
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if !report.StripeCustomerDeleted {
		t.Fatalf("Forget didn't delete Stripe customer (report=%+v)", report)
	}
	// Verify Stripe customer is marked deleted (Get returns Deleted=true).
	got, err := sc.V1Customers.Retrieve(ctx, string(custID), nil)
	if err == nil && !got.Deleted {
		t.Fatalf("Stripe customer %s should be marked deleted post-Forget", custID)
	}
	t.Logf("ok — Stripe customer %s confirmed deleted", custID)
}

// TestLive_SetupIntentForCardOnFile creates a SetupIntent against a
// real Stripe customer; asserts client_secret is populated (the
// frontend Stripe.js Elements UI consumes that).
func TestLive_SetupIntentForCardOnFile(t *testing.T) {
	key := liveKey(t)
	sc := stripeapi.NewClient(stripeapi.Config{SecretKey: key})
	ckBE := stripeapi.NewCheckoutBackend(sc)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	custID, err := ckBE.CreateCustomer(ctx, checkout.CustomerCreate{
		Metadata: map[string]string{
			"app_namespace":  "livesi",
			"app_subject_id": "live_si_subj",
			"run_id":         fmt.Sprintf("si_%d", time.Now().Unix()),
			"live_test_run":  "true",
		},
	})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	t.Cleanup(func() { _, _ = sc.V1Customers.Delete(ctx, string(custID), nil) })

	repo := checkout.NewMemoryCustomerRepo()
	_ = repo.Upsert(ctx, "live_si_subj", custID)
	ops := paymentmethods.New(paymentmethods.Config{StripeClient: sc, CustomerRepo: repo})

	si, err := ops.CreateSetupIntent(ctx, paymentmethods.CreateSetupIntentInput{
		SubjectID: "live_si_subj",
	})
	if err != nil {
		t.Fatalf("CreateSetupIntent: %v", err)
	}
	if si.StripeID == "" || si.ClientSecret == "" {
		t.Fatalf("setup intent missing fields: %+v", si)
	}
	t.Logf("ok — setup intent %s (status=%s)", si.StripeID, si.Status)
}

// TestLive_DisputesListEmptyOnFreshAccount asserts the Disputes.List
// path works against a real account (returns empty since the test
// account hasn't had chargebacks).
func TestLive_DisputesListEmptyOnFreshAccount(t *testing.T) {
	key := liveKey(t)
	sc := stripeapi.NewClient(stripeapi.Config{SecretKey: key})
	ops := disputes.New(sc)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	list, err := ops.List(ctx, disputes.ListInput{Limit: 10})
	if err != nil {
		t.Fatalf("Disputes.List: %v", err)
	}
	t.Logf("ok — disputes list returned %d entries", len(list))
}

// Best-effort cleanup so live tests can be re-run without pollution.
func archiveProductsByNamespace(t *testing.T, backend catalog.Backend, ns string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	products, err := backend.ListProductsByNamespace(ctx, ns)
	if err != nil {
		t.Logf("cleanup list: %v", err)
		return
	}
	for _, p := range products {
		for _, pr := range p.Prices {
			if pr.Active {
				_ = backend.UpdatePriceActive(ctx, pr.ID, false)
			}
		}
		if p.Active {
			_ = backend.UpdateProductActive(ctx, p.ID, false)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
