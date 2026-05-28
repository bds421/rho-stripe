package invoices_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/bds421/rho-stripe/invoices"
)

func TestMemoryNumberRepo_NextIsSequential(t *testing.T) {
	r := invoices.NewMemoryNumberRepo()
	n1, err := r.Next(t.Context(), "AT-2026-")
	if err != nil || n1 != "AT-2026-000001" {
		t.Fatalf("Next #1 = (%q,%v)", n1, err)
	}
	n2, _ := r.Next(t.Context(), "AT-2026-")
	if n2 != "AT-2026-000002" {
		t.Errorf("Next #2 = %q", n2)
	}
}

func TestMemoryNumberRepo_NextIsPerPrefix(t *testing.T) {
	r := invoices.NewMemoryNumberRepo()
	_, _ = r.Next(t.Context(), "AT-2026-")
	_, _ = r.Next(t.Context(), "AT-2026-")
	deRes, _ := r.Next(t.Context(), "DE-2026-")
	if deRes != "DE-2026-000001" {
		t.Errorf("per-prefix sequence broken: %q", deRes)
	}
}

func TestMemoryNumberRepo_MarkUsedIdempotent(t *testing.T) {
	r := invoices.NewMemoryNumberRepo()
	n, _ := r.Next(t.Context(), "X-")
	if err := r.MarkUsed(t.Context(), n, "in_1"); err != nil {
		t.Fatalf("first MarkUsed: %v", err)
	}
	if err := r.MarkUsed(t.Context(), n, "in_1"); err != nil {
		t.Errorf("duplicate MarkUsed should be no-op, got %v", err)
	}
}

func TestMemoryNumberRepo_MarkUsedConflict(t *testing.T) {
	r := invoices.NewMemoryNumberRepo()
	n, _ := r.Next(t.Context(), "X-")
	_ = r.MarkUsed(t.Context(), n, "in_1")
	if err := r.MarkUsed(t.Context(), n, "in_2"); !errors.Is(err, invoices.ErrNumberAlreadyResolved) {
		t.Errorf("want ErrNumberAlreadyResolved, got %v", err)
	}
}

func TestMemoryNumberRepo_MarkVoidedThenMarkUsedRejected(t *testing.T) {
	r := invoices.NewMemoryNumberRepo()
	n, _ := r.Next(t.Context(), "X-")
	_ = r.MarkVoided(t.Context(), n, "draft cancelled")
	if err := r.MarkUsed(t.Context(), n, "in_1"); !errors.Is(err, invoices.ErrNumberAlreadyResolved) {
		t.Errorf("voided then used: want ErrNumberAlreadyResolved, got %v", err)
	}
}

func TestMemoryNumberRepo_MarkOnUnissuedRejected(t *testing.T) {
	r := invoices.NewMemoryNumberRepo()
	if err := r.MarkUsed(t.Context(), "X-000001", "in_1"); !errors.Is(err, invoices.ErrNumberNotIssued) {
		t.Errorf("want ErrNumberNotIssued, got %v", err)
	}
}

func TestMemoryNumberRepo_ConcurrentNextIsGapless(t *testing.T) {
	r := invoices.NewMemoryNumberRepo()
	const n = 50
	var wg sync.WaitGroup
	var mu sync.Mutex
	results := make(map[string]bool, n)
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
			results[num] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(results) != n {
		t.Errorf("duplicate numbers issued: got %d unique, want %d", len(results), n)
	}
}
