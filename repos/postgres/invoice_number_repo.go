package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/bds421/rho-stripe/invoices"
)

// InvoiceNumberRepo implements invoices.NumberRepo against Postgres.
// Each Next call:
//
//  1. Opens a transaction.
//  2. Locks the per-prefix counter via SELECT … FOR UPDATE on the most
//     recent issued row (or a no-op SELECT on the table when the prefix
//     is new — the inserted row then becomes the new tip).
//  3. Inserts (number, prefix, counter, 'issued').
//  4. Commits.
//
// The per-prefix UNIQUE (prefix, counter) constraint guarantees the
// sequence is gapless within a prefix even under concurrent issuance.
type InvoiceNumberRepo struct {
	db        *sql.DB
	formatter func(prefix string, counter int64) string
}

// NumberFormatter renders (prefix, counter) into the final invoice
// number string. Defaults to "<prefix><6-digit zero-padded counter>".
type NumberFormatter func(prefix string, counter int64) string

// NewInvoiceNumberRepo wraps a *sql.DB. The default formatter is
// fmt.Sprintf("%s%06d", prefix, counter). Pass WithFormatter to
// customize (e.g. "AT-2026-000123" instead of "AT-2026-000001").
func NewInvoiceNumberRepo(db *sql.DB, opts ...InvoiceNumberOption) *InvoiceNumberRepo {
	if db == nil {
		panic("postgres.NewInvoiceNumberRepo: db is required")
	}
	r := &InvoiceNumberRepo{
		db:        db,
		formatter: defaultFormatter,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// InvoiceNumberOption configures a repo at construction time.
type InvoiceNumberOption func(*InvoiceNumberRepo)

// WithFormatter replaces the default counter-rendering formatter.
func WithFormatter(f NumberFormatter) InvoiceNumberOption {
	return func(r *InvoiceNumberRepo) {
		if f != nil {
			r.formatter = f
		}
	}
}

func defaultFormatter(prefix string, counter int64) string {
	return fmt.Sprintf("%s%06d", prefix, counter)
}

var (
	_ invoices.NumberRepo     = (*InvoiceNumberRepo)(nil)
	_ invoices.OrphanReporter = (*InvoiceNumberRepo)(nil)
)

// ListIssuedOlderThan returns numbers still in "issued" state whose
// issued_at is older than deadline. Apps run this periodically to
// detect orphans (Next() succeeded but MarkUsed/MarkVoided was never
// reached — process crash, network outage, missed OrphanHook call).
func (r *InvoiceNumberRepo) ListIssuedOlderThan(ctx context.Context, deadline time.Time) ([]invoices.IssuedNumber, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT number, prefix, counter, issued_at
		  FROM stripe_connector_invoice_numbers
		 WHERE state = 'issued' AND issued_at < $1
		 ORDER BY prefix, counter
	`, deadline)
	if err != nil {
		return nil, fmt.Errorf("postgres.InvoiceNumberRepo.ListIssuedOlderThan: %w", err)
	}
	defer rows.Close()
	var out []invoices.IssuedNumber
	for rows.Next() {
		var in invoices.IssuedNumber
		if err := rows.Scan(&in.Number, &in.Prefix, &in.Counter, &in.IssuedAt); err != nil {
			return nil, fmt.Errorf("postgres.InvoiceNumberRepo.ListIssuedOlderThan: scan: %w", err)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// Next allocates the next number for prefix and persists it as
// 'issued' inside a single transaction. The advisory-lock-free
// approach uses SELECT FOR UPDATE on the existing row with the
// highest counter for this prefix, serializing concurrent callers.
func (r *InvoiceNumberRepo) Next(ctx context.Context, prefix string) (string, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("postgres.InvoiceNumberRepo.Next: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Serialize concurrent Next() on the same prefix via a per-prefix
	// transaction-scoped advisory lock. Postgres doesn't allow
	// `SELECT MAX(...) FOR UPDATE` (aggregates + row locks don't mix),
	// and using FOR UPDATE on the raw rows still has a row-level race
	// (concurrent inserts could both compute next=current+1). The
	// advisory lock pattern is the same one the credits ledger uses.
	// 64-bit lock key (vs hashtext's 32 bits) keeps the birthday
	// collision rate negligible: the previous hashtext($1) saw ~25%
	// collision probability at ~16k distinct prefixes (different prefixes
	// would unnecessarily serialize). md5-prefix→bigint gives ~2^64
	// space, collision-free for any practical app.
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(('x' || substr(md5($1), 1, 16))::bit(64)::bigint)`,
		"stripe_connector_invoice_numbers:"+prefix,
	); err != nil {
		return "", fmt.Errorf("postgres.InvoiceNumberRepo.Next: advisory lock: %w", err)
	}
	var current sql.NullInt64
	err = tx.QueryRowContext(ctx, `
		SELECT MAX(counter) FROM stripe_connector_invoice_numbers
		 WHERE prefix = $1
	`, prefix).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("postgres.InvoiceNumberRepo.Next: read max counter: %w", err)
	}

	next := int64(1)
	if current.Valid {
		next = current.Int64 + 1
	}
	number := r.formatter(prefix, next)

	_, err = tx.ExecContext(ctx, `
		INSERT INTO stripe_connector_invoice_numbers
		    (number, prefix, counter, state)
		VALUES ($1, $2, $3, 'issued')
	`, number, prefix, next)
	if err != nil {
		return "", fmt.Errorf("postgres.InvoiceNumberRepo.Next: insert: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("postgres.InvoiceNumberRepo.Next: commit: %w", err)
	}
	return number, nil
}

// MarkUsed records the Stripe invoice id against the number. Idempotent
// when the same stripeInvoiceID is passed; returns
// ErrNumberAlreadyResolved if a different Stripe id was previously
// recorded (or if the number was voided).
func (r *InvoiceNumberRepo) MarkUsed(ctx context.Context, number, stripeInvoiceID string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgres.InvoiceNumberRepo.MarkUsed: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var state string
	var existingStripeID sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT state, stripe_invoice_id FROM stripe_connector_invoice_numbers
		 WHERE number = $1
		 FOR UPDATE
	`, number).Scan(&state, &existingStripeID)
	if errors.Is(err, sql.ErrNoRows) {
		return invoices.ErrNumberNotIssued
	}
	if err != nil {
		return fmt.Errorf("postgres.InvoiceNumberRepo.MarkUsed: read: %w", err)
	}

	switch state {
	case "issued":
		_, err = tx.ExecContext(ctx, `
			UPDATE stripe_connector_invoice_numbers
			   SET state = 'used', stripe_invoice_id = $2, resolved_at = $3
			 WHERE number = $1
		`, number, stripeInvoiceID, time.Now().UTC())
		if err != nil {
			return fmt.Errorf("postgres.InvoiceNumberRepo.MarkUsed: update: %w", err)
		}
	case "used":
		if existingStripeID.Valid && existingStripeID.String == stripeInvoiceID {
			return tx.Commit() // idempotent no-op
		}
		return invoices.ErrNumberAlreadyResolved
	case "voided":
		return invoices.ErrNumberAlreadyResolved
	}
	return tx.Commit()
}

// MarkVoided records the number as intentionally skipped. Idempotent;
// returns ErrNumberAlreadyResolved if the number was previously used.
func (r *InvoiceNumberRepo) MarkVoided(ctx context.Context, number, reason string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgres.InvoiceNumberRepo.MarkVoided: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var state string
	err = tx.QueryRowContext(ctx, `
		SELECT state FROM stripe_connector_invoice_numbers
		 WHERE number = $1
		 FOR UPDATE
	`, number).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return invoices.ErrNumberNotIssued
	}
	if err != nil {
		return fmt.Errorf("postgres.InvoiceNumberRepo.MarkVoided: read: %w", err)
	}

	switch state {
	case "issued":
		_, err = tx.ExecContext(ctx, `
			UPDATE stripe_connector_invoice_numbers
			   SET state = 'voided', void_reason = $2, resolved_at = $3
			 WHERE number = $1
		`, number, reason, time.Now().UTC())
		if err != nil {
			return fmt.Errorf("postgres.InvoiceNumberRepo.MarkVoided: update: %w", err)
		}
	case "voided":
		return tx.Commit() // idempotent no-op
	case "used":
		return invoices.ErrNumberAlreadyResolved
	}
	return tx.Commit()
}
