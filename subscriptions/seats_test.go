package subscriptions_test

import (
	"errors"
	"testing"

	"github.com/bds421/rho-stripe/subscriptions"
)

func seatedSub() *subscriptions.Subscription {
	return &subscriptions.Subscription{
		StripeID:         "sub_seats",
		SubjectID:        subscriptions.SubjectID("org_acme"),
		StripeCustomerID: "cus_acme",
		Status:           subscriptions.StatusActive,
		Items: []subscriptions.SubscriptionItem{
			{StripeID: "si_seats", PriceKey: "demo.team_plan.per_seat_eur", Quantity: 5},
		},
	}
}

func TestSetSeats_ChangesQuantity(t *testing.T) {
	be := &fakeBackend{}
	repo := subscriptions.NewMemoryRepo()
	_ = repo.Upsert(t.Context(), seatedSub())
	spec := opsSpec()
	ops := subscriptions.New(subscriptions.Config{Backend: be, Repo: repo, Spec: spec, Cache: nil})

	err := ops.SetSeats(t.Context(), "org_acme", "sub_seats", "team_plan.per_seat_eur", 9, true)
	if err != nil {
		t.Fatalf("SetSeats: %v", err)
	}
	if len(be.updateItemsCalls) != 1 {
		t.Fatalf("expected 1 updateItems call, got %d", len(be.updateItemsCalls))
	}
	call := be.updateItemsCalls[0]
	if call.Items[0].StripeItemID != "si_seats" || call.Items[0].Quantity != 9 {
		t.Errorf("backend call wrong: %+v", call.Items[0])
	}
}

func TestSetSeats_NoopWhenSame(t *testing.T) {
	be := &fakeBackend{}
	repo := subscriptions.NewMemoryRepo()
	_ = repo.Upsert(t.Context(), seatedSub())
	ops := subscriptions.New(subscriptions.Config{Backend: be, Repo: repo, Spec: opsSpec(), Cache: nil})

	if err := ops.SetSeats(t.Context(), "org_acme", "sub_seats", "team_plan.per_seat_eur", 5, true); err != nil {
		t.Fatalf("SetSeats: %v", err)
	}
	if len(be.updateItemsCalls) != 0 {
		t.Error("expected no backend call when quantity unchanged")
	}
}

func TestSetSeats_ErrorWhenSubjectMismatch(t *testing.T) {
	repo := subscriptions.NewMemoryRepo()
	_ = repo.Upsert(t.Context(), seatedSub())
	ops := subscriptions.New(subscriptions.Config{Backend: &fakeBackend{}, Repo: repo, Spec: opsSpec(), Cache: nil})

	err := ops.SetSeats(t.Context(), "org_other", "sub_seats", "team_plan.per_seat_eur", 9, true)
	if !errors.Is(err, subscriptions.ErrSubscriptionNotForSubject) {
		t.Errorf("want ErrSubscriptionNotForSubject, got %v", err)
	}
}

func TestSetSeats_ErrorWhenPriceMissing(t *testing.T) {
	repo := subscriptions.NewMemoryRepo()
	_ = repo.Upsert(t.Context(), seatedSub())
	ops := subscriptions.New(subscriptions.Config{Backend: &fakeBackend{}, Repo: repo, Spec: opsSpec(), Cache: nil})

	err := ops.SetSeats(t.Context(), "org_acme", "sub_seats", "team_plan.wrong_price", 9, true)
	if !errors.Is(err, subscriptions.ErrSeatPriceNotInSubscription) {
		t.Errorf("want ErrSeatPriceNotInSubscription, got %v", err)
	}
}

func TestAddSeats_AccumulatesAndCallsBackend(t *testing.T) {
	be := &fakeBackend{}
	repo := subscriptions.NewMemoryRepo()
	_ = repo.Upsert(t.Context(), seatedSub())
	ops := subscriptions.New(subscriptions.Config{Backend: be, Repo: repo, Spec: opsSpec(), Cache: nil})

	if err := ops.AddSeats(t.Context(), "org_acme", "sub_seats", "team_plan.per_seat_eur", 2, true); err != nil {
		t.Fatalf("AddSeats: %v", err)
	}
	if len(be.updateItemsCalls) != 1 || be.updateItemsCalls[0].Items[0].Quantity != 7 {
		t.Errorf("expected qty=7, got calls=%+v", be.updateItemsCalls)
	}
}

func TestSeatCount_ReadsMirror(t *testing.T) {
	repo := subscriptions.NewMemoryRepo()
	_ = repo.Upsert(t.Context(), seatedSub())
	ops := subscriptions.New(subscriptions.Config{Backend: &fakeBackend{}, Repo: repo, Spec: opsSpec(), Cache: nil})

	n, err := ops.SeatCount(t.Context(), "org_acme", "sub_seats", "team_plan.per_seat_eur")
	if err != nil || n != 5 {
		t.Errorf("SeatCount returned (%d,%v)", n, err)
	}
}

// --- Slice 38 follow-up: Stripe fallback when mirror hasn't seen sub ---

func TestSetSeats_MirrorMissFallsBackToStripe(t *testing.T) {
	be := &fakeBackend{
		// Mirror is empty (no Upsert), so SetSeats must hit Stripe.
		getItemsResult: []subscriptions.SubscriptionItem{
			{StripeID: "si_seats_fresh", PriceKey: "demo.team_plan.per_seat_eur", Quantity: 3},
		},
	}
	repo := subscriptions.NewMemoryRepo() // intentionally empty
	ops := subscriptions.New(subscriptions.Config{Backend: be, Repo: repo, Spec: opsSpec(), Cache: nil})

	err := ops.SetSeats(t.Context(), "org_acme", "sub_fresh", "team_plan.per_seat_eur", 9, true)
	if err != nil {
		t.Fatalf("SetSeats: %v", err)
	}
	if len(be.getItemsCalls) != 1 || be.getItemsCalls[0] != "sub_fresh" {
		t.Errorf("expected GetSubscriptionItems(sub_fresh), got %v", be.getItemsCalls)
	}
	if len(be.updateItemsCalls) != 1 {
		t.Fatalf("expected UpdateItems call, got %d", len(be.updateItemsCalls))
	}
	if be.updateItemsCalls[0].Items[0].StripeItemID != "si_seats_fresh" {
		t.Errorf("wrong item id: %+v", be.updateItemsCalls[0].Items[0])
	}
}

func TestSetSeats_MirrorMissAndStripeMissReturnsSeatNotFound(t *testing.T) {
	be := &fakeBackend{} // Stripe returns no items either
	repo := subscriptions.NewMemoryRepo()
	ops := subscriptions.New(subscriptions.Config{Backend: be, Repo: repo, Spec: opsSpec(), Cache: nil})

	err := ops.SetSeats(t.Context(), "org_acme", "sub_fresh", "team_plan.per_seat_eur", 9, true)
	if !errors.Is(err, subscriptions.ErrSeatPriceNotInSubscription) {
		t.Errorf("want ErrSeatPriceNotInSubscription, got %v", err)
	}
}

func TestSetSeats_MirrorHitWithWrongSubjectStillReturnsSubjectMismatch(t *testing.T) {
	// Ensure the new fallback path doesn't accidentally let cross-tenant
	// writes through: when the mirror has the sub but with a different
	// subject, we MUST refuse — no Stripe fallback that would skip the
	// auth check.
	be := &fakeBackend{
		getItemsResult: []subscriptions.SubscriptionItem{
			{StripeID: "si_x", PriceKey: "demo.team_plan.per_seat_eur", Quantity: 1},
		},
	}
	repo := subscriptions.NewMemoryRepo()
	_ = repo.Upsert(t.Context(), &subscriptions.Subscription{
		StripeID:  "sub_other",
		SubjectID: "org_other",
		Items: []subscriptions.SubscriptionItem{
			{StripeID: "si_other", PriceKey: "demo.team_plan.per_seat_eur", Quantity: 5},
		},
	})
	ops := subscriptions.New(subscriptions.Config{Backend: be, Repo: repo, Spec: opsSpec(), Cache: nil})

	err := ops.SetSeats(t.Context(), "org_acme", "sub_other", "team_plan.per_seat_eur", 9, true)
	if !errors.Is(err, subscriptions.ErrSubscriptionNotForSubject) {
		t.Errorf("want ErrSubscriptionNotForSubject, got %v", err)
	}
	if len(be.getItemsCalls) != 0 {
		t.Error("should NOT fall back to Stripe when mirror has the sub for a different subject")
	}
}
