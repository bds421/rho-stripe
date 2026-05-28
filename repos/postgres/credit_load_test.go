//go:build postgres_integration

package postgres_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bds421/rho-stripe/credits"
	"github.com/bds421/rho-stripe/repos/postgres"
)

// TestCreditRepo_LoadDeducts1000Concurrent stresses the pgadvisory-
// locked TryDeduct path. The library's safety invariant:
//
//	"Sum of (ok deducts) <= grant amount; final balance = grant - ok deducts."
//
// i.e. NO double-spend even under heavy lock contention. The lib
// surfaces lock errors to the caller (preferred over blocking
// indefinitely on contention); the caller is expected to retry
// transient errors. This test asserts the safety invariant and
// reports the contention characteristics for ops planning.
//
// Skip with -short.
func TestCreditRepo_LoadDeducts1000Concurrent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load test under -short")
	}
	db := setupDB(t)
	defer db.Close()
	repo := postgres.NewCreditRepo(db)

	const subject = credits.SubjectID("loadtest_subject")
	const bucket = "load_bucket"
	const grantAmount int64 = 500
	const workers = 1000

	_, err := repo.Grant(t.Context(), credits.GrantInput{
		SubjectID: subject, Bucket: bucket, Amount: grantAmount,
		Source: credits.SourceAdminGrant, SourceRef: "load_grant",
	})
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}

	var (
		wg               sync.WaitGroup
		ok, insufficient atomic.Int64
		errs             atomic.Int64
	)
	start := time.Now()
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(reqIdx int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			succeeded, _, err := repo.TryDeduct(ctx, credits.DeductInput{
				SubjectID:   subject,
				Bucket:    bucket,
				Amount:    1,
				RequestID: fmt.Sprintf("load_req_%d", reqIdx),
			})
			if err != nil {
				errs.Add(1)
				return
			}
			if succeeded {
				ok.Add(1)
			} else {
				insufficient.Add(1)
			}
		}(i)
	}
	wg.Wait()
	dur := time.Since(start)

	t.Logf("workers=%d ok=%d insufficient=%d transient_errors=%d elapsed=%s rate=%.0f/sec",
		workers, ok.Load(), insufficient.Load(), errs.Load(), dur, float64(workers)/dur.Seconds())

	// SAFETY INVARIANT: NO double-spend. Successful deducts cannot
	// exceed the grant amount.
	if ok.Load() > grantAmount {
		t.Errorf("DOUBLE-SPEND DETECTED: ok deducts = %d, grant = %d", ok.Load(), grantAmount)
	}

	// BALANCE INTEGRITY: remaining = grant - ok.
	bal, err := repo.Balance(t.Context(), subject, bucket)
	if err != nil {
		t.Fatalf("final Balance: %v", err)
	}
	expected := grantAmount - ok.Load()
	if bal.Total != expected {
		t.Errorf("balance integrity broken: final=%d, expected=%d (grant=%d - ok=%d)",
			bal.Total, expected, grantAmount, ok.Load())
	}

	// INFORMATIONAL: lock-contention rate. Apps that hit this at
	// production scale should add retries with backoff. The lib
	// deliberately surfaces lock errors instead of blocking on
	// contention — see docs/howto/credits-load.md.
	if errs.Load() > 0 {
		t.Logf("note: %d/%d requests hit pg_advisory_xact_lock contention (%.0f%%). "+
			"Apps under this much concurrency on a single subject should retry transient errors.",
			errs.Load(), workers, 100*float64(errs.Load())/float64(workers))
	}
}
