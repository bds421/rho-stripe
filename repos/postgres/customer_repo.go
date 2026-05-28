// Package postgres is the reference implementation of the lib's
// repository interfaces backed by PostgreSQL. It depends on rho-kit's
// idempotency.Store/pgstore for webhook dedup; CustomerRepo and
// CreditRepo are implemented here.
//
// This is a separate Go module (own go.mod) so apps that use a
// different storage backend never pull pgx into their build.
//
// Apply the schema from schema/embed.go with your preferred migration
// tool before constructing repos at runtime.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/bds421/rho-stripe/checkout"
)

// CustomerRepo implements checkout.CustomerRepo using a *sql.DB.
type CustomerRepo struct {
	db *sql.DB
}

// NewCustomerRepo wraps db.
func NewCustomerRepo(db *sql.DB) *CustomerRepo {
	if db == nil {
		panic("postgres.NewCustomerRepo: db is required")
	}
	return &CustomerRepo{db: db}
}

var _ checkout.CustomerRepo = (*CustomerRepo)(nil)

// Get returns the stored mapping, or ("", false, nil) if absent.
func (r *CustomerRepo) Get(ctx context.Context, subject checkout.SubjectID) (checkout.StripeCustomerID, bool, error) {
	var id string
	err := r.db.QueryRowContext(ctx,
		`SELECT stripe_customer_id FROM stripe_connector_customers WHERE subject_id = $1`,
		string(subject),
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("postgres.CustomerRepo.Get: %w", err)
	}
	return checkout.StripeCustomerID(id), true, nil
}

// Upsert stores or updates the mapping. The unique constraint on
// stripe_customer_id ensures a single Stripe Customer is never linked
// to two different SubjectIDs simultaneously (would surface as an
// integration bug).
func (r *CustomerRepo) Upsert(ctx context.Context, subject checkout.SubjectID, id checkout.StripeCustomerID) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO stripe_connector_customers (subject_id, stripe_customer_id)
		VALUES ($1, $2)
		ON CONFLICT (subject_id) DO UPDATE
		    SET stripe_customer_id = EXCLUDED.stripe_customer_id,
		        updated_at         = now()
	`, string(subject), string(id))
	if err != nil {
		return fmt.Errorf("postgres.CustomerRepo.Upsert: %w", err)
	}
	return nil
}
