package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	pgadvisory "github.com/bds421/rho-kit/data/lock/pgadvisory/v2"
	"github.com/bds421/rho-stripe/credits"
)

// CreditRepo implements credits.CreditRepo using a *sql.DB. Grant
// idempotency is enforced via the partial UNIQUE index on
// (subject_id, bucket, source_ref); deduction idempotency via the
// UNIQUE constraint on (subject_id, request_id). Concurrent
// deductions on the same subject are serialized via rho-kit's
// pgadvisory.Locker.
type CreditRepo struct {
	db     *sql.DB
	locker *pgadvisory.Locker
}

// NewCreditRepo wraps db.
func NewCreditRepo(db *sql.DB) *CreditRepo {
	if db == nil {
		panic("postgres.NewCreditRepo: db is required")
	}
	return &CreditRepo{db: db, locker: pgadvisory.New(db)}
}

var _ credits.CreditRepo = (*CreditRepo)(nil)

// Grant inserts a new ledger row. When SourceRef is non-empty and a
// row with (subject, bucket, source_ref) already exists, returns the
// existing grant instead of creating a duplicate — implements the
// idempotency contract documented on credits.CreditRepo.
func (r *CreditRepo) Grant(ctx context.Context, in credits.GrantInput) (*credits.Grant, error) {
	if in.Amount <= 0 {
		return nil, fmt.Errorf("postgres.CreditRepo.Grant: Amount must be positive (got %d)", in.Amount)
	}
	metadataJSON, err := json.Marshal(orEmpty(in.Metadata))
	if err != nil {
		return nil, fmt.Errorf("postgres.CreditRepo.Grant: marshal metadata: %w", err)
	}

	var expiresAt sql.NullTime
	if in.ValidDays > 0 {
		expiresAt = sql.NullTime{Time: time.Now().AddDate(0, 0, in.ValidDays), Valid: true}
	}
	var sourceRef sql.NullString
	if in.SourceRef != "" {
		sourceRef = sql.NullString{String: in.SourceRef, Valid: true}
	}

	// INSERT ... ON CONFLICT DO NOTHING. The partial unique index only
	// fires when source_ref IS NOT NULL, so admin-grants without a
	// ref still always insert.
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO stripe_connector_credit_grants
		    (subject_id, bucket, amount_initial, amount_remaining,
		     expires_at, source, source_ref, metadata)
		VALUES ($1, $2, $3, $3, $4, $5, $6, $7)
		ON CONFLICT (subject_id, bucket, source_ref)
		    WHERE source_ref IS NOT NULL
		    DO NOTHING
		RETURNING id, granted_at
	`, string(in.SubjectID), in.Bucket, in.Amount, expiresAt, string(in.Source), sourceRef, metadataJSON)

	var id string
	var grantedAt time.Time
	if err := row.Scan(&id, &grantedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Dedup hit. Re-read the existing grant.
			existing, lookupErr := r.findExisting(ctx, in.SubjectID, in.Bucket, in.SourceRef)
			if lookupErr != nil {
				return nil, fmt.Errorf("postgres.CreditRepo.Grant: lookup existing after dedup: %w", lookupErr)
			}
			return existing, nil
		}
		return nil, fmt.Errorf("postgres.CreditRepo.Grant: insert: %w", err)
	}

	g := &credits.Grant{
		ID:              id,
		SubjectID:         in.SubjectID,
		Bucket:          in.Bucket,
		AmountInitial:   in.Amount,
		AmountRemaining: in.Amount,
		GrantedAt:       grantedAt,
		Source:          in.Source,
		SourceRef:       in.SourceRef,
		Metadata:        in.Metadata,
	}
	if expiresAt.Valid {
		t := expiresAt.Time
		g.ExpiresAt = &t
	}
	return g, nil
}

// ListBySubject returns every grant for a subject across buckets.
func (r *CreditRepo) ListBySubject(ctx context.Context, subject credits.SubjectID) ([]credits.Grant, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, bucket, amount_initial, amount_remaining,
		       granted_at, expires_at, source, source_ref, metadata
		FROM stripe_connector_credit_grants
		WHERE subject_id = $1
		ORDER BY granted_at ASC, id ASC
	`, string(subject))
	if err != nil {
		return nil, fmt.Errorf("postgres.CreditRepo.ListBySubject: %w", err)
	}
	defer rows.Close()

	var out []credits.Grant
	for rows.Next() {
		var (
			id, bucket, source string
			amountInitial      int64
			amountRemaining    int64
			grantedAt          time.Time
			expiresAt          sql.NullTime
			sourceRef          sql.NullString
			metadataJSON       []byte
		)
		if err := rows.Scan(&id, &bucket, &amountInitial, &amountRemaining,
			&grantedAt, &expiresAt, &source, &sourceRef, &metadataJSON); err != nil {
			return nil, fmt.Errorf("postgres.CreditRepo.ListBySubject: scan: %w", err)
		}
		g := credits.Grant{
			ID:              id,
			SubjectID:         subject,
			Bucket:          bucket,
			AmountInitial:   amountInitial,
			AmountRemaining: amountRemaining,
			GrantedAt:       grantedAt,
			Source:          credits.Source(source),
			SourceRef:       sourceRef.String,
		}
		if expiresAt.Valid {
			t := expiresAt.Time
			g.ExpiresAt = &t
		}
		if err := json.Unmarshal(metadataJSON, &g.Metadata); err != nil {
			// Malformed metadata is a data-corruption signal — log so it's
			// observable, then surface the grant with nil Metadata (caller
			// gets the row rather than losing the whole list).
			slog.WarnContext(ctx, "postgres.CreditRepo: malformed grant metadata",
				slog.String("grant_id", id),
				slog.String("err", err.Error()),
			)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres.CreditRepo.ListBySubject: rows: %w", err)
	}
	return out, nil
}

func (r *CreditRepo) findExisting(ctx context.Context, subject credits.SubjectID, bucket, sourceRef string) (*credits.Grant, error) {
	var (
		id, source      string
		amountInitial   int64
		amountRemaining int64
		grantedAt       time.Time
		expiresAt       sql.NullTime
		metadataJSON    []byte
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT id, amount_initial, amount_remaining, granted_at,
		       expires_at, source, metadata
		FROM stripe_connector_credit_grants
		WHERE subject_id = $1 AND bucket = $2 AND source_ref = $3
		LIMIT 1
	`, string(subject), bucket, sourceRef).Scan(
		&id, &amountInitial, &amountRemaining, &grantedAt,
		&expiresAt, &source, &metadataJSON,
	)
	if err != nil {
		return nil, err
	}
	g := &credits.Grant{
		ID:              id,
		SubjectID:         subject,
		Bucket:          bucket,
		AmountInitial:   amountInitial,
		AmountRemaining: amountRemaining,
		GrantedAt:       grantedAt,
		Source:          credits.Source(source),
		SourceRef:       sourceRef,
	}
	if expiresAt.Valid {
		t := expiresAt.Time
		g.ExpiresAt = &t
	}
	if err := json.Unmarshal(metadataJSON, &g.Metadata); err != nil {
		slog.WarnContext(ctx, "postgres.CreditRepo: malformed grant metadata",
			slog.String("grant_id", id),
			slog.String("err", err.Error()),
		)
	}
	return g, nil
}

func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// TryDeduct implements FIFO-by-expiry deduction with idempotency on
// (subject, request_id). The flow:
//  1. Open transaction, acquire per-subject advisory lock (serializes
//     concurrent deductions on the same subject).
//  2. Check (subject, request_id) for an existing deduction — return
//     cached result if found.
//  3. SELECT eligible grants FOR UPDATE ordered by expires ASC NULLS
//     LAST, granted_at ASC.
//  4. Walk, plan, apply: UPDATE grants + INSERT deductions.
//  5. Commit; return new balance.
func (r *CreditRepo) TryDeduct(ctx context.Context, in credits.DeductInput) (bool, credits.Balance, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, credits.Balance{}, fmt.Errorf("postgres.CreditRepo.TryDeduct: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck — Commit happens on success path

	if _, err := r.locker.AcquireTx(ctx, tx, "credits:"+string(in.SubjectID)); err != nil {
		return false, credits.Balance{}, fmt.Errorf("postgres.CreditRepo.TryDeduct: acquire lock: %w", err)
	}

	// Idempotency check.
	var existingGrantID string
	err = tx.QueryRowContext(ctx, `
		SELECT grant_id FROM stripe_connector_credit_deductions
		WHERE subject_id = $1 AND request_id = $2 LIMIT 1
	`, string(in.SubjectID), in.RequestID).Scan(&existingGrantID)
	if err == nil {
		// Duplicate. Compute balance and return.
		bal, balErr := balanceFromTx(ctx, tx, in.SubjectID, in.Bucket)
		if balErr != nil {
			return false, bal, balErr
		}
		if cmErr := tx.Commit(); cmErr != nil {
			return false, bal, cmErr
		}
		return true, bal, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return false, credits.Balance{}, fmt.Errorf("postgres.CreditRepo.TryDeduct: idempotency check: %w", err)
	}

	// Eligible grants, oldest-expiring first.
	rows, err := tx.QueryContext(ctx, `
		SELECT id, amount_remaining, expires_at
		FROM stripe_connector_credit_grants
		WHERE subject_id = $1 AND bucket = $2
		  AND amount_remaining > 0
		  AND revoked_at IS NULL
		  AND (expires_at IS NULL OR expires_at > now())
		ORDER BY expires_at NULLS LAST, granted_at ASC
		FOR UPDATE
	`, string(in.SubjectID), in.Bucket)
	if err != nil {
		return false, credits.Balance{}, fmt.Errorf("postgres.CreditRepo.TryDeduct: select grants: %w", err)
	}

	type grantRow struct {
		ID        string
		Remaining int64
		Expires   sql.NullTime
	}
	var pool []grantRow
	scanErr := func() error {
		defer rows.Close()
		for rows.Next() {
			var g grantRow
			if err := rows.Scan(&g.ID, &g.Remaining, &g.Expires); err != nil {
				return err
			}
			pool = append(pool, g)
		}
		return rows.Err()
	}()
	if scanErr != nil {
		return false, credits.Balance{}, scanErr
	}

	type planStep struct {
		grantID string
		take    int64
	}
	var plan []planStep
	remaining := in.Amount
	for _, g := range pool {
		if remaining == 0 {
			break
		}
		take := g.Remaining
		if take > remaining {
			take = remaining
		}
		plan = append(plan, planStep{g.ID, take})
		remaining -= take
	}

	if remaining > 0 {
		bal, balErr := balanceFromTx(ctx, tx, in.SubjectID, in.Bucket)
		if balErr != nil {
			return false, bal, balErr
		}
		if cmErr := tx.Commit(); cmErr != nil {
			return false, bal, cmErr
		}
		return false, bal, nil
	}

	metadataJSON, _ := json.Marshal(orEmptyMap(in.Metadata))
	for _, step := range plan {
		if _, err := tx.ExecContext(ctx, `
			UPDATE stripe_connector_credit_grants
			SET amount_remaining = amount_remaining - $1
			WHERE id = $2
		`, step.take, step.grantID); err != nil {
			return false, credits.Balance{}, fmt.Errorf("postgres.CreditRepo.TryDeduct: decrement grant: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO stripe_connector_credit_deductions
			    (subject_id, bucket, grant_id, amount, reason, request_id, metadata)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, string(in.SubjectID), in.Bucket, step.grantID, step.take, in.Reason, in.RequestID, metadataJSON); err != nil {
			return false, credits.Balance{}, fmt.Errorf("postgres.CreditRepo.TryDeduct: insert deduction: %w", err)
		}
	}

	bal, err := balanceFromTx(ctx, tx, in.SubjectID, in.Bucket)
	if err != nil {
		return false, bal, err
	}
	if err := tx.Commit(); err != nil {
		return false, bal, fmt.Errorf("postgres.CreditRepo.TryDeduct: commit: %w", err)
	}
	return true, bal, nil
}

// Balance returns the subject/bucket remaining total + next-expiry +
// grant count.
func (r *CreditRepo) Balance(ctx context.Context, subject credits.SubjectID, bucket string) (credits.Balance, error) {
	return balanceFromQuerier(ctx, r.db, subject, bucket)
}

// AllBalances groups balances by bucket.
func (r *CreditRepo) AllBalances(ctx context.Context, subject credits.SubjectID) (map[string]credits.Balance, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT bucket,
		       COALESCE(SUM(amount_remaining), 0) AS total,
		       MIN(expires_at) FILTER (WHERE expires_at IS NOT NULL) AS next_expiry,
		       COUNT(*) FILTER (WHERE amount_remaining > 0) AS grant_count
		FROM stripe_connector_credit_grants
		WHERE subject_id = $1
		  AND amount_remaining > 0
		  AND revoked_at IS NULL
		  AND (expires_at IS NULL OR expires_at > now())
		GROUP BY bucket
	`, string(subject))
	if err != nil {
		return nil, fmt.Errorf("postgres.CreditRepo.AllBalances: %w", err)
	}
	defer rows.Close()
	out := map[string]credits.Balance{}
	for rows.Next() {
		var (
			bucket     string
			total      int64
			nextExpiry sql.NullTime
			grantCount int
		)
		if err := rows.Scan(&bucket, &total, &nextExpiry, &grantCount); err != nil {
			return nil, err
		}
		bal := credits.Balance{SubjectID: subject, Bucket: bucket, Total: total, GrantCount: grantCount}
		if nextExpiry.Valid {
			t := nextExpiry.Time
			bal.NextExpiry = &t
		}
		out[bucket] = bal
	}
	return out, rows.Err()
}

// RunExpiry zeros out remaining on grants whose expires_at has passed.
// Two-query approach: SELECT the aggregate (count + sum) BEFORE the
// UPDATE so we can return what was expired. A single-statement
// "UPDATE ... RETURNING" would require a CTE that captures the prior
// amount_remaining, which Postgres doesn't support directly because
// RETURNING returns the new (post-UPDATE) row state.
func (r *CreditRepo) RunExpiry(ctx context.Context) (int, int64, error) {
	// Step 1: capture aggregates BEFORE zeroing.
	var (
		grants  int
		credits int64
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(amount_remaining), 0)
		FROM stripe_connector_credit_grants
		WHERE expires_at IS NOT NULL
		  AND expires_at < now()
		  AND amount_remaining > 0
		  AND revoked_at IS NULL
	`).Scan(&grants, &credits)
	if err != nil {
		return 0, 0, fmt.Errorf("postgres.CreditRepo.RunExpiry: aggregate: %w", err)
	}
	if grants == 0 {
		return 0, 0, nil
	}
	if _, err := r.db.ExecContext(ctx, `
		UPDATE stripe_connector_credit_grants
		SET amount_remaining = 0
		WHERE expires_at IS NOT NULL
		  AND expires_at < now()
		  AND amount_remaining > 0
		  AND revoked_at IS NULL
	`); err != nil {
		return 0, 0, fmt.Errorf("postgres.CreditRepo.RunExpiry: zero: %w", err)
	}
	return grants, credits, nil
}

// History returns a unified time-sorted stream of grants and deductions.
func (r *CreditRepo) History(ctx context.Context, subject credits.SubjectID, since time.Time) ([]credits.LedgerEntry, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT 'grant'::text AS entry_type, granted_at AS ts, bucket, amount_initial AS amount,
		       ''::text AS reason, source, COALESCE(source_ref, '')::text, id::text AS grant_id
		FROM stripe_connector_credit_grants
		WHERE subject_id = $1 AND granted_at >= $2
		UNION ALL
		SELECT 'deduction'::text, created_at, bucket, amount,
		       reason, ''::text, ''::text, grant_id::text
		FROM stripe_connector_credit_deductions
		WHERE subject_id = $1 AND created_at >= $2
		ORDER BY ts ASC
	`, string(subject), since)
	if err != nil {
		return nil, fmt.Errorf("postgres.CreditRepo.History: %w", err)
	}
	defer rows.Close()
	var out []credits.LedgerEntry
	for rows.Next() {
		var (
			entryType, bucket, reason, source, sourceRef, grantID string
			ts                                                    time.Time
			amount                                                int64
		)
		if err := rows.Scan(&entryType, &ts, &bucket, &amount, &reason, &source, &sourceRef, &grantID); err != nil {
			return nil, err
		}
		out = append(out, credits.LedgerEntry{
			Type:      credits.EntryType(entryType),
			Timestamp: ts,
			Bucket:    bucket,
			Amount:    amount,
			Reason:    reason,
			Source:    credits.Source(source),
			SourceRef: sourceRef,
			GrantID:   grantID,
		})
	}
	return out, rows.Err()
}

// RevokeGrant soft-deletes a grant: zeros its remaining + records the reason.
func (r *CreditRepo) RevokeGrant(ctx context.Context, grantID, reason string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE stripe_connector_credit_grants
		SET amount_remaining = 0,
		    revoked_at       = now(),
		    revoked_reason   = $2
		WHERE id = $1::uuid AND revoked_at IS NULL
	`, grantID, reason)
	if err != nil {
		return fmt.Errorf("postgres.CreditRepo.RevokeGrant: %w", err)
	}
	return nil
}

// FindGrantsBySourceRef returns grants whose source_ref equals the
// given value. Used by the refund auto-revoke flow to locate grants
// tied to a refunded Stripe charge / payment_intent.
func (r *CreditRepo) FindGrantsBySourceRef(ctx context.Context, sourceRef string) ([]credits.Grant, error) {
	if sourceRef == "" {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id::text, subject_id, bucket, amount_initial, amount_remaining,
		       source, source_ref, granted_at, expires_at, revoked_at, revoked_reason
		  FROM stripe_connector_credit_grants
		 WHERE source_ref = $1
		 ORDER BY granted_at
	`, sourceRef)
	if err != nil {
		return nil, fmt.Errorf("postgres.CreditRepo.FindGrantsBySourceRef: %w", err)
	}
	defer rows.Close()
	var out []credits.Grant
	for rows.Next() {
		var (
			g             credits.Grant
			expiresAt     sql.NullTime
			revokedAt     sql.NullTime
			revokedReason sql.NullString
		)
		if err := rows.Scan(&g.ID, &g.SubjectID, &g.Bucket, &g.AmountInitial, &g.AmountRemaining,
			&g.Source, &g.SourceRef, &g.GrantedAt, &expiresAt, &revokedAt, &revokedReason); err != nil {
			return nil, fmt.Errorf("postgres.CreditRepo.FindGrantsBySourceRef: scan: %w", err)
		}
		if expiresAt.Valid {
			t := expiresAt.Time
			g.ExpiresAt = &t
		}
		if revokedAt.Valid || revokedReason.Valid {
			g.Metadata = map[string]string{}
			if revokedReason.Valid {
				g.Metadata["revoked_reason"] = revokedReason.String
			}
			if revokedAt.Valid {
				g.Metadata["revoked_at"] = revokedAt.Time.Format(time.RFC3339)
			}
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// --- helpers shared by Balance + the TryDeduct in-tx balance read ---

type rowQuerier interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

func balanceFromQuerier(ctx context.Context, q rowQuerier, subject credits.SubjectID, bucket string) (credits.Balance, error) {
	var (
		total      int64
		nextExpiry sql.NullTime
		grantCount int
	)
	err := q.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount_remaining), 0),
		       MIN(expires_at) FILTER (WHERE expires_at IS NOT NULL),
		       COUNT(*) FILTER (WHERE amount_remaining > 0)
		FROM stripe_connector_credit_grants
		WHERE subject_id = $1 AND bucket = $2
		  AND amount_remaining > 0
		  AND revoked_at IS NULL
		  AND (expires_at IS NULL OR expires_at > now())
	`, string(subject), bucket).Scan(&total, &nextExpiry, &grantCount)
	if err != nil {
		return credits.Balance{}, fmt.Errorf("postgres.CreditRepo balance: %w", err)
	}
	bal := credits.Balance{SubjectID: subject, Bucket: bucket, Total: total, GrantCount: grantCount}
	if nextExpiry.Valid {
		t := nextExpiry.Time
		bal.NextExpiry = &t
	}
	return bal, nil
}

func balanceFromTx(ctx context.Context, tx *sql.Tx, subject credits.SubjectID, bucket string) (credits.Balance, error) {
	return balanceFromQuerier(ctx, tx, subject, bucket)
}
