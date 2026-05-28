package subscriptions_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/bds421/rho-stripe/subscriptions"
	"github.com/bds421/rho-stripe/webhooks"
	stripe "github.com/stripe/stripe-go/v82"
)

// --- Repo + status semantics ---

func TestStatus_IsAccessGranting(t *testing.T) {
	for _, tc := range []struct {
		s    subscriptions.Status
		want bool
	}{
		{subscriptions.StatusActive, true},
		{subscriptions.StatusTrialing, true},
		{subscriptions.StatusPastDue, true},
		{subscriptions.StatusUnpaid, false},
		{subscriptions.StatusCanceled, false},
		{subscriptions.StatusPaused, false},
		{subscriptions.StatusIncomplete, false},
		{subscriptions.StatusIncompleteExpired, false},
	} {
		if got := tc.s.IsAccessGranting(); got != tc.want {
			t.Errorf("%s.IsAccessGranting() = %v, want %v", tc.s, got, tc.want)
		}
	}
}

func TestMemoryRepo_UpsertAndGet(t *testing.T) {
	repo := subscriptions.NewMemoryRepo()
	ctx := t.Context()

	s := &subscriptions.Subscription{
		StripeID:        "sub_1",
		SubjectID:       "org_acme",
		Status:          subscriptions.StatusActive,
		StripeUpdatedAt: time.Now(),
	}
	if err := repo.Upsert(ctx, s); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, ok, err := repo.GetByStripeID(ctx, "sub_1")
	if err != nil || !ok {
		t.Fatalf("GetByStripeID: ok=%v err=%v", ok, err)
	}
	if got.SubjectID != "org_acme" || got.Status != subscriptions.StatusActive {
		t.Errorf("Get returned wrong shape: %+v", got)
	}
}

func TestMemoryRepo_RejectsStaleEvents(t *testing.T) {
	repo := subscriptions.NewMemoryRepo()
	ctx := t.Context()

	t1 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)

	// Newer event arrives first.
	newer := &subscriptions.Subscription{StripeID: "sub_x", Status: subscriptions.StatusActive, StripeUpdatedAt: t2}
	if err := repo.Upsert(ctx, newer); err != nil {
		t.Fatal(err)
	}

	// Then an older one (out-of-order delivery from Stripe).
	older := &subscriptions.Subscription{StripeID: "sub_x", Status: subscriptions.StatusIncomplete, StripeUpdatedAt: t1}
	if err := repo.Upsert(ctx, older); err != nil {
		t.Fatal(err)
	}

	got, _, _ := repo.GetByStripeID(ctx, "sub_x")
	if got.Status != subscriptions.StatusActive {
		t.Errorf("stale event clobbered newer state; status = %s, want active", got.Status)
	}
}

func TestMemoryRepo_ListBySubject(t *testing.T) {
	repo := subscriptions.NewMemoryRepo()
	ctx := t.Context()

	_ = repo.Upsert(ctx, &subscriptions.Subscription{StripeID: "sub_a", SubjectID: "org_1", Status: subscriptions.StatusActive, StripeUpdatedAt: time.Now()})
	_ = repo.Upsert(ctx, &subscriptions.Subscription{StripeID: "sub_b", SubjectID: "org_1", Status: subscriptions.StatusCanceled, StripeUpdatedAt: time.Now()})
	_ = repo.Upsert(ctx, &subscriptions.Subscription{StripeID: "sub_c", SubjectID: "org_2", Status: subscriptions.StatusActive, StripeUpdatedAt: time.Now()})

	list, err := repo.ListBySubject(ctx, "org_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Errorf("ListBySubject(org_1) returned %d, want 2", len(list))
	}
}

// --- ApplyEventToMirror ---

func subscriptionEvent(eventType, subID, subject, customerID string, status subscriptions.Status, items []subscriptionItemFixture, createdUnix int64) webhooks.Event {
	itemDataObjs := make([]map[string]any, 0, len(items))
	for _, it := range items {
		obj := map[string]any{
			"id":                   it.id,
			"quantity":             it.qty,
			"current_period_start": it.start,
			"current_period_end":   it.end,
		}
		if it.priceID != "" {
			obj["price"] = map[string]any{
				"id":         it.priceID,
				"lookup_key": it.lookupKey,
			}
		}
		itemDataObjs = append(itemDataObjs, obj)
	}
	body := map[string]any{
		"id":                   subID,
		"object":               "subscription",
		"status":               string(status),
		"customer":             customerID,
		"cancel_at_period_end": false,
		"items":                map[string]any{"data": itemDataObjs},
		"metadata":             map[string]string{"subject_id": subject, "app_namespace": "demo"},
	}
	raw, _ := json.Marshal(body)
	return webhooks.Event{
		ID:          "evt_" + subID,
		Type:        eventType,
		CreatedUnix: createdUnix,
		Raw: &stripe.Event{
			ID:      "evt_" + subID,
			Type:    stripe.EventType(eventType),
			Created: createdUnix,
			Data:    &stripe.EventData{Raw: raw},
		},
	}
}

type subscriptionItemFixture struct {
	id        string
	qty       int64
	priceID   string
	lookupKey string
	start     int64
	end       int64
}

type fakeResolver struct{ m map[string]string }

func (f fakeResolver) PriceKeyByStripeID(id string) (string, bool) { k, ok := f.m[id]; return k, ok }

func TestApplyEventToMirror_CreatedUpsertsMirror(t *testing.T) {
	repo := subscriptions.NewMemoryRepo()
	resolver := fakeResolver{m: map[string]string{"price_pm1": "demo.pro_plan.monthly_eur"}}

	now := time.Now().Unix()
	evt := subscriptionEvent(
		"customer.subscription.created",
		"sub_create",
		"org_acme",
		"cus_1",
		subscriptions.StatusActive,
		[]subscriptionItemFixture{
			{id: "si_1", qty: 1, priceID: "price_pm1", start: now, end: now + 30*24*3600},
		},
		now,
	)

	if err := subscriptions.ApplyEventToMirror(t.Context(), repo, resolver, evt, nil); err != nil {
		t.Fatalf("ApplyEventToMirror: %v", err)
	}

	got, ok, _ := repo.GetByStripeID(t.Context(), "sub_create")
	if !ok {
		t.Fatal("mirror row not created")
	}
	if got.SubjectID != "org_acme" || got.Status != subscriptions.StatusActive {
		t.Errorf("mirror shape wrong: %+v", got)
	}
	if len(got.Items) != 1 || got.Items[0].PriceKey != "demo.pro_plan.monthly_eur" {
		t.Errorf("items not reverse-resolved: %+v", got.Items)
	}
	if got.CurrentPeriodStart.IsZero() || got.CurrentPeriodEnd.IsZero() {
		t.Errorf("period not extracted from first item")
	}
}

func TestApplyEventToMirror_DeletedForcesCanceled(t *testing.T) {
	repo := subscriptions.NewMemoryRepo()
	now := time.Now().Unix()
	evt := subscriptionEvent(
		"customer.subscription.deleted",
		"sub_del",
		"org_acme",
		"cus_1",
		subscriptions.StatusActive, // event payload still says active; lib enforces canceled
		[]subscriptionItemFixture{{id: "si_1", qty: 1, priceID: "price_pm1", start: now, end: now + 30*24*3600}},
		now,
	)
	if err := subscriptions.ApplyEventToMirror(t.Context(), repo, nil, evt, nil); err != nil {
		t.Fatalf("ApplyEventToMirror: %v", err)
	}
	got, _, _ := repo.GetByStripeID(t.Context(), "sub_del")
	if got.Status != subscriptions.StatusCanceled {
		t.Errorf("deleted should force Canceled, got %s", got.Status)
	}
	if got.EndedAt == nil {
		t.Error("deleted should set EndedAt")
	}
}

func TestApplyEventToMirror_FallsBackToLookupKey(t *testing.T) {
	repo := subscriptions.NewMemoryRepo()
	// No resolver — should use the payload's lookup_key field.
	now := time.Now().Unix()
	evt := subscriptionEvent(
		"customer.subscription.created",
		"sub_lk",
		"org_x",
		"cus_x",
		subscriptions.StatusActive,
		[]subscriptionItemFixture{
			{id: "si_1", qty: 1, priceID: "price_unknown", lookupKey: "demo.fallback_plan.monthly", start: now, end: now + 100},
		},
		now,
	)
	if err := subscriptions.ApplyEventToMirror(t.Context(), repo, nil, evt, nil); err != nil {
		t.Fatalf("ApplyEventToMirror: %v", err)
	}
	got, _, _ := repo.GetByStripeID(t.Context(), "sub_lk")
	if got.Items[0].PriceKey != "demo.fallback_plan.monthly" {
		t.Errorf("expected fallback lookup_key, got %q", got.Items[0].PriceKey)
	}
}

func TestApplyEventToMirror_FallsBackToStripeIDPrefix(t *testing.T) {
	repo := subscriptions.NewMemoryRepo()
	now := time.Now().Unix()
	evt := subscriptionEvent(
		"customer.subscription.created",
		"sub_x",
		"org_x",
		"cus_x",
		subscriptions.StatusActive,
		[]subscriptionItemFixture{
			{id: "si_1", qty: 1, priceID: "price_orphan", start: now, end: now + 100},
		},
		now,
	)
	// No resolver, no lookup_key — should fall through to stripe:<id>.
	if err := subscriptions.ApplyEventToMirror(t.Context(), repo, nil, evt, nil); err != nil {
		t.Fatalf("ApplyEventToMirror: %v", err)
	}
	got, _, _ := repo.GetByStripeID(t.Context(), "sub_x")
	if got.Items[0].PriceKey != "stripe:price_orphan" {
		t.Errorf("expected stripe:price_orphan, got %q", got.Items[0].PriceKey)
	}
}

func TestApplyEventToMirror_NonSubscriptionEventIsNoop(t *testing.T) {
	repo := subscriptions.NewMemoryRepo()
	evt := webhooks.Event{
		ID: "evt_x", Type: "invoice.paid", CreatedUnix: time.Now().Unix(),
		Raw: &stripe.Event{Data: &stripe.EventData{Raw: []byte(`{}`)}},
	}
	if err := subscriptions.ApplyEventToMirror(t.Context(), repo, nil, evt, nil); err != nil {
		t.Errorf("non-subscription event should noop, got %v", err)
	}
}

func TestApplyEventToMirror_NilRepoNoop(t *testing.T) {
	evt := subscriptionEvent("customer.subscription.created", "sub_z", "x", "y", subscriptions.StatusActive, nil, time.Now().Unix())
	if err := subscriptions.ApplyEventToMirror(t.Context(), nil, nil, evt, nil); err != nil {
		t.Errorf("nil repo should noop, got %v", err)
	}
}

// --- Hot-path queries ---

func setupHotPath(t *testing.T) subscriptions.SubscriptionRepo {
	t.Helper()
	repo := subscriptions.NewMemoryRepo()
	ctx := t.Context()
	_ = repo.Upsert(ctx, &subscriptions.Subscription{
		StripeID: "sub_pro_m", SubjectID: "org_acme", Status: subscriptions.StatusActive,
		Items:           []subscriptions.SubscriptionItem{{StripeID: "si_1", PriceKey: "demo.pro_plan.monthly_eur", Quantity: 1}},
		StripeUpdatedAt: time.Now(),
	})
	_ = repo.Upsert(ctx, &subscriptions.Subscription{
		StripeID: "sub_pro_y", SubjectID: "org_acme", Status: subscriptions.StatusTrialing,
		Items:           []subscriptions.SubscriptionItem{{StripeID: "si_2", PriceKey: "demo.pro_plan.yearly_eur", Quantity: 1}},
		StripeUpdatedAt: time.Now(),
	})
	_ = repo.Upsert(ctx, &subscriptions.Subscription{
		StripeID: "sub_dead", SubjectID: "org_acme", Status: subscriptions.StatusCanceled,
		Items:           []subscriptions.SubscriptionItem{{StripeID: "si_3", PriceKey: "demo.starter.monthly_eur", Quantity: 1}},
		StripeUpdatedAt: time.Now(),
	})
	return repo
}

func TestListActive_FiltersTerminal(t *testing.T) {
	repo := setupHotPath(t)
	active, err := subscriptions.ListActive(t.Context(), repo, "org_acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 2 {
		t.Errorf("ListActive returned %d, want 2 (canceled excluded)", len(active))
	}
}

func TestHasActivePrice_ExactMatch(t *testing.T) {
	repo := setupHotPath(t)
	ok, err := subscriptions.HasActivePrice(t.Context(), repo, "org_acme", "demo.pro_plan.monthly_eur")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("expected match on exact monthly key")
	}
}

func TestHasActivePrice_GlobMatch(t *testing.T) {
	repo := setupHotPath(t)
	ok, _ := subscriptions.HasActivePrice(t.Context(), repo, "org_acme", "demo.pro_plan.*")
	if !ok {
		t.Error("expected glob match on demo.pro_plan.*")
	}
	ok, _ = subscriptions.HasActivePrice(t.Context(), repo, "org_acme", "demo.starter.*")
	if ok {
		t.Error("starter is canceled — glob should NOT match active")
	}
}

func TestHasActivePrice_NoMatch(t *testing.T) {
	repo := setupHotPath(t)
	ok, _ := subscriptions.HasActivePrice(t.Context(), repo, "org_acme", "demo.enterprise.*")
	if ok {
		t.Error("unexpected match for enterprise prefix")
	}
}

func TestListByPriceKey_GlobReturnsMatchingSubs(t *testing.T) {
	repo := setupHotPath(t)
	subs, err := subscriptions.ListByPriceKey(t.Context(), repo, "org_acme", "demo.pro_plan.*")
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 2 {
		t.Errorf("ListByPriceKey(pro_plan.*) returned %d, want 2", len(subs))
	}
}

func TestListActive_OtherSubjectsIgnored(t *testing.T) {
	repo := setupHotPath(t)
	subs, _ := subscriptions.ListActive(t.Context(), repo, "org_other")
	if len(subs) != 0 {
		t.Errorf("other subject returned %d, want 0", len(subs))
	}
}

// guard against accidental import drift
var _ = fmt.Sprintf
