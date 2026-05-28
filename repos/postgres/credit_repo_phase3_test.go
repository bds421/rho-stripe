//go:build postgres_integration

package postgres_test

import (
	"errors"
	"testing"
	"time"

	"github.com/bds421/rho-stripe/credits"
	"github.com/bds421/rho-stripe/repos/postgres"
)

func TestCreditRepo_TryDeduct_HappyPath(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	repo := postgres.NewCreditRepo(db)
	ctx := t.Context()

	_, err := repo.Grant(ctx, credits.GrantInput{
		SubjectID: "org_d", Bucket: "ai", Amount: 100, ValidDays: 30,
		Source: credits.SourceStripePayment, SourceRef: "g_d",
	})
	if err != nil {
		t.Fatal(err)
	}

	ok, bal, err := repo.TryDeduct(ctx, credits.DeductInput{
		SubjectID: "org_d", Bucket: "ai", Amount: 40, Reason: "api", RequestID: "r1",
	})
	if err != nil || !ok {
		t.Fatalf("TryDeduct: ok=%v err=%v", ok, err)
	}
	if bal.Total != 60 {
		t.Errorf("balance = %d, want 60", bal.Total)
	}
}

func TestCreditRepo_TryDeduct_IdempotentOnRequestID(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	repo := postgres.NewCreditRepo(db)
	ctx := t.Context()
	_, _ = repo.Grant(ctx, credits.GrantInput{SubjectID: "org_i", Bucket: "ai", Amount: 100, SourceRef: "g_i"})

	in := credits.DeductInput{SubjectID: "org_i", Bucket: "ai", Amount: 25, Reason: "use", RequestID: "req_idem"}
	for i := 0; i < 3; i++ {
		ok, bal, err := repo.TryDeduct(ctx, in)
		if !ok || err != nil {
			t.Fatalf("attempt %d: ok=%v err=%v", i, ok, err)
		}
		if bal.Total != 75 {
			t.Errorf("attempt %d: balance = %d, want 75 (idempotent)", i, bal.Total)
		}
	}
}

func TestCreditRepo_TryDeduct_Insufficient(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	repo := postgres.NewCreditRepo(db)
	ctx := t.Context()
	_, _ = repo.Grant(ctx, credits.GrantInput{SubjectID: "org_low", Bucket: "ai", Amount: 5, SourceRef: "g"})
	ok, bal, err := repo.TryDeduct(ctx, credits.DeductInput{SubjectID: "org_low", Bucket: "ai", Amount: 50, RequestID: "r"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok {
		t.Error("expected ok=false for insufficient")
	}
	if bal.Total != 5 {
		t.Errorf("balance unchanged at 5, got %d", bal.Total)
	}
}

func TestCreditRepo_TryDeduct_FIFOByExpiry(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	repo := postgres.NewCreditRepo(db)
	ctx := t.Context()

	// Grant a short-expiry one first, then a long-expiry one.
	_, _ = repo.Grant(ctx, credits.GrantInput{SubjectID: "org_fifo", Bucket: "ai", Amount: 30, ValidDays: 10, SourceRef: "short"})
	_, _ = repo.Grant(ctx, credits.GrantInput{SubjectID: "org_fifo", Bucket: "ai", Amount: 70, ValidDays: 90, SourceRef: "long"})

	// Deduct 40 → 30 from short + 10 from long
	ok, bal, err := repo.TryDeduct(ctx, credits.DeductInput{SubjectID: "org_fifo", Bucket: "ai", Amount: 40, RequestID: "r"})
	if err != nil || !ok {
		t.Fatalf("TryDeduct: ok=%v err=%v", ok, err)
	}
	if bal.Total != 60 {
		t.Errorf("remaining = %d, want 60 (only long-grant's 60 left)", bal.Total)
	}
}

func TestCreditRepo_AllBalances(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	repo := postgres.NewCreditRepo(db)
	ctx := t.Context()
	_, _ = repo.Grant(ctx, credits.GrantInput{SubjectID: "org_ab", Bucket: "ai", Amount: 100, SourceRef: "a"})
	_, _ = repo.Grant(ctx, credits.GrantInput{SubjectID: "org_ab", Bucket: "api", Amount: 5000, SourceRef: "b"})

	bals, err := repo.AllBalances(ctx, "org_ab")
	if err != nil {
		t.Fatal(err)
	}
	if bals["ai"].Total != 100 || bals["api"].Total != 5000 {
		t.Errorf("AllBalances wrong: %+v", bals)
	}
}

func TestCreditRepo_RevokeGrant_ZerosBalance(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	repo := postgres.NewCreditRepo(db)
	ctx := t.Context()
	g, _ := repo.Grant(ctx, credits.GrantInput{SubjectID: "org_rev", Bucket: "ai", Amount: 200, SourceRef: "rev"})
	if err := repo.RevokeGrant(ctx, g.ID, "refund"); err != nil {
		t.Fatal(err)
	}
	bal, _ := repo.Balance(ctx, "org_rev", "ai")
	if bal.Total != 0 {
		t.Errorf("balance after revoke = %d, want 0", bal.Total)
	}
}

func TestCreditRepo_History_UnifiedTimeline(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	repo := postgres.NewCreditRepo(db)
	ctx := t.Context()
	_, _ = repo.Grant(ctx, credits.GrantInput{SubjectID: "org_h", Bucket: "ai", Amount: 100, SourceRef: "g_h"})
	_, _, _ = repo.TryDeduct(ctx, credits.DeductInput{SubjectID: "org_h", Bucket: "ai", Amount: 20, Reason: "use", RequestID: "r_h"})

	hist, err := repo.History(ctx, "org_h", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) < 2 {
		t.Fatalf("expected at least 2 entries, got %d", len(hist))
	}
	var sawGrant, sawDeduction bool
	for _, e := range hist {
		switch e.Type {
		case credits.EntryTypeGrant:
			sawGrant = true
		case credits.EntryTypeDeduction:
			sawDeduction = true
		}
	}
	if !sawGrant || !sawDeduction {
		t.Errorf("history missing entries: grant=%v deduction=%v", sawGrant, sawDeduction)
	}
}

func TestCreditRepo_RunExpiry_Noop(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	repo := postgres.NewCreditRepo(db)
	ctx := t.Context()
	// Grant with no expiry — RunExpiry should do nothing.
	_, _ = repo.Grant(ctx, credits.GrantInput{SubjectID: "org_ex", Bucket: "ai", Amount: 100, SourceRef: "g"})
	n, _, err := repo.RunExpiry(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("nothing should expire, got %d", n)
	}
}

// Sanity: confirm Operations + Postgres backend work together.
func TestCreditRepo_OpsDeductSurfacesErrInsufficient(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	repo := postgres.NewCreditRepo(db)
	ops := credits.New(repo)

	_, err := ops.Deduct(t.Context(), credits.DeductInput{
		SubjectID: "org_oops", Bucket: "ai", Amount: 10, RequestID: "r",
	})
	if !errors.Is(err, credits.ErrInsufficientCredit) {
		t.Errorf("expected ErrInsufficientCredit, got %v", err)
	}
}
