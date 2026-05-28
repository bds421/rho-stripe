package invoices

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// NumberRepo is the optional invoice-number issuer for apps that need
// gapless sequential numbering (Austrian §11 UStG and similar
// jurisdictions where the tax authority requires reasoning about
// every number in the sequence).
//
// Designed in ADR-0009: when an app provides a NumberRepo, the
// connector calls Next before creating each draft invoice and stamps
// the returned string into CreateInput.NumberOverride. On Stripe
// success the app calls MarkUsed; on failure (network error, Stripe
// rejection) the app calls MarkVoided so the number is recorded as
// intentionally skipped, preserving "every number is accounted for."
//
// Per-prefix sequences are supported (e.g. "AT-2026-" for invoices
// issued by the Austrian entity in 2026): Next takes a prefix and
// returns the next value within that scope.
type NumberRepo interface {
	// Next returns the next number for the given prefix scope. The
	// returned string is fully formed (prefix + counter) and ready to
	// stamp on the invoice. Implementations MUST persist the issuance
	// before returning; a returned number that wasn't persisted would
	// be reused on a retry and break the sequence.
	Next(ctx context.Context, prefix string) (string, error)

	// MarkUsed records that the number was successfully attached to a
	// Stripe invoice. The stripeInvoiceID is stored so audits can map
	// number → Stripe object. Idempotent: re-calling with the same
	// (number, stripeInvoiceID) is a no-op.
	MarkUsed(ctx context.Context, number string, stripeInvoiceID string) error

	// MarkVoided records that the number was issued but not used
	// (e.g. the draft was never finalized, or Stripe rejected it).
	// The reason is stored for the audit trail. Idempotent.
	MarkVoided(ctx context.Context, number string, reason string) error
}

// ErrNumberNotIssued is returned by MarkUsed / MarkVoided when the
// number was never persisted by Next. Callers should treat this as
// a programming error: it means the app called MarkUsed without
// first asking the repo for a number, or the repo lost durability.
var ErrNumberNotIssued = errors.New("invoices: number was never issued by Next")

// ErrNumberAlreadyResolved is returned when MarkUsed / MarkVoided is
// called on a number whose status was already terminal (used OR voided)
// AND the new resolution disagrees with the recorded one. Same-status
// repeats are no-ops; cross-status repeats are an integrity violation.
var ErrNumberAlreadyResolved = errors.New("invoices: number already resolved to a different terminal status")

// OrphanReporter is an optional extension Postgres-backed NumberRepos
// implement so apps can periodically scan for numbers stuck in
// "issued" state past a deadline — indicating either MarkVoided failed
// (orphan path) or the process crashed between Next and the Stripe call.
//
// Apps drive this from a cron / scheduled job: pick a deadline like
// 5 minutes ago, list orphans, decide per-orphan whether to MarkVoided
// (Stripe never saw the number) or MarkUsed (Stripe-side reconciliation
// found the missing mapping).
type OrphanReporter interface {
	NumberRepo
	// ListIssuedOlderThan returns numbers still in "issued" state whose
	// issuance time is older than the deadline.
	ListIssuedOlderThan(ctx context.Context, deadline time.Time) ([]IssuedNumber, error)
}

// IssuedNumber is the shape ListIssuedOlderThan returns.
type IssuedNumber struct {
	Number   string
	Prefix   string
	Counter  int64
	IssuedAt time.Time
}

// MemoryNumberRepo is an in-memory NumberRepo for tests and demos.
// Production apps wire a Postgres-backed implementation (see
// repos/postgres.NewInvoiceNumberRepo).
type MemoryNumberRepo struct {
	mu     sync.Mutex
	seq    map[string]int64    // prefix -> last issued counter
	status map[string]nrStatus // full number -> status
}

type nrStatus struct {
	State    string // "issued" | "used" | "voided"
	StripeID string
	Reason   string
}

// NewMemoryNumberRepo returns an empty in-memory NumberRepo.
func NewMemoryNumberRepo() *MemoryNumberRepo {
	return &MemoryNumberRepo{
		seq:    make(map[string]int64),
		status: make(map[string]nrStatus),
	}
}

func (r *MemoryNumberRepo) Next(_ context.Context, prefix string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq[prefix]++
	number := fmt.Sprintf("%s%06d", prefix, r.seq[prefix])
	r.status[number] = nrStatus{State: "issued"}
	return number, nil
}

func (r *MemoryNumberRepo) MarkUsed(_ context.Context, number, stripeID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.status[number]
	if !ok {
		return ErrNumberNotIssued
	}
	switch cur.State {
	case "issued":
		r.status[number] = nrStatus{State: "used", StripeID: stripeID}
		return nil
	case "used":
		if cur.StripeID == stripeID {
			return nil
		}
		return ErrNumberAlreadyResolved
	default:
		return ErrNumberAlreadyResolved
	}
}

func (r *MemoryNumberRepo) MarkVoided(_ context.Context, number, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.status[number]
	if !ok {
		return ErrNumberNotIssued
	}
	switch cur.State {
	case "issued":
		r.status[number] = nrStatus{State: "voided", Reason: reason}
		return nil
	case "voided":
		return nil
	default:
		return ErrNumberAlreadyResolved
	}
}
