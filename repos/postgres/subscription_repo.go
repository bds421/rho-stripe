package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/bds421/rho-stripe/subscriptions"
)

// SubscriptionRepo implements subscriptions.SubscriptionRepo against
// a Postgres backing store. Out-of-order webhook delivery is handled
// via a `WHERE stripe_updated_at < EXCLUDED.stripe_updated_at` clause
// on the conflict UPDATE — a stale event Upsert is a silent no-op.
type SubscriptionRepo struct {
	db *sql.DB
}

// NewSubscriptionRepo wraps db.
func NewSubscriptionRepo(db *sql.DB) *SubscriptionRepo {
	if db == nil {
		panic("postgres.NewSubscriptionRepo: db is required")
	}
	return &SubscriptionRepo{db: db}
}

var _ subscriptions.SubscriptionRepo = (*SubscriptionRepo)(nil)

func (r *SubscriptionRepo) Upsert(ctx context.Context, s *subscriptions.Subscription) error {
	itemsJSON, err := json.Marshal(s.Items)
	if err != nil {
		return fmt.Errorf("postgres.SubscriptionRepo.Upsert: marshal items: %w", err)
	}
	metadataJSON, err := json.Marshal(orEmptyMap(s.Metadata))
	if err != nil {
		return fmt.Errorf("postgres.SubscriptionRepo.Upsert: marshal metadata: %w", err)
	}

	_, err = r.db.ExecContext(ctx, `
		INSERT INTO stripe_connector_subscriptions (
		    stripe_id, subject_id, stripe_customer_id, status,
		    current_period_start, current_period_end, cancel_at_period_end,
		    canceled_at, ended_at, trial_start, trial_end,
		    metadata, items, updated_at, stripe_updated_at
		) VALUES (
		    $1, $2, $3, $4,
		    $5, $6, $7,
		    $8, $9, $10, $11,
		    $12, $13, now(), $14
		)
		ON CONFLICT (stripe_id) DO UPDATE
		SET subject_id           = EXCLUDED.subject_id,
		    stripe_customer_id   = EXCLUDED.stripe_customer_id,
		    status               = EXCLUDED.status,
		    current_period_start = EXCLUDED.current_period_start,
		    current_period_end   = EXCLUDED.current_period_end,
		    cancel_at_period_end = EXCLUDED.cancel_at_period_end,
		    canceled_at          = EXCLUDED.canceled_at,
		    ended_at             = EXCLUDED.ended_at,
		    trial_start          = EXCLUDED.trial_start,
		    trial_end            = EXCLUDED.trial_end,
		    metadata             = EXCLUDED.metadata,
		    items                = EXCLUDED.items,
		    updated_at           = now(),
		    stripe_updated_at    = EXCLUDED.stripe_updated_at
		WHERE stripe_connector_subscriptions.stripe_updated_at < EXCLUDED.stripe_updated_at
	`,
		s.StripeID, string(s.SubjectID), s.StripeCustomerID, string(s.Status),
		nullTime(s.CurrentPeriodStart), nullTime(s.CurrentPeriodEnd), s.CancelAtPeriodEnd,
		nullTimePtr(s.CanceledAt), nullTimePtr(s.EndedAt), nullTimePtr(s.TrialStart), nullTimePtr(s.TrialEnd),
		metadataJSON, itemsJSON, s.StripeUpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres.SubscriptionRepo.Upsert: %w", err)
	}
	return nil
}

func (r *SubscriptionRepo) GetByStripeID(ctx context.Context, stripeSubID string) (*subscriptions.Subscription, bool, error) {
	row := r.db.QueryRowContext(ctx, selectSubscriptionFields+` WHERE stripe_id = $1`, stripeSubID)
	sub, err := scanSubscription(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("postgres.SubscriptionRepo.GetByStripeID: %w", err)
	}
	return sub, true, nil
}

func (r *SubscriptionRepo) ListBySubject(ctx context.Context, subject subscriptions.SubjectID) ([]*subscriptions.Subscription, error) {
	rows, err := r.db.QueryContext(ctx, selectSubscriptionFields+` WHERE subject_id = $1 ORDER BY updated_at DESC`, string(subject))
	if err != nil {
		return nil, fmt.Errorf("postgres.SubscriptionRepo.ListBySubject: %w", err)
	}
	defer rows.Close()
	var out []*subscriptions.Subscription
	for rows.Next() {
		sub, err := scanSubscription(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("postgres.SubscriptionRepo.ListBySubject: scan: %w", err)
		}
		out = append(out, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres.SubscriptionRepo.ListBySubject: rows: %w", err)
	}
	return out, nil
}

const selectSubscriptionFields = `
	SELECT stripe_id, subject_id, stripe_customer_id, status,
	       current_period_start, current_period_end, cancel_at_period_end,
	       canceled_at, ended_at, trial_start, trial_end,
	       metadata, items, updated_at, stripe_updated_at
	FROM stripe_connector_subscriptions`

func scanSubscription(scan func(...any) error) (*subscriptions.Subscription, error) {
	var (
		stripeID, subjectID, stripeCustomerID, status string
		currentPeriodStart, currentPeriodEnd          sql.NullTime
		cancelAtPeriodEnd                             bool
		canceledAt, endedAt, trialStart, trialEnd     sql.NullTime
		metadataJSON, itemsJSON                       []byte
		updatedAt, stripeUpdatedAt                    time.Time
	)
	if err := scan(
		&stripeID, &subjectID, &stripeCustomerID, &status,
		&currentPeriodStart, &currentPeriodEnd, &cancelAtPeriodEnd,
		&canceledAt, &endedAt, &trialStart, &trialEnd,
		&metadataJSON, &itemsJSON, &updatedAt, &stripeUpdatedAt,
	); err != nil {
		return nil, err
	}
	sub := &subscriptions.Subscription{
		StripeID:          stripeID,
		SubjectID:         subscriptions.SubjectID(subjectID),
		StripeCustomerID:  stripeCustomerID,
		Status:            subscriptions.Status(status),
		CancelAtPeriodEnd: cancelAtPeriodEnd,
		UpdatedAt:         updatedAt,
		StripeUpdatedAt:   stripeUpdatedAt,
	}
	if currentPeriodStart.Valid {
		sub.CurrentPeriodStart = currentPeriodStart.Time
	}
	if currentPeriodEnd.Valid {
		sub.CurrentPeriodEnd = currentPeriodEnd.Time
	}
	sub.CanceledAt = nullableTime(canceledAt)
	sub.EndedAt = nullableTime(endedAt)
	sub.TrialStart = nullableTime(trialStart)
	sub.TrialEnd = nullableTime(trialEnd)
	if err := json.Unmarshal(metadataJSON, &sub.Metadata); err != nil {
		slog.Warn("postgres.SubscriptionRepo: malformed subscription metadata",
			slog.String("stripe_id", stripeID),
			slog.String("err", err.Error()),
		)
	}
	if err := json.Unmarshal(itemsJSON, &sub.Items); err != nil {
		slog.Warn("postgres.SubscriptionRepo: malformed subscription items",
			slog.String("stripe_id", stripeID),
			slog.String("err", err.Error()),
		)
	}
	return sub, nil
}

func nullTime(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t, Valid: true}
}

func nullTimePtr(t *time.Time) sql.NullTime {
	if t == nil || t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: *t, Valid: true}
}

func nullableTime(n sql.NullTime) *time.Time {
	if !n.Valid {
		return nil
	}
	t := n.Time
	return &t
}

func orEmptyMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
