package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/bds421/rho-stripe/metering"
)

// UsageRepo implements metering.UsageRepo against Postgres.
// Idempotency on (subject, metric, request_id) enforced by the
// UNIQUE constraint; RecordUsage uses INSERT ... ON CONFLICT DO
// NOTHING to silently no-op duplicates.
type UsageRepo struct {
	db *sql.DB
}

func NewUsageRepo(db *sql.DB) *UsageRepo {
	if db == nil {
		panic("postgres.NewUsageRepo: db is required")
	}
	return &UsageRepo{db: db}
}

var _ metering.UsageRepo = (*UsageRepo)(nil)

func (r *UsageRepo) RecordUsage(ctx context.Context, evt metering.MeterEvent) error {
	metadataJSON, _ := json.Marshal(orEmptyMap(evt.Metadata))
	occurredAt := evt.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO stripe_connector_usage_events
		    (subject_id, metric, quantity, occurred_at, request_id, metadata)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (subject_id, metric, request_id) DO NOTHING
	`, string(evt.SubjectID), evt.Metric, evt.Quantity, occurredAt, evt.RequestID, metadataJSON)
	if err != nil {
		return fmt.Errorf("postgres.UsageRepo.RecordUsage: %w", err)
	}
	return nil
}

func (r *UsageRepo) AggregateByPeriod(ctx context.Context, metric string, period metering.Period) ([]metering.UsageAggregate, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT subject_id, COALESCE(SUM(quantity), 0)
		FROM stripe_connector_usage_events
		WHERE metric = $1
		  AND occurred_at >= $2
		  AND occurred_at < $3
		GROUP BY subject_id
		ORDER BY subject_id
	`, metric, period.Start, period.End)
	if err != nil {
		return nil, fmt.Errorf("postgres.UsageRepo.AggregateByPeriod: %w", err)
	}
	defer rows.Close()
	var out []metering.UsageAggregate
	for rows.Next() {
		var subj string
		var total int64
		if err := rows.Scan(&subj, &total); err != nil {
			return nil, err
		}
		out = append(out, metering.UsageAggregate{
			SubjectID: metering.SubjectID(subj), Metric: metric,
			Total: total, Period: period,
		})
	}
	return out, rows.Err()
}

func (r *UsageRepo) QueryByPeriod(ctx context.Context, subject metering.SubjectID, metric string, start, end time.Time) (int64, error) {
	var total int64
	err := r.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(quantity), 0)
		FROM stripe_connector_usage_events
		WHERE subject_id = $1 AND metric = $2
		  AND occurred_at >= $3 AND occurred_at < $4
	`, string(subject), metric, start, end).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("postgres.UsageRepo.QueryByPeriod: %w", err)
	}
	return total, nil
}

func (r *UsageRepo) PruneOlderThan(ctx context.Context, before time.Time) (int, error) {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM stripe_connector_usage_events WHERE occurred_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("postgres.UsageRepo.PruneOlderThan: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
