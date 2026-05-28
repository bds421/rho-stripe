package invoices_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/bds421/rho-stripe/invoices"
)

type fakeBackend struct {
	mu sync.Mutex

	createdDrafts         []invoices.CreateInput
	finalized             []string
	finalizedAndSent      []string
	voided                []string
	uncollectible         []string
	creditNotes           []invoices.CreditNoteInput
	listOverdueResult     []invoices.Invoice
	listInvoicesResult    []invoices.AuditEntry
	listForCustomerResult []invoices.Invoice
	err                   error
}

func (b *fakeBackend) ListInvoices(_ context.Context, _ invoices.Period) ([]invoices.AuditEntry, error) {
	return b.listInvoicesResult, b.err
}
func (b *fakeBackend) CreateDraft(_ context.Context, in invoices.CreateInput) (invoices.Invoice, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return invoices.Invoice{}, b.err
	}
	b.createdDrafts = append(b.createdDrafts, in)
	return invoices.Invoice{StripeID: "in_fake", Status: "draft", Customer: in.Customer}, nil
}
func (b *fakeBackend) Finalize(_ context.Context, id string) (invoices.Invoice, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.finalized = append(b.finalized, id)
	return invoices.Invoice{StripeID: id, Status: "open"}, b.err
}
func (b *fakeBackend) FinalizeAndSend(_ context.Context, id string) (invoices.Invoice, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.finalizedAndSent = append(b.finalizedAndSent, id)
	return invoices.Invoice{StripeID: id, Status: "open"}, b.err
}
func (b *fakeBackend) Void(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.voided = append(b.voided, id)
	return b.err
}
func (b *fakeBackend) MarkUncollectible(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.uncollectible = append(b.uncollectible, id)
	return b.err
}
func (b *fakeBackend) IssueCreditNote(_ context.Context, in invoices.CreditNoteInput) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.creditNotes = append(b.creditNotes, in)
	return b.err
}
func (b *fakeBackend) ListOverdue(_ context.Context) ([]invoices.Invoice, error) {
	return b.listOverdueResult, b.err
}
func (b *fakeBackend) ListByCustomer(_ context.Context, _ invoices.StripeCustomerID, _ string, _ int) ([]invoices.Invoice, error) {
	return b.listForCustomerResult, b.err
}
func (b *fakeBackend) ListByCustomerPage(_ context.Context, _ invoices.InvoicePageRequest) (invoices.InvoicePage, error) {
	return invoices.InvoicePage{Invoices: b.listForCustomerResult}, b.err
}

func TestCreateDraft_HappyPath(t *testing.T) {
	be := &fakeBackend{}
	ops := invoices.New(be)
	_, err := ops.CreateDraft(t.Context(), invoices.CreateInput{
		Customer:  "cus_x",
		LineItems: []invoices.CreateLineItem{{Description: "Setup", Amount: 1000, Currency: "eur"}},
	})
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if len(be.createdDrafts) != 1 {
		t.Errorf("expected 1 draft, got %d", len(be.createdDrafts))
	}
}

func TestCreateDraft_RequiresCustomerAndLines(t *testing.T) {
	ops := invoices.New(&fakeBackend{})
	if _, err := ops.CreateDraft(t.Context(), invoices.CreateInput{LineItems: []invoices.CreateLineItem{{Amount: 1, Currency: "eur"}}}); err == nil {
		t.Error("expected error for missing customer")
	}
	if _, err := ops.CreateDraft(t.Context(), invoices.CreateInput{Customer: "cus_x"}); err == nil {
		t.Error("expected error for missing line items")
	}
}

func TestCreateDraft_LineItemValidation(t *testing.T) {
	ops := invoices.New(&fakeBackend{})
	if _, err := ops.CreateDraft(t.Context(), invoices.CreateInput{
		Customer: "cus_x", LineItems: []invoices.CreateLineItem{{Amount: 0, Currency: "eur"}},
	}); err == nil {
		t.Error("expected error for zero amount")
	}
	if _, err := ops.CreateDraft(t.Context(), invoices.CreateInput{
		Customer: "cus_x", LineItems: []invoices.CreateLineItem{{Amount: 100, Currency: ""}},
	}); err == nil {
		t.Error("expected error for missing currency")
	}
}

func TestFinalizeAndVoid(t *testing.T) {
	be := &fakeBackend{}
	ops := invoices.New(be)
	if _, err := ops.Finalize(t.Context(), "in_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := ops.FinalizeAndSend(t.Context(), "in_2"); err != nil {
		t.Fatal(err)
	}
	if err := ops.Void(t.Context(), "in_3"); err != nil {
		t.Fatal(err)
	}
	if err := ops.MarkUncollectible(t.Context(), "in_4"); err != nil {
		t.Fatal(err)
	}
	if len(be.finalized) != 1 || len(be.finalizedAndSent) != 1 || len(be.voided) != 1 || len(be.uncollectible) != 1 {
		t.Errorf("call tracking wrong: %+v", be)
	}
}

func TestIssueCreditNote(t *testing.T) {
	be := &fakeBackend{}
	ops := invoices.New(be)
	in := invoices.CreditNoteInput{
		InvoiceID: "in_x",
		Lines:     []invoices.CreateLineItem{{Description: "Refund", Amount: 500, Currency: "eur"}},
		Reason:    "duplicate", Refund: true,
	}
	if err := ops.IssueCreditNote(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if len(be.creditNotes) != 1 || !be.creditNotes[0].Refund {
		t.Errorf("credit note wrong: %+v", be.creditNotes)
	}
}

func TestIssueCreditNote_Validation(t *testing.T) {
	ops := invoices.New(&fakeBackend{})
	if err := ops.IssueCreditNote(t.Context(), invoices.CreditNoteInput{}); err == nil {
		t.Error("expected error for missing InvoiceID")
	}
	if err := ops.IssueCreditNote(t.Context(), invoices.CreditNoteInput{InvoiceID: "in_x"}); err == nil {
		t.Error("expected error for empty Lines")
	}
}

func TestListOverdue(t *testing.T) {
	be := &fakeBackend{listOverdueResult: []invoices.Invoice{{StripeID: "in_overdue"}}}
	ops := invoices.New(be)
	list, err := ops.ListOverdue(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Errorf("expected 1, got %d", len(list))
	}
}

func TestEmptyIDsRejected(t *testing.T) {
	ops := invoices.New(&fakeBackend{})
	if _, err := ops.Finalize(t.Context(), ""); err == nil {
		t.Error("Finalize empty id")
	}
	if _, err := ops.FinalizeAndSend(t.Context(), ""); err == nil {
		t.Error("FinalizeAndSend empty id")
	}
	if err := ops.Void(t.Context(), ""); err == nil {
		t.Error("Void empty id")
	}
	if err := ops.MarkUncollectible(t.Context(), ""); err == nil {
		t.Error("MarkUncollectible empty id")
	}
}

func TestCreateDraft_WithNumberRepo_StampsAndMarksUsed(t *testing.T) {
	be := &fakeBackend{}
	repo := invoices.NewMemoryNumberRepo()
	ops := invoices.New(be, invoices.WithNumberRepo(repo))

	inv, err := ops.CreateDraft(t.Context(), invoices.CreateInput{
		Customer:     "cus_x",
		LineItems:    []invoices.CreateLineItem{{Amount: 1000, Currency: "eur"}},
		NumberPrefix: "AT-2026-",
	})
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if len(be.createdDrafts) != 1 {
		t.Fatalf("expected 1 backend call")
	}
	if be.createdDrafts[0].NumberOverride != "AT-2026-000001" {
		t.Errorf("number not stamped, got %q", be.createdDrafts[0].NumberOverride)
	}
	// The same MarkUsed should be a no-op when re-applied.
	if err := repo.MarkUsed(t.Context(), "AT-2026-000001", inv.StripeID); err != nil {
		t.Errorf("MarkUsed should be idempotent after CreateDraft, got %v", err)
	}
}

func TestCreateDraft_WithNumberRepo_BackendErrorVoidsNumber(t *testing.T) {
	be := &fakeBackend{err: errVoid}
	repo := invoices.NewMemoryNumberRepo()
	ops := invoices.New(be, invoices.WithNumberRepo(repo))

	_, err := ops.CreateDraft(t.Context(), invoices.CreateInput{
		Customer:     "cus_x",
		LineItems:    []invoices.CreateLineItem{{Amount: 1000, Currency: "eur"}},
		NumberPrefix: "AT-2026-",
	})
	if err == nil {
		t.Fatal("expected backend error")
	}
	// The number must be voided, not reused.
	next, _ := repo.Next(t.Context(), "AT-2026-")
	if next != "AT-2026-000002" {
		t.Errorf("expected next sequence after void to be 000002, got %q", next)
	}
}

func TestCreateDraft_NumberOverridePreemptsRepo(t *testing.T) {
	be := &fakeBackend{}
	repo := invoices.NewMemoryNumberRepo()
	ops := invoices.New(be, invoices.WithNumberRepo(repo))

	_, err := ops.CreateDraft(t.Context(), invoices.CreateInput{
		Customer:       "cus_x",
		LineItems:      []invoices.CreateLineItem{{Amount: 1000, Currency: "eur"}},
		NumberOverride: "MANUAL-001",
	})
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if be.createdDrafts[0].NumberOverride != "MANUAL-001" {
		t.Errorf("expected manual override preserved, got %q", be.createdDrafts[0].NumberOverride)
	}
	// Repo's first allocation should still be #1 — wasn't consumed.
	n, _ := repo.Next(t.Context(), "AT-2026-")
	if n != "AT-2026-000001" {
		t.Errorf("repo consumed despite override: %q", n)
	}
}

var errVoid = stripeErr("simulated stripe failure")

type stripeErr string

func (e stripeErr) Error() string { return string(e) }

// --- Slice 41 follow-up: orphan-hook coverage for silent-failure paths ---

// orphanTrackingNumberRepo lets tests force MarkUsed / MarkVoided to
// fail, simulating Postgres outages between Stripe call and ledger
// update.
type orphanTrackingNumberRepo struct {
	*invoices.MemoryNumberRepo
	failMarkUsed   error
	failMarkVoided error
}

func (r *orphanTrackingNumberRepo) MarkUsed(ctx context.Context, num, id string) error {
	if r.failMarkUsed != nil {
		return r.failMarkUsed
	}
	return r.MemoryNumberRepo.MarkUsed(ctx, num, id)
}
func (r *orphanTrackingNumberRepo) MarkVoided(ctx context.Context, num, reason string) error {
	if r.failMarkVoided != nil {
		return r.failMarkVoided
	}
	return r.MemoryNumberRepo.MarkVoided(ctx, num, reason)
}

func TestCreateDraft_OrphanHook_FiresWhenMarkVoidedFailsAfterStripeError(t *testing.T) {
	be := &fakeBackend{err: errVoid}
	repo := &orphanTrackingNumberRepo{
		MemoryNumberRepo: invoices.NewMemoryNumberRepo(),
		failMarkVoided:   errors.New("postgres connection refused"),
	}
	var orphan invoices.OrphanInfo
	var hookCalls int
	ops := invoices.New(be,
		invoices.WithNumberRepo(repo),
		invoices.WithOrphanHook(func(_ context.Context, info invoices.OrphanInfo) error {
			hookCalls++
			orphan = info
			return nil
		}),
	)

	_, err := ops.CreateDraft(t.Context(), invoices.CreateInput{
		Customer:     "cus_x",
		LineItems:    []invoices.CreateLineItem{{Amount: 1000, Currency: "eur"}},
		NumberPrefix: "AT-2026-",
	})
	if err == nil {
		t.Fatal("expected Stripe error to propagate")
	}
	if hookCalls != 1 {
		t.Fatalf("OrphanHook calls = %d, want 1", hookCalls)
	}
	if orphan.Reason != "void_failed_after_stripe_error" {
		t.Errorf("Reason = %q", orphan.Reason)
	}
	if orphan.Number != "AT-2026-000001" {
		t.Errorf("Number = %q", orphan.Number)
	}
	if orphan.LedgerError == nil || orphan.StripeError == nil {
		t.Errorf("expected both LedgerError and StripeError set, got %+v", orphan)
	}
}

func TestCreateDraft_OrphanHook_FiresWhenMarkUsedFailsAfterStripeSuccess(t *testing.T) {
	be := &fakeBackend{}
	repo := &orphanTrackingNumberRepo{
		MemoryNumberRepo: invoices.NewMemoryNumberRepo(),
		failMarkUsed:     errors.New("postgres connection refused"),
	}
	var orphan invoices.OrphanInfo
	var hookCalls int
	ops := invoices.New(be,
		invoices.WithNumberRepo(repo),
		invoices.WithOrphanHook(func(_ context.Context, info invoices.OrphanInfo) error {
			hookCalls++
			orphan = info
			return nil
		}),
	)

	inv, err := ops.CreateDraft(t.Context(), invoices.CreateInput{
		Customer:     "cus_x",
		LineItems:    []invoices.CreateLineItem{{Amount: 1000, Currency: "eur"}},
		NumberPrefix: "AT-2026-",
	})
	if err == nil {
		t.Fatal("expected MarkUsed failure to surface as error")
	}
	if hookCalls != 1 {
		t.Fatalf("OrphanHook calls = %d, want 1", hookCalls)
	}
	if orphan.Reason != "used_failed_after_stripe_success" {
		t.Errorf("Reason = %q", orphan.Reason)
	}
	if orphan.Number != "AT-2026-000001" {
		t.Errorf("Number = %q", orphan.Number)
	}
	if orphan.StripeInvoiceID != inv.StripeID {
		t.Errorf("StripeInvoiceID = %q, want %q (Stripe id from successful create)", orphan.StripeInvoiceID, inv.StripeID)
	}
	if orphan.LedgerError == nil {
		t.Error("LedgerError missing")
	}
}

func TestCreateDraft_OrphanHookFailure_LogsToStderr(t *testing.T) {
	var buf bytes.Buffer
	prev := invoices.GetStderrSink()
	invoices.SetStderrSink(&buf)
	defer invoices.SetStderrSink(prev)

	be := &fakeBackend{}
	repo := &orphanTrackingNumberRepo{
		MemoryNumberRepo: invoices.NewMemoryNumberRepo(),
		failMarkUsed:     errors.New("ledger down"),
	}
	ops := invoices.New(be,
		invoices.WithNumberRepo(repo),
		invoices.WithOrphanHook(func(_ context.Context, _ invoices.OrphanInfo) error {
			return errors.New("alert dispatcher offline")
		}),
	)
	_, _ = ops.CreateDraft(t.Context(), invoices.CreateInput{
		Customer:     "cus_x",
		LineItems:    []invoices.CreateLineItem{{Amount: 1000, Currency: "eur"}},
		NumberPrefix: "AT-2026-",
	})
	if !strings.Contains(buf.String(), "OrphanHook failed") {
		t.Errorf("expected stderr to log OrphanHook failure; got %q", buf.String())
	}
}
