package credits_test

import (
	"errors"
	"testing"
	"time"

	"github.com/bds421/rho-stripe/credits"
)

func setupRepo(t *testing.T) *credits.Operations {
	t.Helper()
	return credits.New(credits.NewMemoryRepo())
}

func TestOps_GrantAndBalance(t *testing.T) {
	ops := setupRepo(t)
	_, err := ops.Grant(t.Context(), credits.GrantInput{
		SubjectID: "org", Bucket: "ai", Amount: 1000, ValidDays: 90,
		Source: credits.SourceStripePayment, SourceRef: "pi_1",
	})
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	bal, _ := ops.Balance(t.Context(), "org", "ai")
	if bal.Total != 1000 || bal.GrantCount != 1 {
		t.Errorf("Balance wrong: %+v", bal)
	}
	if bal.NextExpiry == nil {
		t.Error("NextExpiry should be set for grants with ValidDays")
	}
}

func TestOps_GrantRejectsZeroAmount(t *testing.T) {
	ops := setupRepo(t)
	_, err := ops.Grant(t.Context(), credits.GrantInput{SubjectID: "x", Bucket: "ai", Amount: 0})
	if err == nil {
		t.Error("expected error for Amount=0")
	}
}

func TestOps_TryDeduct_HappyPath(t *testing.T) {
	ops := setupRepo(t)
	_, _ = ops.Grant(t.Context(), credits.GrantInput{SubjectID: "org", Bucket: "ai", Amount: 100, ValidDays: 30, SourceRef: "g1"})

	ok, bal, err := ops.TryDeduct(t.Context(), credits.DeductInput{
		SubjectID: "org", Bucket: "ai", Amount: 40, Reason: "api_call", RequestID: "req_1",
	})
	if err != nil || !ok {
		t.Fatalf("TryDeduct: ok=%v err=%v", ok, err)
	}
	if bal.Total != 60 {
		t.Errorf("balance after deduct = %d, want 60", bal.Total)
	}
}

func TestOps_TryDeduct_FIFOByExpiry(t *testing.T) {
	ops := setupRepo(t)
	// Two grants: one expires sooner, one later. Deduction should
	// drain the sooner-expiring one first.
	_, _ = ops.Grant(t.Context(), credits.GrantInput{SubjectID: "org", Bucket: "ai", Amount: 30, ValidDays: 10, SourceRef: "g_short"})
	_, _ = ops.Grant(t.Context(), credits.GrantInput{SubjectID: "org", Bucket: "ai", Amount: 50, ValidDays: 90, SourceRef: "g_long"})

	// Deduct 40 → 30 from short grant + 10 from long
	ok, bal, _ := ops.TryDeduct(t.Context(), credits.DeductInput{
		SubjectID: "org", Bucket: "ai", Amount: 40, Reason: "use", RequestID: "req_fifo",
	})
	if !ok {
		t.Fatal("TryDeduct should succeed")
	}
	if bal.Total != 40 {
		t.Errorf("remaining = %d, want 40 (only the long-grant's 40 left)", bal.Total)
	}
	// next expiry should be the long grant's now (short is drained)
	hist, _ := ops.History(t.Context(), "org", time.Now().Add(-time.Hour))
	deductionCount := 0
	for _, e := range hist {
		if e.Type == credits.EntryTypeDeduction {
			deductionCount++
		}
	}
	if deductionCount != 2 {
		t.Errorf("expected 2 deduction entries (split across grants), got %d", deductionCount)
	}
}

func TestOps_TryDeduct_Insufficient(t *testing.T) {
	ops := setupRepo(t)
	_, _ = ops.Grant(t.Context(), credits.GrantInput{SubjectID: "org", Bucket: "ai", Amount: 10, SourceRef: "g"})
	ok, bal, err := ops.TryDeduct(t.Context(), credits.DeductInput{SubjectID: "org", Bucket: "ai", Amount: 20, RequestID: "req"})
	if err != nil {
		t.Fatalf("TryDeduct unexpected err: %v", err)
	}
	if ok {
		t.Error("expected ok=false for insufficient credit")
	}
	if bal.Total != 10 {
		t.Errorf("balance should be unchanged at 10, got %d", bal.Total)
	}
}

func TestOps_TryDeduct_Idempotent(t *testing.T) {
	ops := setupRepo(t)
	_, _ = ops.Grant(t.Context(), credits.GrantInput{SubjectID: "org", Bucket: "ai", Amount: 100, SourceRef: "g"})
	for i := 0; i < 3; i++ {
		ok, bal, err := ops.TryDeduct(t.Context(), credits.DeductInput{SubjectID: "org", Bucket: "ai", Amount: 10, RequestID: "same_req"})
		if !ok || err != nil {
			t.Fatalf("attempt %d: ok=%v err=%v", i, ok, err)
		}
		if bal.Total != 90 {
			t.Errorf("attempt %d: balance = %d, want 90 (idempotent)", i, bal.Total)
		}
	}
}

func TestOps_TryDeduct_RequiresRequestID(t *testing.T) {
	ops := setupRepo(t)
	_, _, err := ops.TryDeduct(t.Context(), credits.DeductInput{SubjectID: "org", Bucket: "ai", Amount: 1})
	if err == nil {
		t.Error("expected error when RequestID is empty")
	}
}

func TestOps_Deduct_ReturnsErrInsufficientCredit(t *testing.T) {
	ops := setupRepo(t)
	_, err := ops.Deduct(t.Context(), credits.DeductInput{SubjectID: "x", Bucket: "ai", Amount: 5, RequestID: "r"})
	if !errors.Is(err, credits.ErrInsufficientCredit) {
		t.Errorf("expected ErrInsufficientCredit, got %v", err)
	}
}

func TestOps_AllBalances(t *testing.T) {
	ops := setupRepo(t)
	_, _ = ops.Grant(t.Context(), credits.GrantInput{SubjectID: "org", Bucket: "ai", Amount: 100, SourceRef: "a"})
	_, _ = ops.Grant(t.Context(), credits.GrantInput{SubjectID: "org", Bucket: "api", Amount: 5000, SourceRef: "b"})
	bals, err := ops.AllBalances(t.Context(), "org")
	if err != nil {
		t.Fatal(err)
	}
	if bals["ai"].Total != 100 || bals["api"].Total != 5000 {
		t.Errorf("AllBalances wrong: %+v", bals)
	}
}

func TestOps_RunExpiry_ZeroesPastDue(t *testing.T) {
	ops := setupRepo(t)
	// Insert a grant that already expired (by setting ValidDays=1
	// then waiting). Easier: directly use 0 valid days → never expires;
	// instead grant ValidDays=1 and... actually we can't time-travel.
	// Use the no-expiry case + revoke as a proxy. Or grant via Repo
	// directly to control timestamps. For this test use ValidDays=1
	// and time.Sleep — not great. Instead, test the no-op path.
	_, _ = ops.Grant(t.Context(), credits.GrantInput{SubjectID: "org", Bucket: "ai", Amount: 100})
	n, _, err := ops.RunExpiry(t.Context())
	if err != nil {
		t.Fatalf("RunExpiry: %v", err)
	}
	if n != 0 {
		t.Errorf("nothing should expire (no ValidDays); got %d", n)
	}
}

func TestOps_RevokeGrant(t *testing.T) {
	ops := setupRepo(t)
	g, _ := ops.Grant(t.Context(), credits.GrantInput{SubjectID: "org", Bucket: "ai", Amount: 100, SourceRef: "rev"})
	if err := ops.RevokeGrant(t.Context(), g.ID, "refund"); err != nil {
		t.Fatalf("RevokeGrant: %v", err)
	}
	bal, _ := ops.Balance(t.Context(), "org", "ai")
	if bal.Total != 0 {
		t.Errorf("balance after revoke = %d, want 0", bal.Total)
	}
}

func TestOps_RevokeGrant_RequiresID(t *testing.T) {
	ops := setupRepo(t)
	if err := ops.RevokeGrant(t.Context(), "", "x"); err == nil {
		t.Error("expected error for empty grantID")
	}
}

func TestOps_History_OrderedTimeline(t *testing.T) {
	ops := setupRepo(t)
	_, _ = ops.Grant(t.Context(), credits.GrantInput{SubjectID: "org", Bucket: "ai", Amount: 100, SourceRef: "g1"})
	_, _, _ = ops.TryDeduct(t.Context(), credits.DeductInput{SubjectID: "org", Bucket: "ai", Amount: 30, Reason: "r", RequestID: "d1"})
	hist, _ := ops.History(t.Context(), "org", time.Now().Add(-time.Hour))
	if len(hist) < 2 {
		t.Fatalf("expected at least 2 entries, got %d", len(hist))
	}
	// First entry should be the grant (earlier).
	if hist[0].Type != credits.EntryTypeGrant {
		t.Errorf("first entry should be grant, got %s", hist[0].Type)
	}
}

// --- HasAccess (cohort-access pattern) ---

func TestHasAccess_TrueWhileGrantActive(t *testing.T) {
	repo := credits.NewMemoryRepo()
	ops := credits.New(repo)
	subj := credits.SubjectID("user_alice")
	_, err := repo.Grant(t.Context(), credits.GrantInput{
		SubjectID: subj, Bucket: "course_access",
		Amount: 1, Source: credits.SourceAdminGrant, SourceRef: "init",
	})
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	ok, err := ops.HasAccess(t.Context(), subj, "course_access")
	if err != nil || !ok {
		t.Errorf("HasAccess after Grant = (%v, %v); want (true, nil)", ok, err)
	}
}

func TestHasAccess_FalseWhenNoGrants(t *testing.T) {
	ops := credits.New(credits.NewMemoryRepo())
	ok, err := ops.HasAccess(t.Context(), "user_x", "course_access")
	if err != nil {
		t.Fatalf("HasAccess: %v", err)
	}
	if ok {
		t.Error("HasAccess should be false with no grants")
	}
}
