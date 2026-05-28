//go:build postgres_integration

package postgres_test

import (
	"testing"
	"time"

	"github.com/bds421/rho-stripe/repos/postgres"
	"github.com/bds421/rho-stripe/subscriptions"
)

func TestSubscriptionRepo_UpsertAndGet(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	repo := postgres.NewSubscriptionRepo(db)
	ctx := t.Context()

	s := &subscriptions.Subscription{
		StripeID:         "sub_pg_1",
		SubjectID:        "org_acme",
		StripeCustomerID: "cus_a",
		Status:           subscriptions.StatusActive,
		Items: []subscriptions.SubscriptionItem{
			{StripeID: "si_1", PriceKey: "demo.pro_plan.monthly_eur", Quantity: 1},
		},
		CurrentPeriodStart: time.Now().UTC().Truncate(time.Second),
		CurrentPeriodEnd:   time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second),
		StripeUpdatedAt:    time.Now().UTC().Truncate(time.Second),
	}
	if err := repo.Upsert(ctx, s); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, ok, err := repo.GetByStripeID(ctx, "sub_pg_1")
	if err != nil || !ok {
		t.Fatalf("GetByStripeID: ok=%v err=%v", ok, err)
	}
	if got.SubjectID != "org_acme" || got.Status != subscriptions.StatusActive {
		t.Errorf("scan mismatch: %+v", got)
	}
	if len(got.Items) != 1 || got.Items[0].PriceKey != "demo.pro_plan.monthly_eur" {
		t.Errorf("items not roundtripped: %+v", got.Items)
	}
}

func TestSubscriptionRepo_StaleEventRejected(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	repo := postgres.NewSubscriptionRepo(db)
	ctx := t.Context()

	t1 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)

	newer := &subscriptions.Subscription{StripeID: "sub_pg_stale", Status: subscriptions.StatusActive, StripeCustomerID: "c", StripeUpdatedAt: t2}
	if err := repo.Upsert(ctx, newer); err != nil {
		t.Fatal(err)
	}
	older := &subscriptions.Subscription{StripeID: "sub_pg_stale", Status: subscriptions.StatusIncomplete, StripeCustomerID: "c", StripeUpdatedAt: t1}
	if err := repo.Upsert(ctx, older); err != nil {
		t.Fatal(err)
	}
	got, _, _ := repo.GetByStripeID(ctx, "sub_pg_stale")
	if got.Status != subscriptions.StatusActive {
		t.Errorf("stale event clobbered newer state; status = %s, want active", got.Status)
	}
}

func TestSubscriptionRepo_ListBySubject(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	repo := postgres.NewSubscriptionRepo(db)
	ctx := t.Context()

	for _, s := range []*subscriptions.Subscription{
		{StripeID: "sub_list_a", SubjectID: "org_list", StripeCustomerID: "c1", Status: subscriptions.StatusActive, StripeUpdatedAt: time.Now()},
		{StripeID: "sub_list_b", SubjectID: "org_list", StripeCustomerID: "c1", Status: subscriptions.StatusCanceled, StripeUpdatedAt: time.Now()},
		{StripeID: "sub_other", SubjectID: "org_other", StripeCustomerID: "c2", Status: subscriptions.StatusActive, StripeUpdatedAt: time.Now()},
	} {
		if err := repo.Upsert(ctx, s); err != nil {
			t.Fatalf("Upsert %s: %v", s.StripeID, err)
		}
	}
	list, err := repo.ListBySubject(ctx, "org_list")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Errorf("ListBySubject(org_list) returned %d, want 2", len(list))
	}
}

func TestRepos_BundleIncludesSubscriptions(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	repos := postgres.NewRepos(db)
	if repos.Subscriptions == nil {
		t.Fatal("NewRepos returned nil Subscriptions")
	}
	_ = repos.Subscriptions.Upsert(t.Context(), &subscriptions.Subscription{
		StripeID: "sub_bundle", SubjectID: "x", StripeCustomerID: "c", Status: subscriptions.StatusActive,
		StripeUpdatedAt: time.Now(),
	})
}
