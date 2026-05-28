package credits

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sort"
	"sync"
	"time"
)

// CreditRepo persists the credit ledger: grants + deductions + the
// idempotent FIFO-by-expiry deduction logic. Phase 3.
type CreditRepo interface {
	// Grant creates a new ledger entry. Implementations must enforce
	// idempotency on (Subject, Bucket, SourceRef) when SourceRef is
	// non-empty — re-deliveries of the same webhook event must not
	// create duplicate grants.
	Grant(ctx context.Context, in GrantInput) (*Grant, error)

	// ListBySubject returns all grants for a subject (across buckets).
	ListBySubject(ctx context.Context, subject SubjectID) ([]Grant, error)

	// TryDeduct attempts to deduct Amount from the subject's bucket,
	// drawing from grants oldest-to-expire-first (FIFO). Returns
	// (true, balance, nil) on success, (false, balance, nil) on
	// insufficient credit, error on system failure. Idempotent on
	// (Subject, RequestID): repeat calls return the original result.
	TryDeduct(ctx context.Context, in DeductInput) (ok bool, balance Balance, err error)

	// Balance returns the current remaining total for a subject/bucket.
	Balance(ctx context.Context, subject SubjectID, bucket string) (Balance, error)

	// AllBalances returns balances across every bucket the subject has.
	AllBalances(ctx context.Context, subject SubjectID) (map[string]Balance, error)

	// RunExpiry marks grants past their expiry as zero-remaining.
	// Returns (numGrantsExpired, numCreditsExpired, err). Apps
	// schedule this (e.g. hourly via cron).
	RunExpiry(ctx context.Context) (int, int64, error)

	// History returns a unified time-sorted stream of grants and
	// deductions for the subject since `since`.
	History(ctx context.Context, subject SubjectID, since time.Time) ([]LedgerEntry, error)

	// RevokeGrant soft-deletes a grant: zeroes its remaining, records
	// the reason in metadata, but preserves the row for audit. Past
	// deductions referencing the grant are unaffected.
	RevokeGrant(ctx context.Context, grantID, reason string) error

	// FindGrantsBySourceRef returns grants whose SourceRef matches
	// (typically: every grant created from a specific Stripe
	// charge/payment_intent/invoice id). Used by the refund-auto-
	// revoke flow to find grants tied to a refunded charge.
	//
	// Implementations should match on exact equality (no globbing).
	// Returns an empty slice when nothing matches.
	FindGrantsBySourceRef(ctx context.Context, sourceRef string) ([]Grant, error)
}

// NewMemoryRepo returns an in-memory CreditRepo suitable for tests
// and demos. Thread-safe for concurrent goroutines; not persistent
// across process restarts.
//
// The returned repo uses time.Now() for all timestamps. For
// deterministic time-dependent tests (expiry, grant timestamps),
// use NewMemoryRepoWithClock.
func NewMemoryRepo() CreditRepo {
	return NewMemoryRepoWithClock(nil)
}

// NewMemoryRepoWithClock is like NewMemoryRepo but with an injectable
// clock. Passing nil uses the real wall clock; pass a *clock.Fake
// to drive expiry / grant-time logic deterministically in tests.
func NewMemoryRepoWithClock(clk clockNow) CreditRepo {
	if clk == nil {
		clk = realClockNow{}
	}
	return &memoryRepo{
		bySubject:          map[SubjectID][]Grant{},
		deductionBySubject: map[SubjectID][]Deduction{},
		seenGrant:          map[string]string{},
		seenDeduction:      map[string]string{}, // (subject|request_id) → deduction id
		clock:              clk,
	}
}

// clockNow is a tiny subset of clock.Clock used to avoid importing the
// full clock package here (which would create a cycle through the
// Postgres repo). Apps wire it via NewMemoryRepoWithClock.
type clockNow interface {
	Now() time.Time
}

type realClockNow struct{}

func (realClockNow) Now() time.Time { return time.Now() }

type memoryRepo struct {
	mu                 sync.RWMutex // RWMutex so read-only ops (Balance, AllBalances, History, ListBySubject, FindGrantsBySourceRef) can run in parallel
	bySubject          map[SubjectID][]Grant
	deductionBySubject map[SubjectID][]Deduction
	seenGrant          map[string]string
	seenDeduction      map[string]string
	clock              clockNow
}

func (r *memoryRepo) Grant(_ context.Context, in GrantInput) (*Grant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if dedupKey := grantDedupKey(in); dedupKey != "" {
		if id, ok := r.seenGrant[dedupKey]; ok {
			for _, g := range r.bySubject[in.SubjectID] {
				if g.ID == id {
					return &g, nil
				}
			}
		}
	}

	now := r.clock.Now()
	g := Grant{
		ID:              newGrantID(),
		SubjectID:         in.SubjectID,
		Bucket:          in.Bucket,
		AmountInitial:   in.Amount,
		AmountRemaining: in.Amount,
		GrantedAt:       now,
		Source:          in.Source,
		SourceRef:       in.SourceRef,
		Metadata:        in.Metadata,
	}
	if in.ValidDays > 0 {
		exp := now.AddDate(0, 0, in.ValidDays)
		g.ExpiresAt = &exp
	}
	r.bySubject[in.SubjectID] = append(r.bySubject[in.SubjectID], g)
	if dedupKey := grantDedupKey(in); dedupKey != "" {
		r.seenGrant[dedupKey] = g.ID
	}
	return &g, nil
}

func (r *memoryRepo) TryDeduct(_ context.Context, in DeductInput) (bool, Balance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if in.RequestID != "" {
		dedupKey := string(in.SubjectID) + "|" + in.RequestID
		if _, ok := r.seenDeduction[dedupKey]; ok {
			return true, r.balanceLocked(in.SubjectID, in.Bucket), nil
		}
	}

	// FIFO-by-expiry: sort grants oldest-expires first; NULL expiry last.
	grants := r.bySubject[in.SubjectID]
	now := r.clock.Now()
	type idx struct {
		i        int
		expires  time.Time
		neverExp bool
	}
	var pool []idx
	for i, g := range grants {
		if g.AmountRemaining <= 0 {
			continue
		}
		if g.ExpiresAt != nil && g.ExpiresAt.Before(now) {
			continue
		}
		if g.Bucket != in.Bucket {
			continue
		}
		entry := idx{i: i}
		if g.ExpiresAt == nil {
			entry.neverExp = true
		} else {
			entry.expires = *g.ExpiresAt
		}
		pool = append(pool, entry)
	}
	sort.Slice(pool, func(i, j int) bool {
		// non-expiring grants drained last
		if pool[i].neverExp != pool[j].neverExp {
			return !pool[i].neverExp
		}
		if pool[i].expires.Equal(pool[j].expires) {
			return grants[pool[i].i].GrantedAt.Before(grants[pool[j].i].GrantedAt)
		}
		return pool[i].expires.Before(pool[j].expires)
	})

	// Plan deductions
	remaining := in.Amount
	type plan struct {
		grantIdx int
		take     int64
	}
	var planSteps []plan
	for _, p := range pool {
		if remaining == 0 {
			break
		}
		take := grants[p.i].AmountRemaining
		if take > remaining {
			take = remaining
		}
		planSteps = append(planSteps, plan{p.i, take})
		remaining -= take
	}
	if remaining > 0 {
		return false, r.balanceLocked(in.SubjectID, in.Bucket), nil
	}

	// Apply
	for _, step := range planSteps {
		grants[step.grantIdx].AmountRemaining -= step.take
		d := Deduction{
			ID:        newDeductionID(),
			SubjectID:   in.SubjectID,
			Bucket:    in.Bucket,
			GrantID:   grants[step.grantIdx].ID,
			Amount:    step.take,
			Reason:    in.Reason,
			RequestID: in.RequestID,
			CreatedAt: now,
			Metadata:  in.Metadata,
		}
		r.deductionBySubject[in.SubjectID] = append(r.deductionBySubject[in.SubjectID], d)
	}
	r.bySubject[in.SubjectID] = grants
	if in.RequestID != "" {
		// Value isn't read; we just need to know we processed this id.
		r.seenDeduction[string(in.SubjectID)+"|"+in.RequestID] = "ok"
	}
	return true, r.balanceLocked(in.SubjectID, in.Bucket), nil
}

func (r *memoryRepo) Balance(_ context.Context, subject SubjectID, bucket string) (Balance, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.balanceLocked(subject, bucket), nil
}

func (r *memoryRepo) AllBalances(_ context.Context, subject SubjectID) (map[string]Balance, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	buckets := map[string]struct{}{}
	for _, g := range r.bySubject[subject] {
		buckets[g.Bucket] = struct{}{}
	}
	out := make(map[string]Balance, len(buckets))
	for b := range buckets {
		out[b] = r.balanceLocked(subject, b)
	}
	return out, nil
}

func (r *memoryRepo) RunExpiry(_ context.Context) (int, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock.Now()
	var grants, credits int64
	for subj, gs := range r.bySubject {
		for i := range gs {
			if gs[i].ExpiresAt != nil && gs[i].ExpiresAt.Before(now) && gs[i].AmountRemaining > 0 {
				credits += gs[i].AmountRemaining
				gs[i].AmountRemaining = 0
				grants++
			}
		}
		r.bySubject[subj] = gs
	}
	return int(grants), credits, nil
}

func (r *memoryRepo) History(_ context.Context, subject SubjectID, since time.Time) ([]LedgerEntry, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []LedgerEntry
	for _, g := range r.bySubject[subject] {
		if g.GrantedAt.Before(since) {
			continue
		}
		out = append(out, LedgerEntry{
			Type: EntryTypeGrant, Timestamp: g.GrantedAt, Bucket: g.Bucket,
			Amount: g.AmountInitial, Source: g.Source, SourceRef: g.SourceRef, GrantID: g.ID,
		})
	}
	for _, d := range r.deductionBySubject[subject] {
		if d.CreatedAt.Before(since) {
			continue
		}
		out = append(out, LedgerEntry{
			Type: EntryTypeDeduction, Timestamp: d.CreatedAt, Bucket: d.Bucket,
			Amount: d.Amount, Reason: d.Reason, GrantID: d.GrantID,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	return out, nil
}

func (r *memoryRepo) RevokeGrant(_ context.Context, grantID, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for subj, gs := range r.bySubject {
		for i := range gs {
			if gs[i].ID == grantID {
				gs[i].AmountRemaining = 0
				if gs[i].Metadata == nil {
					gs[i].Metadata = map[string]string{}
				}
				gs[i].Metadata["revoked_reason"] = reason
				gs[i].Metadata["revoked_at"] = r.clock.Now().UTC().Format(time.RFC3339)
				r.bySubject[subj] = gs
				return nil
			}
		}
	}
	return nil // not found = no-op (don't error; caller may have stale id)
}

func (r *memoryRepo) FindGrantsBySourceRef(_ context.Context, sourceRef string) ([]Grant, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if sourceRef == "" {
		return nil, nil
	}
	var out []Grant
	for _, gs := range r.bySubject {
		for _, g := range gs {
			if g.SourceRef == sourceRef {
				out = append(out, g)
			}
		}
	}
	return out, nil
}

func (r *memoryRepo) balanceLocked(subject SubjectID, bucket string) Balance {
	bal := Balance{SubjectID: subject, Bucket: bucket}
	now := r.clock.Now()
	for _, g := range r.bySubject[subject] {
		if g.Bucket != bucket || g.AmountRemaining <= 0 {
			continue
		}
		if g.ExpiresAt != nil && g.ExpiresAt.Before(now) {
			continue
		}
		bal.Total += g.AmountRemaining
		bal.GrantCount++
		if g.ExpiresAt != nil {
			if bal.NextExpiry == nil || g.ExpiresAt.Before(*bal.NextExpiry) {
				t := *g.ExpiresAt
				bal.NextExpiry = &t
			}
		}
	}
	return bal
}

func newDeductionID() string {
	return "de_" + randomHex12()
}

func (r *memoryRepo) ListBySubject(_ context.Context, subject SubjectID) ([]Grant, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Grant, len(r.bySubject[subject]))
	copy(out, r.bySubject[subject])
	return out, nil
}

func grantDedupKey(in GrantInput) string {
	if in.SourceRef == "" {
		return ""
	}
	return string(in.SubjectID) + "|" + in.Bucket + "|" + in.SourceRef
}

func newGrantID() string {
	return "gr_" + randomHex12()
}

// randomHex12 returns 24 hex chars of crypto-random data.
//
// crypto/rand.Read returns an error only when the OS RNG is broken
// (extremely rare — both Linux getrandom and macOS arc4random
// guarantee success modulo kernel bugs). We panic instead of
// returning "all zeros" (which would silently produce colliding
// grant/deduction IDs and corrupt the ledger).
func randomHex12() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("credits: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
