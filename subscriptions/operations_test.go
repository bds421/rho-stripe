package subscriptions_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bds421/rho-stripe/catalog"
	"github.com/bds421/rho-stripe/subscriptions"
)

// fakeBackend records mutation calls for assertions.
type fakeBackend struct {
	mu sync.Mutex

	cancelAtPeriodCalls []struct {
		ID string
		On bool
	}
	cancelNowCalls []struct {
		ID   string
		Opts subscriptions.CancelNowOptions
	}
	updateItemsCalls []struct {
		ID    string
		Items []subscriptions.ItemChange
		Opts  subscriptions.UpdateOptions
	}
	applyCouponCalls  []struct{ SubID, CouponID string }
	removeCouponCalls []string
	reconcileResult   []subscriptions.ReconciledSubscription
	scheduleCalls     []subscriptions.ScheduleBackendInput
	getItemsResult    []subscriptions.SubscriptionItem
	getItemsErr       error
	getItemsCalls     []string
	previewResult     subscriptions.MigratePreview
	previewErr        error
	previewCalls      []previewCall
	pauseCalls        []pauseCall
	resumeCalls       []string

	err error
}

func (b *fakeBackend) CancelAtPeriodEnd(_ context.Context, id string, on bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cancelAtPeriodCalls = append(b.cancelAtPeriodCalls, struct {
		ID string
		On bool
	}{id, on})
	return b.err
}
func (b *fakeBackend) CancelNow(_ context.Context, id string, opts subscriptions.CancelNowOptions) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cancelNowCalls = append(b.cancelNowCalls, struct {
		ID   string
		Opts subscriptions.CancelNowOptions
	}{id, opts})
	return b.err
}
func (b *fakeBackend) UpdateItems(_ context.Context, id string, items []subscriptions.ItemChange, opts subscriptions.UpdateOptions) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.updateItemsCalls = append(b.updateItemsCalls, struct {
		ID    string
		Items []subscriptions.ItemChange
		Opts  subscriptions.UpdateOptions
	}{id, items, opts})
	return b.err
}
func (b *fakeBackend) ApplyCoupon(_ context.Context, subID, couponID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.applyCouponCalls = append(b.applyCouponCalls, struct{ SubID, CouponID string }{subID, couponID})
	return b.err
}
func (b *fakeBackend) RemoveCoupon(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.removeCouponCalls = append(b.removeCouponCalls, id)
	return b.err
}
func (b *fakeBackend) ListSince(_ context.Context, _ time.Time) ([]subscriptions.ReconciledSubscription, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return nil, b.err
	}
	return b.reconcileResult, nil
}
func (b *fakeBackend) CreateInvoiced(_ context.Context, _ subscriptions.InvoicedSubscriptionInput) (*subscriptions.Subscription, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return nil, b.err
	}
	return &subscriptions.Subscription{StripeID: "sub_invoiced_fake"}, nil
}
func (b *fakeBackend) CreateSubscriptionSchedule(_ context.Context, in subscriptions.ScheduleBackendInput) (*subscriptions.Schedule, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return nil, b.err
	}
	b.scheduleCalls = append(b.scheduleCalls, in)
	return &subscriptions.Schedule{StripeID: "sub_sched_fake"}, nil
}
func (b *fakeBackend) GetSubscriptionItems(_ context.Context, id string) ([]subscriptions.SubscriptionItem, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.getItemsCalls = append(b.getItemsCalls, id)
	if b.getItemsErr != nil {
		return nil, b.getItemsErr
	}
	return b.getItemsResult, nil
}
func (b *fakeBackend) Pause(_ context.Context, id string, opts subscriptions.PauseOptions) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pauseCalls = append(b.pauseCalls, pauseCall{ID: id, Opts: opts})
	return b.err
}
func (b *fakeBackend) Resume(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resumeCalls = append(b.resumeCalls, id)
	return b.err
}

type pauseCall struct {
	ID   string
	Opts subscriptions.PauseOptions
}

func (b *fakeBackend) PreviewItemChange(_ context.Context, id string, items []subscriptions.ItemChange) (subscriptions.MigratePreview, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.previewCalls = append(b.previewCalls, previewCall{ID: id, Items: items})
	if b.previewErr != nil {
		return subscriptions.MigratePreview{}, b.previewErr
	}
	return b.previewResult, nil
}

type previewCall struct {
	ID    string
	Items []subscriptions.ItemChange
}

func opsSpec() *catalog.Spec {
	return catalog.MustSpec(catalog.Spec{
		Namespace: "demo",
		Products: map[string]catalog.Product{
			"pro_plan": {
				Name:        "Pro",
				TaxCategory: catalog.TaxCategorySaaS,
				Prices: map[string]catalog.Price{
					"monthly_eur": {Amount: 4900, Currency: "eur", Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
					"yearly_eur":  {Amount: 49000, Currency: "eur", Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalYear},
				},
			},
		},
		Coupons: map[string]catalog.Coupon{
			"SAVE20": {PercentOff: 20, Duration: catalog.CouponDurationOnce},
		},
	})
}

// stubLister implements catalog.PriceLister for cache warmup.
type stubLister struct{}

func (stubLister) ListByLookupKeys(_ context.Context, keys []string) ([]catalog.ResolvedPrice, error) {
	out := make([]catalog.ResolvedPrice, 0, len(keys))
	for _, k := range keys {
		out = append(out, catalog.ResolvedPrice{LookupKey: k, PriceID: "price_" + k, Active: true})
	}
	return out, nil
}

func warmedCache(t *testing.T, spec *catalog.Spec) *catalog.Cache {
	t.Helper()
	cache := catalog.NewCache(stubLister{})
	if err := cache.Warm(t.Context(), spec); err != nil {
		t.Fatalf("warm cache: %v", err)
	}
	return cache
}

func opsRepoWith(t *testing.T, sub *subscriptions.Subscription) subscriptions.SubscriptionRepo {
	t.Helper()
	repo := subscriptions.NewMemoryRepo()
	if sub != nil {
		_ = repo.Upsert(t.Context(), sub)
	}
	return repo
}

func TestOperations_CancelAtPeriodEnd(t *testing.T) {
	be := &fakeBackend{}
	ops := subscriptions.New(subscriptions.Config{Backend: be, Repo: opsRepoWith(t, nil)})
	if err := ops.CancelAtPeriodEnd(t.Context(), "sub_1"); err != nil {
		t.Fatalf("CancelAtPeriodEnd: %v", err)
	}
	if len(be.cancelAtPeriodCalls) != 1 || !be.cancelAtPeriodCalls[0].On || be.cancelAtPeriodCalls[0].ID != "sub_1" {
		t.Errorf("backend call wrong: %+v", be.cancelAtPeriodCalls)
	}
}

func TestOperations_UndoCancelAtPeriodEnd(t *testing.T) {
	be := &fakeBackend{}
	ops := subscriptions.New(subscriptions.Config{Backend: be, Repo: opsRepoWith(t, nil)})
	if err := ops.UndoCancelAtPeriodEnd(t.Context(), "sub_1"); err != nil {
		t.Fatalf("UndoCancelAtPeriodEnd: %v", err)
	}
	if be.cancelAtPeriodCalls[0].On {
		t.Error("UndoCancelAtPeriodEnd should pass cancelAtPeriodEnd=false")
	}
}

func TestOperations_CancelNow(t *testing.T) {
	be := &fakeBackend{}
	ops := subscriptions.New(subscriptions.Config{Backend: be, Repo: opsRepoWith(t, nil)})
	err := ops.CancelNow(t.Context(), "sub_x", subscriptions.CancelNowOptions{Prorate: true, Reason: "test"})
	if err != nil {
		t.Fatalf("CancelNow: %v", err)
	}
	if !be.cancelNowCalls[0].Opts.Prorate {
		t.Error("Prorate not passed through")
	}
	if be.cancelNowCalls[0].Opts.Reason != "test" {
		t.Errorf("Reason = %q", be.cancelNowCalls[0].Opts.Reason)
	}
}

func TestOperations_Migrate_HappyPath(t *testing.T) {
	spec := opsSpec()
	cache := warmedCache(t, spec)
	repo := opsRepoWith(t, &subscriptions.Subscription{
		StripeID:        "sub_m1",
		SubjectID:       "org_acme",
		Status:          subscriptions.StatusActive,
		StripeUpdatedAt: time.Now(),
		Items: []subscriptions.SubscriptionItem{
			{StripeID: "si_m1", PriceKey: "demo.pro_plan.monthly_eur", Quantity: 1},
		},
	})
	be := &fakeBackend{}
	ops := subscriptions.New(subscriptions.Config{Backend: be, Repo: repo, Spec: spec, Cache: cache})

	err := ops.Migrate(t.Context(), subscriptions.MigrateInput{
		StripeSubID:  "sub_m1",
		FromPriceKey: "pro_plan.monthly_eur",
		ToPriceKey:   "pro_plan.yearly_eur",
		Prorate:      true,
	})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(be.updateItemsCalls) != 1 {
		t.Fatalf("expected 1 update call, got %d", len(be.updateItemsCalls))
	}
	c := be.updateItemsCalls[0]
	if c.ID != "sub_m1" {
		t.Errorf("ID = %q", c.ID)
	}
	if c.Opts.ProrationBehavior != "create_prorations" {
		t.Errorf("ProrationBehavior = %q (Prorate=true should map to create_prorations)", c.Opts.ProrationBehavior)
	}
	if len(c.Items) != 2 {
		t.Fatalf("expected 2 item changes (delete + add), got %d", len(c.Items))
	}
	if !c.Items[0].Deleted || c.Items[0].StripeItemID != "si_m1" {
		t.Errorf("first item change should delete si_m1: %+v", c.Items[0])
	}
	if c.Items[1].PriceID != "price_demo.pro_plan.yearly_eur" || c.Items[1].Deleted {
		t.Errorf("second item change should add yearly_eur price: %+v", c.Items[1])
	}
}

func TestOperations_Migrate_FromPriceNotPresent(t *testing.T) {
	spec := opsSpec()
	cache := warmedCache(t, spec)
	repo := opsRepoWith(t, &subscriptions.Subscription{
		StripeID:        "sub_x",
		Status:          subscriptions.StatusActive,
		StripeUpdatedAt: time.Now(),
		Items: []subscriptions.SubscriptionItem{
			{StripeID: "si_x", PriceKey: "demo.pro_plan.yearly_eur"},
		},
	})
	ops := subscriptions.New(subscriptions.Config{Backend: &fakeBackend{}, Repo: repo, Spec: spec, Cache: cache})
	err := ops.Migrate(t.Context(), subscriptions.MigrateInput{
		StripeSubID:  "sub_x",
		FromPriceKey: "pro_plan.monthly_eur", // sub doesn't have monthly
		ToPriceKey:   "pro_plan.yearly_eur",
	})
	if !errors.Is(err, subscriptions.ErrFromPriceNotOnSub) {
		t.Errorf("expected ErrFromPriceNotOnSub, got %v", err)
	}
}

func TestOperations_Migrate_SubscriptionNotInMirror(t *testing.T) {
	spec := opsSpec()
	cache := warmedCache(t, spec)
	ops := subscriptions.New(subscriptions.Config{Backend: &fakeBackend{}, Repo: opsRepoWith(t, nil), Spec: spec, Cache: cache})
	err := ops.Migrate(t.Context(), subscriptions.MigrateInput{
		StripeSubID:  "sub_missing",
		FromPriceKey: "pro_plan.monthly_eur",
		ToPriceKey:   "pro_plan.yearly_eur",
	})
	if !errors.Is(err, subscriptions.ErrSubscriptionNotInMirror) {
		t.Errorf("expected ErrSubscriptionNotInMirror, got %v", err)
	}
}

func TestOperations_Migrate_RequiresCacheAndSpec(t *testing.T) {
	ops := subscriptions.New(subscriptions.Config{Backend: &fakeBackend{}, Repo: opsRepoWith(t, nil)})
	err := ops.Migrate(t.Context(), subscriptions.MigrateInput{StripeSubID: "x", FromPriceKey: "a", ToPriceKey: "b"})
	if !errors.Is(err, subscriptions.ErrMigrateNeedsCacheAndSpec) {
		t.Errorf("expected ErrMigrateNeedsCacheAndSpec, got %v", err)
	}
}

func TestOperations_ApplyCoupon_Namespaced(t *testing.T) {
	spec := opsSpec()
	be := &fakeBackend{}
	ops := subscriptions.New(subscriptions.Config{Backend: be, Repo: opsRepoWith(t, nil), Spec: spec})
	if err := ops.ApplyCoupon(t.Context(), "sub_c", "SAVE20"); err != nil {
		t.Fatalf("ApplyCoupon: %v", err)
	}
	if len(be.applyCouponCalls) != 1 || be.applyCouponCalls[0].CouponID != "SAVE20_demo" {
		t.Errorf("ApplyCoupon should namespace SAVE20 → SAVE20_demo, got %+v", be.applyCouponCalls)
	}
}

func TestOperations_RemoveCoupon(t *testing.T) {
	be := &fakeBackend{}
	ops := subscriptions.New(subscriptions.Config{Backend: be, Repo: opsRepoWith(t, nil), Spec: opsSpec()})
	if err := ops.RemoveCoupon(t.Context(), "sub_c"); err != nil {
		t.Fatalf("RemoveCoupon: %v", err)
	}
	if len(be.removeCouponCalls) != 1 || be.removeCouponCalls[0] != "sub_c" {
		t.Errorf("RemoveCoupon wrong: %v", be.removeCouponCalls)
	}
}

func TestOperations_HotPathHelpers_AcceptUnnamespacedGlob(t *testing.T) {
	spec := opsSpec()
	repo := opsRepoWith(t, &subscriptions.Subscription{
		StripeID:        "sub_p",
		SubjectID:       "org_acme",
		Status:          subscriptions.StatusActive,
		StripeUpdatedAt: time.Now(),
		Items: []subscriptions.SubscriptionItem{
			{StripeID: "si", PriceKey: "demo.pro_plan.monthly_eur"},
		},
	})
	ops := subscriptions.New(subscriptions.Config{Backend: &fakeBackend{}, Repo: repo, Spec: spec, Cache: nil})

	// User passes app-relative key — ops namespaces it before matching.
	ok, err := ops.HasActivePrice(t.Context(), "org_acme", "pro_plan.*")
	if err != nil || !ok {
		t.Errorf("HasActivePrice(pro_plan.*) = (%v, %v); want (true, nil)", ok, err)
	}
}

func TestReconcileFromStripe_UpsertsAndCountsApplied(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	be := &fakeBackend{
		reconcileResult: []subscriptions.ReconciledSubscription{
			{
				StripeID:         "sub_r1",
				StripeCustomerID: "cus_r1",
				Status:           subscriptions.StatusActive,
				Metadata:         map[string]string{"subject_id": "org_r"},
				UpdatedUnix:      now.Unix(),
			},
			{
				StripeID:         "sub_r2",
				StripeCustomerID: "cus_r2",
				Status:           subscriptions.StatusCanceled,
				Metadata:         map[string]string{"subject_id": "org_r"},
				UpdatedUnix:      now.Unix(),
			},
		},
	}
	repo := subscriptions.NewMemoryRepo()
	ops := subscriptions.New(subscriptions.Config{Backend: be, Repo: repo, Spec: nil, Cache: nil})

	stats, err := ops.ReconcileFromStripe(t.Context(), now.Add(-time.Hour), nil)
	if err != nil {
		t.Fatalf("ReconcileFromStripe: %v", err)
	}
	if stats.Listed != 2 || stats.Upserted != 2 {
		t.Errorf("stats wrong: %+v", stats)
	}
	subs, _ := repo.ListBySubject(t.Context(), "org_r")
	if len(subs) != 2 {
		t.Errorf("repo has %d subs, want 2", len(subs))
	}
}

func TestReconcileFromStripe_PropagatesListError(t *testing.T) {
	be := &fakeBackend{err: errors.New("stripe down")}
	ops := subscriptions.New(subscriptions.Config{Backend: be, Repo: subscriptions.NewMemoryRepo(), Spec: nil, Cache: nil})
	_, err := ops.ReconcileFromStripe(t.Context(), time.Now().Add(-time.Hour), nil)
	if err == nil {
		t.Error("expected error from failed ListSince")
	}
}

// customerListerBackend embeds fakeBackend and adds the OPTIONAL
// CustomerSubscriptionLister capability, so ReconcileSubject takes its fast
// (subject-scoped) path. Plain fakeBackend deliberately omits it — that is what
// the unsupported-backend test exercises.
type customerListerBackend struct {
	*fakeBackend
	byCustomer      []subscriptions.ReconciledSubscription
	byCustomerErr   error
	byCustomerCalls []string
}

func (b *customerListerBackend) ListByCustomer(_ context.Context, customerID string) ([]subscriptions.ReconciledSubscription, error) {
	b.byCustomerCalls = append(b.byCustomerCalls, customerID)
	if b.byCustomerErr != nil {
		return nil, b.byCustomerErr
	}
	return b.byCustomer, nil
}

func TestReconcileSubject_UpsertsForCustomer(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	be := &customerListerBackend{
		fakeBackend: &fakeBackend{},
		byCustomer: []subscriptions.ReconciledSubscription{
			{
				StripeID:         "sub_c1",
				StripeCustomerID: "cus_target",
				Status:           subscriptions.StatusActive,
				Metadata:         map[string]string{"subject_id": "org_target"},
				UpdatedUnix:      now.Unix(),
			},
		},
	}
	repo := subscriptions.NewMemoryRepo()
	ops := subscriptions.New(subscriptions.Config{Backend: be, Repo: repo, Spec: nil, Cache: nil})

	stats, err := ops.ReconcileSubject(t.Context(), "cus_target", nil)
	if err != nil {
		t.Fatalf("ReconcileSubject: %v", err)
	}
	if stats.Listed != 1 || stats.Upserted != 1 {
		t.Errorf("stats wrong: %+v", stats)
	}
	if len(be.byCustomerCalls) != 1 || be.byCustomerCalls[0] != "cus_target" {
		t.Errorf("ListByCustomer calls = %v; want [cus_target]", be.byCustomerCalls)
	}
	subs, _ := repo.ListBySubject(t.Context(), "org_target")
	if len(subs) != 1 {
		t.Errorf("repo has %d subs, want 1", len(subs))
	}
}

func TestReconcileSubject_EmptyCustomerIsNoop(t *testing.T) {
	be := &customerListerBackend{fakeBackend: &fakeBackend{}}
	ops := subscriptions.New(subscriptions.Config{Backend: be, Repo: subscriptions.NewMemoryRepo(), Spec: nil, Cache: nil})

	stats, err := ops.ReconcileSubject(t.Context(), "", nil)
	if err != nil {
		t.Fatalf("ReconcileSubject(empty): %v", err)
	}
	if stats.Listed != 0 || stats.Upserted != 0 {
		t.Errorf("empty-customer stats wrong: %+v", stats)
	}
	if len(be.byCustomerCalls) != 0 {
		t.Errorf("ListByCustomer should not be called for empty customer; got %v", be.byCustomerCalls)
	}
}

func TestReconcileSubject_UnsupportedBackendReturnsSentinel(t *testing.T) {
	// Plain fakeBackend does NOT implement CustomerSubscriptionLister.
	ops := subscriptions.New(subscriptions.Config{Backend: &fakeBackend{}, Repo: subscriptions.NewMemoryRepo(), Spec: nil, Cache: nil})

	_, err := ops.ReconcileSubject(t.Context(), "cus_x", nil)
	if !errors.Is(err, subscriptions.ErrSubjectReconcileUnsupported) {
		t.Errorf("err = %v; want ErrSubjectReconcileUnsupported", err)
	}
}

func TestReconcileSubject_PropagatesListError(t *testing.T) {
	be := &customerListerBackend{fakeBackend: &fakeBackend{}, byCustomerErr: errors.New("stripe down")}
	ops := subscriptions.New(subscriptions.Config{Backend: be, Repo: subscriptions.NewMemoryRepo(), Spec: nil, Cache: nil})

	_, err := ops.ReconcileSubject(t.Context(), "cus_x", nil)
	if err == nil || errors.Is(err, subscriptions.ErrSubjectReconcileUnsupported) {
		t.Errorf("err = %v; want a wrapped list error (not the unsupported sentinel)", err)
	}
}

func TestOperations_PassesBackendErrorThrough(t *testing.T) {
	be := &fakeBackend{err: errors.New("stripe boom")}
	ops := subscriptions.New(subscriptions.Config{Backend: be, Repo: opsRepoWith(t, nil)})
	if err := ops.CancelAtPeriodEnd(t.Context(), "sub_x"); err == nil || err.Error() != "stripe boom" {
		t.Errorf("expected backend error to pass through, got %v", err)
	}
}

func TestCreateSchedule_ResolvesPhasesAndCoupons(t *testing.T) {
	be := &fakeBackend{}
	spec := opsSpec()
	cache := warmedCache(t, spec)
	ops := subscriptions.New(subscriptions.Config{Backend: be, Repo: subscriptions.NewMemoryRepo(), Spec: spec, Cache: cache})

	id, err := ops.CreateSchedule(t.Context(), subscriptions.ScheduleInput{
		StripeCustomerID: "cus_1",
		Phases: []subscriptions.SchedulePhase{
			{PriceKey: "pro_plan.monthly_eur", Iterations: 3, CouponKey: "SAVE20"},
			{PriceKey: "pro_plan.monthly_eur"},
		},
		Metadata: map[string]string{"campaign": "spring"},
	})
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	if id.StripeID != "sub_sched_fake" {
		t.Errorf("schedule id: got %q", id.StripeID)
	}
	if len(be.scheduleCalls) != 1 {
		t.Fatalf("schedule calls: %d", len(be.scheduleCalls))
	}
	got := be.scheduleCalls[0]
	if got.StripeCustomerID != "cus_1" || got.EndBehavior != "release" {
		t.Errorf("backend call wrong: %+v", got)
	}
	if len(got.Phases) != 2 {
		t.Fatalf("phases: %d", len(got.Phases))
	}
	// Phase 0: namespaced key resolves; coupon namespaced.
	if got.Phases[0].StripePriceID != "price_demo.pro_plan.monthly_eur" {
		t.Errorf("phase0 price: %q", got.Phases[0].StripePriceID)
	}
	if got.Phases[0].StripeCouponID != "SAVE20_demo" {
		t.Errorf("phase0 coupon: %q", got.Phases[0].StripeCouponID)
	}
	if got.Phases[0].Quantity != 1 || got.Phases[0].Iterations != 3 {
		t.Errorf("phase0 qty/dur: %+v", got.Phases[0])
	}
	if got.Phases[1].StripeCouponID != "" {
		t.Errorf("phase1 should have no coupon")
	}
}

func TestCreateSchedule_RequiresCacheAndSpec(t *testing.T) {
	ops := subscriptions.New(subscriptions.Config{Backend: &fakeBackend{}, Repo: subscriptions.NewMemoryRepo(), Spec: nil, Cache: nil})
	_, err := ops.CreateSchedule(t.Context(), subscriptions.ScheduleInput{
		StripeCustomerID: "cus_1",
		Phases:           []subscriptions.SchedulePhase{{PriceKey: "x.y"}},
	})
	if !errors.Is(err, subscriptions.ErrMigrateNeedsCacheAndSpec) {
		t.Errorf("want ErrMigrateNeedsCacheAndSpec, got %v", err)
	}
}

func TestCreateSchedule_RejectsEmptyInputs(t *testing.T) {
	spec := opsSpec()
	cache := warmedCache(t, spec)
	ops := subscriptions.New(subscriptions.Config{Backend: &fakeBackend{}, Repo: subscriptions.NewMemoryRepo(), Spec: spec, Cache: cache})

	if _, err := ops.CreateSchedule(t.Context(), subscriptions.ScheduleInput{
		Phases: []subscriptions.SchedulePhase{{PriceKey: "pro_plan.monthly_eur"}},
	}); err == nil {
		t.Error("want error for missing customer")
	}
	if _, err := ops.CreateSchedule(t.Context(), subscriptions.ScheduleInput{
		StripeCustomerID: "cus_1",
	}); err == nil {
		t.Error("want error for missing phases")
	}
}

func TestAdjustSeats_DispatchesUpdateItems(t *testing.T) {
	be := &fakeBackend{}
	ops := subscriptions.New(subscriptions.Config{Backend: be, Repo: subscriptions.NewMemoryRepo(), Spec: nil, Cache: nil})
	if err := ops.AdjustSeats(t.Context(), "sub_1", "si_1", 7, true); err != nil {
		t.Fatalf("AdjustSeats: %v", err)
	}
	if len(be.updateItemsCalls) != 1 {
		t.Fatalf("updateItems calls: %d", len(be.updateItemsCalls))
	}
	call := be.updateItemsCalls[0]
	if call.ID != "sub_1" || len(call.Items) != 1 || call.Items[0].StripeItemID != "si_1" || call.Items[0].Quantity != 7 {
		t.Errorf("unexpected call: %+v", call)
	}
	if call.Opts.ProrationBehavior == "" {
		t.Error("expected proration behavior to be set")
	}
}

func TestAdjustSeats_RejectsInvalidArgs(t *testing.T) {
	ops := subscriptions.New(subscriptions.Config{Backend: &fakeBackend{}, Repo: subscriptions.NewMemoryRepo(), Spec: nil, Cache: nil})
	if err := ops.AdjustSeats(t.Context(), "", "si_1", 1, false); err == nil {
		t.Error("want error for empty sub id")
	}
	if err := ops.AdjustSeats(t.Context(), "sub_1", "", 1, false); err == nil {
		t.Error("want error for empty item id")
	}
	if err := ops.AdjustSeats(t.Context(), "sub_1", "si_1", 0, false); err == nil {
		t.Error("want error for zero quantity")
	}
}

// --- PreviewMigrate ---

func TestPreviewMigrate_ResolvesPricesAndReturnsBackendResult(t *testing.T) {
	spec := opsSpec()
	cache := warmedCache(t, spec)
	sub := &subscriptions.Subscription{
		StripeID:  "sub_1",
		SubjectID: "org_acme",
		Items: []subscriptions.SubscriptionItem{
			{StripeID: "si_old", PriceKey: "demo.pro_plan.monthly_eur", Quantity: 1},
		},
	}
	repo := subscriptions.NewMemoryRepo()
	_ = repo.Upsert(t.Context(), sub)
	be := &fakeBackend{
		previewResult: subscriptions.MigratePreview{
			Currency: "eur", AmountDueNow: 1500, AmountSubtotal: 1300, AmountTax: 200,
			ProrationLineItems: []subscriptions.MigratePreviewLine{
				{Description: "Unused time on Monthly", Amount: -1000},
				{Description: "Remaining time on Yearly", Amount: 2300},
			},
		},
	}
	ops := subscriptions.New(subscriptions.Config{Backend: be, Repo: repo, Spec: spec, Cache: cache})
	preview, err := ops.PreviewMigrate(t.Context(), subscriptions.MigrateInput{
		StripeSubID:  "sub_1",
		FromPriceKey: "pro_plan.monthly_eur",
		ToPriceKey:   "pro_plan.yearly_eur",
	})
	if err != nil {
		t.Fatalf("PreviewMigrate: %v", err)
	}
	if preview.AmountDueNow != 1500 {
		t.Errorf("AmountDueNow = %d", preview.AmountDueNow)
	}
	if len(preview.ProrationLineItems) != 2 {
		t.Errorf("expected 2 proration lines, got %d", len(preview.ProrationLineItems))
	}
	if len(be.previewCalls) != 1 {
		t.Fatal("backend not invoked")
	}
	call := be.previewCalls[0]
	if call.ID != "sub_1" {
		t.Errorf("sub id %q", call.ID)
	}
	if len(call.Items) != 2 || !call.Items[0].Deleted || call.Items[1].PriceID != "price_demo.pro_plan.yearly_eur" {
		t.Errorf("backend items wrong: %+v", call.Items)
	}
}

func TestPreviewMigrate_RequiresCacheAndSpec(t *testing.T) {
	ops := subscriptions.New(subscriptions.Config{Backend: &fakeBackend{}, Repo: subscriptions.NewMemoryRepo(), Spec: nil, Cache: nil})
	_, err := ops.PreviewMigrate(t.Context(), subscriptions.MigrateInput{StripeSubID: "s"})
	if !errors.Is(err, subscriptions.ErrMigrateNeedsCacheAndSpec) {
		t.Errorf("want ErrMigrateNeedsCacheAndSpec, got %v", err)
	}
}

// --- Pause / Resume ---

func TestPause_PassesOptionsAndIDThrough(t *testing.T) {
	be := &fakeBackend{}
	ops := subscriptions.New(subscriptions.Config{Backend: be, Repo: opsRepoWith(t, nil)})
	if err := ops.Pause(t.Context(), "sub_p", subscriptions.PauseOptions{
		Behavior: "mark_uncollectible",
	}); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if len(be.pauseCalls) != 1 || be.pauseCalls[0].ID != "sub_p" || be.pauseCalls[0].Opts.Behavior != "mark_uncollectible" {
		t.Errorf("pause call wrong: %+v", be.pauseCalls)
	}
}

func TestPause_RequiresSubID(t *testing.T) {
	ops := subscriptions.New(subscriptions.Config{Backend: &fakeBackend{}, Repo: opsRepoWith(t, nil)})
	if err := ops.Pause(t.Context(), "", subscriptions.PauseOptions{}); err == nil {
		t.Error("expected error for empty subID")
	}
}

func TestResume_CallsBackend(t *testing.T) {
	be := &fakeBackend{}
	ops := subscriptions.New(subscriptions.Config{Backend: be, Repo: opsRepoWith(t, nil)})
	if err := ops.Resume(t.Context(), "sub_r"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(be.resumeCalls) != 1 || be.resumeCalls[0] != "sub_r" {
		t.Errorf("resume calls wrong: %+v", be.resumeCalls)
	}
}
