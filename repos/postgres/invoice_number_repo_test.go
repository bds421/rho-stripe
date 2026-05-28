//go:build postgres_integration

package postgres_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bds421/rho-stripe/invoices"
	"github.com/bds421/rho-stripe/repos/postgres"
)

func TestPostgresInvoiceNumberRepo_NextSequential(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	r := postgres.NewInvoiceNumberRepo(db)

	first, err := r.Next(t.Context(), "AT-2026-")
	if err != nil {
		t.Fatalf("Next #1: %v", err)
	}
	if first != "AT-2026-000001" {
		t.Errorf("first = %q", first)
	}
	second, _ := r.Next(t.Context(), "AT-2026-")
	if second != "AT-2026-000002" {
		t.Errorf("second = %q", second)
	}
}

func TestPostgresInvoiceNumberRepo_PerPrefixIndependent(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	r := postgres.NewInvoiceNumberRepo(db)

	_, _ = r.Next(t.Context(), "AT-2026-")
	_, _ = r.Next(t.Context(), "AT-2026-")
	de, _ := r.Next(t.Context(), "DE-2026-")
	if de != "DE-2026-000001" {
		t.Errorf("DE prefix not independent: %q", de)
	}
}

func TestPostgresInvoiceNumberRepo_MarkUsedIdempotent(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	r := postgres.NewInvoiceNumberRepo(db)

	n, _ := r.Next(t.Context(), "X-")
	if err := r.MarkUsed(t.Context(), n, "in_1"); err != nil {
		t.Fatalf("first MarkUsed: %v", err)
	}
	if err := r.MarkUsed(t.Context(), n, "in_1"); err != nil {
		t.Errorf("duplicate MarkUsed should be no-op, got %v", err)
	}
	if err := r.MarkUsed(t.Context(), n, "in_2"); !errors.Is(err, invoices.ErrNumberAlreadyResolved) {
		t.Errorf("conflicting MarkUsed: want ErrNumberAlreadyResolved, got %v", err)
	}
}

func TestPostgresInvoiceNumberRepo_VoidedThenUsedRejected(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	r := postgres.NewInvoiceNumberRepo(db)

	n, _ := r.Next(t.Context(), "X-")
	if err := r.MarkVoided(t.Context(), n, "draft cancelled"); err != nil {
		t.Fatalf("MarkVoided: %v", err)
	}
	if err := r.MarkUsed(t.Context(), n, "in_after"); !errors.Is(err, invoices.ErrNumberAlreadyResolved) {
		t.Errorf("voided then used should fail, got %v", err)
	}
}

func TestPostgresInvoiceNumberRepo_MarkOnUnissuedRejected(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	r := postgres.NewInvoiceNumberRepo(db)

	if err := r.MarkUsed(t.Context(), "X-000001", "in_x"); !errors.Is(err, invoices.ErrNumberNotIssued) {
		t.Errorf("MarkUsed on unissued: want ErrNumberNotIssued, got %v", err)
	}
	if err := r.MarkVoided(t.Context(), "X-000001", "n/a"); !errors.Is(err, invoices.ErrNumberNotIssued) {
		t.Errorf("MarkVoided on unissued: want ErrNumberNotIssued, got %v", err)
	}
}

func TestPostgresInvoiceNumberRepo_ConcurrentNextGapless(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	r := postgres.NewInvoiceNumberRepo(db)

	const n = 25
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			num, err := r.Next(t.Context(), "C-")
			if err != nil {
				t.Errorf("Next err: %v", err)
				return
			}
			mu.Lock()
			got[num] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(got) != n {
		t.Errorf("expected %d unique numbers, got %d (collisions or gaps)", n, len(got))
	}
}

func TestPostgresInvoiceNumberRepo_CustomFormatter(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	r := postgres.NewInvoiceNumberRepo(db, postgres.WithFormatter(func(p string, c int64) string {
		return p + "INV-" + zeroPad(c, 4)
	}))
	n, err := r.Next(t.Context(), "AT-2026-")
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if n != "AT-2026-INV-0001" {
		t.Errorf("custom formatter: got %q", n)
	}
}

func TestPostgresInvoiceNumberRepo_ListIssuedOlderThan(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	r := postgres.NewInvoiceNumberRepo(db)

	// Three numbers: leave 1 "issued" (orphan), mark 1 used, mark 1 voided.
	n1, _ := r.Next(t.Context(), "X-")
	n2, _ := r.Next(t.Context(), "X-")
	n3, _ := r.Next(t.Context(), "X-")
	_ = r.MarkUsed(t.Context(), n2, "in_2")
	_ = r.MarkVoided(t.Context(), n3, "test void")

	// All three were issued just now; deadline = now+1m → all returned
	// would be 1 (only n1 still issued).
	deadline := time.Now().Add(time.Minute)
	orphans, err := r.ListIssuedOlderThan(t.Context(), deadline)
	if err != nil {
		t.Fatalf("ListIssuedOlderThan: %v", err)
	}
	if len(orphans) != 1 || orphans[0].Number != n1 {
		t.Errorf("expected exactly %q as orphan, got %+v", n1, orphans)
	}
	if orphans[0].Prefix != "X-" || orphans[0].Counter != 1 {
		t.Errorf("orphan fields wrong: %+v", orphans[0])
	}
}

func zeroPad(n int64, width int) string {
	s := []byte{}
	for n > 0 {
		s = append([]byte{byte('0' + n%10)}, s...)
		n /= 10
	}
	for len(s) < width {
		s = append([]byte{'0'}, s...)
	}
	return string(s)
}
