package credits

import (
	"context"
	"errors"
	"time"
)

// Operations is the public surface apps use to interact with the
// credit ledger. The connector facade constructs one when
// Config.Credits is set.
type Operations struct {
	repo CreditRepo
}

// New wraps a CreditRepo in an Operations facade.
func New(repo CreditRepo) *Operations {
	if repo == nil {
		panic("credits.New: repo is required")
	}
	return &Operations{repo: repo}
}

// Grant adds credits to the ledger. Usually called by the lib's own
// auto-handler on checkout.session.completed for catalog-declared
// CreditGrant products; apps call directly for admin grants.
func (o *Operations) Grant(ctx context.Context, in GrantInput) (*Grant, error) {
	if in.Amount <= 0 {
		return nil, errors.New("credits: GrantInput.Amount must be positive")
	}
	return o.repo.Grant(ctx, in)
}

// TryDeduct attempts to deduct credits FIFO-by-expiry. Returns
// (true, balance, nil) on success, (false, balance, nil) on
// insufficient credit (app decides what to do — paywall, prompt to
// buy more, fall back to metered), error on system failure.
func (o *Operations) TryDeduct(ctx context.Context, in DeductInput) (bool, Balance, error) {
	if in.Amount <= 0 {
		return false, Balance{}, errors.New("credits: DeductInput.Amount must be positive")
	}
	if in.RequestID == "" {
		return false, Balance{}, errors.New("credits: DeductInput.RequestID is required for idempotency")
	}
	return o.repo.TryDeduct(ctx, in)
}

// Deduct is the always-succeed-or-error variant. Returns an error on
// insufficient credit (so callers that can't gracefully handle the
// false return value get a typed failure).
func (o *Operations) Deduct(ctx context.Context, in DeductInput) (Balance, error) {
	ok, bal, err := o.TryDeduct(ctx, in)
	if err != nil {
		return bal, err
	}
	if !ok {
		return bal, ErrInsufficientCredit
	}
	return bal, nil
}

// ErrInsufficientCredit is returned by Deduct when the subject doesn't
// have enough credit. Apps that want to discriminate from system
// failures use errors.Is(err, credits.ErrInsufficientCredit).
var ErrInsufficientCredit = errors.New("credits: insufficient credit")

// Balance returns the subject's current remaining total in the bucket.
func (o *Operations) Balance(ctx context.Context, subject SubjectID, bucket string) (Balance, error) {
	return o.repo.Balance(ctx, subject, bucket)
}

// AllBalances returns balances for every bucket the subject has.
func (o *Operations) AllBalances(ctx context.Context, subject SubjectID) (map[string]Balance, error) {
	return o.repo.AllBalances(ctx, subject)
}

// HasAccess is a convenience for time-limited / cohort-based access
// patterns where a single grant grants access for N days. Returns
// true when the subject has ANY remaining credit in the bucket.
//
// Typical pattern: cohort course pays €299 once → CreditGrant of
// 1 credit with ValidDays=180 → app calls HasAccess to gate access
// without doing any deduction (the grant naturally expires after
// the cohort window).
func (o *Operations) HasAccess(ctx context.Context, subject SubjectID, bucket string) (bool, error) {
	bal, err := o.repo.Balance(ctx, subject, bucket)
	if err != nil {
		return false, err
	}
	return bal.Total > 0, nil
}

// RunExpiry marks any past-expiry grants as zero-remaining. Apps
// schedule via cron (typically hourly).
func (o *Operations) RunExpiry(ctx context.Context) (expiredGrants int, expiredCredits int64, err error) {
	return o.repo.RunExpiry(ctx)
}

// History returns the subject's ledger activity since `since`.
func (o *Operations) History(ctx context.Context, subject SubjectID, since time.Time) ([]LedgerEntry, error) {
	return o.repo.History(ctx, subject, since)
}

// RevokeGrant soft-deletes a grant. Past deductions referencing it
// are unaffected; future balance queries exclude it. Use sparingly —
// revocation is a customer-relations decision.
func (o *Operations) RevokeGrant(ctx context.Context, grantID, reason string) error {
	if grantID == "" {
		return errors.New("credits: grantID is required")
	}
	return o.repo.RevokeGrant(ctx, grantID, reason)
}

// Repo exposes the raw repo for advanced direct queries.
func (o *Operations) Repo() CreditRepo { return o.repo }
