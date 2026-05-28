package subject

import (
	"context"
	"sync"
)

// StripeCustomerID is a typed string holding a "cus_..." Stripe Customer id.
// It is distinct from [ID] so functions cannot accept a SubjectID where a
// Stripe id is required (or vice versa) — the compiler catches the swap.
//
// Lives in `subject` rather than `checkout` because multiple packages
// (checkout, customers, invoices, paymentmethods, metering, repos/postgres)
// reach for this type; "the package every other package imports" is the
// definition of `subject`. Other packages alias it locally:
//
//	type StripeCustomerID = subject.StripeCustomerID
type StripeCustomerID string

// String returns the underlying "cus_..." form.
func (s StripeCustomerID) String() string { return string(s) }

// IsZero reports whether the id is unset.
func (s StripeCustomerID) IsZero() bool { return s == "" }

// CustomerRepo persists the SubjectID → StripeCustomerID mapping. The lib
// reuses Stripe Customers across sessions for the same subject (avoids
// duplicate-customer-per-purchase). Apps implement this against their own
// DB; [NewMemoryCustomerRepo] is for tests and demos.
type CustomerRepo interface {
	// Get returns the StripeCustomerID previously stored for subject,
	// or ("", false, nil) if no mapping exists yet.
	Get(ctx context.Context, subject ID) (StripeCustomerID, bool, error)

	// Upsert stores the mapping. Called by checkout after creating
	// a new Customer in Stripe.
	Upsert(ctx context.Context, subject ID, id StripeCustomerID) error
}

// NewMemoryCustomerRepo returns an in-memory CustomerRepo suitable for
// tests and the demo CLI. Not durable across processes; safe for
// concurrent goroutines.
func NewMemoryCustomerRepo() CustomerRepo {
	return &memoryCustomerRepo{m: map[ID]StripeCustomerID{}}
}

type memoryCustomerRepo struct {
	mu sync.RWMutex
	m  map[ID]StripeCustomerID
}

func (r *memoryCustomerRepo) Get(_ context.Context, s ID) (StripeCustomerID, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.m[s]
	return id, ok, nil
}

func (r *memoryCustomerRepo) Upsert(_ context.Context, s ID, id StripeCustomerID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[s] = id
	return nil
}
