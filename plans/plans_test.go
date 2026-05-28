package plans_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/bds421/rho-stripe/catalog"
	"github.com/bds421/rho-stripe/credits"
	"github.com/bds421/rho-stripe/plans"
	"github.com/bds421/rho-stripe/subscriptions"
)

func tieredSpec() *catalog.Spec {
	return catalog.MustSpec(catalog.Spec{
		Namespace: "tiers_demo",
		Products: map[string]catalog.Product{
			"free": {
				Name: "Free", TaxCategory: catalog.TaxCategorySaaSPersonal,
				Metadata: map[string]string{
					"limit.max_seats":        "1",
					"limit.max_api_calls_mo": "1000",
					"feature.sso":            "false",
				},
				Prices: map[string]catalog.Price{
					"monthly": {Amount: 0, Currency: "eur", Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
				},
			},
			"pro": {
				Name: "Pro", TaxCategory: catalog.TaxCategorySaaSBusiness,
				Metadata: map[string]string{
					"limit.max_seats":        "50",
					"limit.max_api_calls_mo": "500000",
					"feature.sso":            "true",
					"feature.audit_log":      "true",
				},
				RecurringGrant: &catalog.RecurringGrant{Bucket: "api_calls", Amount: 500000, ValidDaysFromGrant: 31},
				Prices: map[string]catalog.Price{
					"monthly_eur": {Amount: 9900, Currency: "eur", Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
				},
			},
			"enterprise": {
				Name: "Enterprise", TaxCategory: catalog.TaxCategorySaaSBusiness,
				Metadata: map[string]string{
					"limit.max_seats":        "unlimited",
					"limit.max_api_calls_mo": "unlimited",
					"feature.sso":            "true",
					"feature.audit_log":      "true",
					"feature.dedicated_csm":  "true",
				},
				Prices: map[string]catalog.Price{
					"custom_eur": {Amount: 1, Currency: "eur", Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalYear},
				},
			},
		},
	})
}

func subFor(subj, productKey, priceKey string, status subscriptions.Status) *subscriptions.Subscription {
	return &subscriptions.Subscription{
		StripeID:         "sub_" + productKey,
		SubjectID:        subscriptions.SubjectID(subj),
		Status:           status,
		CurrentPeriodEnd: time.Now().Add(20 * 24 * time.Hour),
		Items: []subscriptions.SubscriptionItem{
			{StripeID: "si_" + productKey, PriceKey: "tiers_demo." + productKey + "." + priceKey, Quantity: 1},
		},
	}
}

func TestSnapshot_NoSubscriptionMeansInactive(t *testing.T) {
	repo := subscriptions.NewMemoryRepo()
	ops := plans.New(plans.Config{Spec: tieredSpec(), SubRepo: repo, CreditRepo: nil})
	snap, err := ops.SnapshotFor(t.Context(), "user_x")
	if err != nil {
		t.Fatalf("SnapshotFor: %v", err)
	}
	if snap.IsActive() {
		t.Error("IsActive should be false with no subscriptions")
	}
	if snap.HasFeature("sso") {
		t.Error("HasFeature should be false when inactive")
	}
}

func TestSnapshot_ProPlanGivesProLimitsAndFeatures(t *testing.T) {
	repo := subscriptions.NewMemoryRepo()
	_ = repo.Upsert(t.Context(), subFor("org_acme", "pro", "monthly_eur", subscriptions.StatusActive))
	ops := plans.New(plans.Config{Spec: tieredSpec(), SubRepo: repo, CreditRepo: nil})

	snap, _ := ops.SnapshotFor(t.Context(), "org_acme")
	if !snap.IsActive() {
		t.Fatal("IsActive should be true")
	}
	if !snap.HasFeature("sso") {
		t.Error("Pro plan should have sso feature")
	}
	if !snap.HasFeature("audit_log") {
		t.Error("Pro plan should have audit_log feature")
	}
	if snap.HasFeature("dedicated_csm") {
		t.Error("Pro plan should NOT have dedicated_csm feature")
	}
	v, has := snap.IntLimit("max_seats")
	if !has || v != 50 {
		t.Errorf("max_seats = (%d, %v); want (50, true)", v, has)
	}
}

func TestSnapshot_EnterprisePlanReportsUnlimitedAsMaxInt(t *testing.T) {
	repo := subscriptions.NewMemoryRepo()
	_ = repo.Upsert(t.Context(), subFor("org_acme", "enterprise", "custom_eur", subscriptions.StatusActive))
	ops := plans.New(plans.Config{Spec: tieredSpec(), SubRepo: repo, CreditRepo: nil})

	snap, _ := ops.SnapshotFor(t.Context(), "org_acme")
	v, has := snap.IntLimit("max_seats")
	if !has {
		t.Fatal("max_seats limit should exist")
	}
	if v != math.MaxInt64 {
		t.Errorf("unlimited should normalize to math.MaxInt64; got %d", v)
	}
}

func TestSnapshot_PastDueIsActiveAndInGracePeriod(t *testing.T) {
	repo := subscriptions.NewMemoryRepo()
	_ = repo.Upsert(t.Context(), subFor("org_acme", "pro", "monthly_eur", subscriptions.StatusPastDue))
	ops := plans.New(plans.Config{Spec: tieredSpec(), SubRepo: repo, CreditRepo: nil})

	snap, _ := ops.SnapshotFor(t.Context(), "org_acme")
	if !snap.IsActive() {
		t.Error("past_due should still grant access")
	}
	if !snap.InGracePeriod() {
		t.Error("past_due should be in grace period")
	}
}

func TestSnapshot_TrialingIsActiveAndTrialFlag(t *testing.T) {
	repo := subscriptions.NewMemoryRepo()
	trialEnd := time.Now().Add(5 * 24 * time.Hour)
	sub := subFor("org_acme", "pro", "monthly_eur", subscriptions.StatusTrialing)
	sub.TrialEnd = &trialEnd
	_ = repo.Upsert(t.Context(), sub)
	ops := plans.New(plans.Config{Spec: tieredSpec(), SubRepo: repo, CreditRepo: nil})

	snap, _ := ops.SnapshotFor(t.Context(), "org_acme")
	if !snap.IsActive() || !snap.IsTrialing() {
		t.Errorf("active=%v trialing=%v", snap.IsActive(), snap.IsTrialing())
	}
	if snap.TrialEndsAt == nil {
		t.Error("TrialEndsAt not surfaced")
	}
}

func TestSnapshot_AggregatesAcrossMultiplePlans(t *testing.T) {
	// Customer has free + pro (unusual but legal); MAX of limits wins.
	repo := subscriptions.NewMemoryRepo()
	_ = repo.Upsert(t.Context(), subFor("org_acme", "free", "monthly", subscriptions.StatusActive))
	_ = repo.Upsert(t.Context(), subFor("org_acme", "pro", "monthly_eur", subscriptions.StatusActive))
	ops := plans.New(plans.Config{Spec: tieredSpec(), SubRepo: repo, CreditRepo: nil})

	snap, _ := ops.SnapshotFor(t.Context(), "org_acme")
	v, _ := snap.IntLimit("max_seats")
	if v != 50 {
		t.Errorf("max_seats = %d, want 50 (MAX of free=1, pro=50)", v)
	}
}

func TestSnapshot_CreditsRemainingReadsLedger(t *testing.T) {
	repo := subscriptions.NewMemoryRepo()
	_ = repo.Upsert(t.Context(), subFor("org_acme", "pro", "monthly_eur", subscriptions.StatusActive))
	cRepo := credits.NewMemoryRepo()
	_, _ = cRepo.Grant(t.Context(), credits.GrantInput{
		SubjectID: "org_acme", Bucket: "api_calls", Amount: 12345,
		Source: credits.SourceAdminGrant, SourceRef: "test",
	})
	ops := plans.New(plans.Config{Spec: tieredSpec(), SubRepo: repo, CreditRepo: cRepo})

	snap, _ := ops.SnapshotFor(t.Context(), "org_acme")
	if got := snap.CreditsRemaining("api_calls"); got != 12345 {
		t.Errorf("CreditsRemaining = %d, want 12345", got)
	}
	if snap.CreditsRemaining("unknown_bucket") != 0 {
		t.Error("unknown bucket should be 0")
	}
}

func TestSnapshot_CreditsRemainingZeroWhenNoLedger(t *testing.T) {
	repo := subscriptions.NewMemoryRepo()
	_ = repo.Upsert(t.Context(), subFor("org_acme", "pro", "monthly_eur", subscriptions.StatusActive))
	ops := plans.New(plans.Config{Spec: tieredSpec(), SubRepo: repo, CreditRepo: nil}) // no credit repo
	snap, _ := ops.SnapshotFor(t.Context(), "org_acme")
	if snap.CreditsRemaining("api_calls") != 0 {
		t.Error("without credits repo, CreditsRemaining must be 0")
	}
}

func TestSnapshot_PlanKeysListsActivePlans(t *testing.T) {
	repo := subscriptions.NewMemoryRepo()
	_ = repo.Upsert(t.Context(), subFor("org_acme", "free", "monthly", subscriptions.StatusActive))
	_ = repo.Upsert(t.Context(), subFor("org_acme", "pro", "monthly_eur", subscriptions.StatusActive))
	ops := plans.New(plans.Config{Spec: tieredSpec(), SubRepo: repo, CreditRepo: nil})

	snap, _ := ops.SnapshotFor(t.Context(), "org_acme")
	keys := snap.PlanKeys()
	if len(keys) != 2 {
		t.Errorf("PlanKeys count = %d, want 2", len(keys))
	}
}

func TestSnapshot_CancelledSubscriptionIsNotActive(t *testing.T) {
	repo := subscriptions.NewMemoryRepo()
	_ = repo.Upsert(t.Context(), subFor("org_acme", "pro", "monthly_eur", subscriptions.StatusCanceled))
	ops := plans.New(plans.Config{Spec: tieredSpec(), SubRepo: repo, CreditRepo: nil})
	snap, _ := ops.SnapshotFor(t.Context(), "org_acme")
	if snap.IsActive() {
		t.Error("canceled subscription should NOT grant access")
	}
}

// Compile-time check that contextual lookups work with package-style calls.
var _ = func(o *plans.Operations) {
	_, _ = o.SnapshotFor(context.Background(), "x")
}
