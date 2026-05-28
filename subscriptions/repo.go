package subscriptions

import (
	"context"
	"sync"
)

// SubscriptionRepo persists subscription mirror rows. Apps implement
// this against their own DB; the in-memory impl is for tests and the
// demo CLI.
//
// The Upsert contract honors event-watermark semantics: implementations
// must reject writes whose StripeUpdatedAt is older than the stored
// row's. The memory impl below does this; the Postgres impl uses
// `WHERE stripe_updated_at < $newValue` on the UPDATE.
type SubscriptionRepo interface {
	// Upsert writes the row if it's new, OR replaces it if the
	// incoming StripeUpdatedAt is >= the stored value. Out-of-order
	// stale events are silently skipped (no error).
	Upsert(ctx context.Context, s *Subscription) error

	// GetByStripeID returns the mirror row for a Stripe subscription
	// id, or (nil, false, nil) if no mirror exists yet.
	GetByStripeID(ctx context.Context, stripeSubID string) (*Subscription, bool, error)

	// ListBySubject returns every subscription a subject owns (active
	// or terminal). Apps filter by status as needed.
	ListBySubject(ctx context.Context, subject SubjectID) ([]*Subscription, error)
}

// NewMemoryRepo returns an in-memory SubscriptionRepo.
func NewMemoryRepo() SubscriptionRepo {
	return &memoryRepo{
		byStripeID: map[string]*Subscription{},
	}
}

type memoryRepo struct {
	mu         sync.RWMutex
	byStripeID map[string]*Subscription
}

func (r *memoryRepo) Upsert(_ context.Context, s *Subscription) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.byStripeID[s.StripeID]; ok {
		if existing.StripeUpdatedAt.After(s.StripeUpdatedAt) {
			// Stale event — keep the newer state.
			return nil
		}
	}
	// Defensive copy so callers' subsequent mutations don't leak in.
	copy := *s
	r.byStripeID[s.StripeID] = &copy
	return nil
}

func (r *memoryRepo) GetByStripeID(_ context.Context, id string) (*Subscription, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.byStripeID[id]
	if !ok {
		return nil, false, nil
	}
	copy := *s
	return &copy, true, nil
}

func (r *memoryRepo) ListBySubject(_ context.Context, subject SubjectID) ([]*Subscription, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*Subscription
	for _, s := range r.byStripeID {
		if s.SubjectID == subject {
			copy := *s
			out = append(out, &copy)
		}
	}
	return out, nil
}
