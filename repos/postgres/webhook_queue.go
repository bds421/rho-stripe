package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/bds421/rho-stripe/webhooks"
)

// WebhookQueue is a Postgres-backed durable implementation of
// webhooks.Queue. Items survive process restarts; multiple workers
// (in separate processes) compete safely via SELECT … FOR UPDATE
// SKIP LOCKED.
//
// Wire:
//
//	q := postgres.NewWebhookQueue(db, postgres.WebhookQueueOptions{
//	    Workers:      4,
//	    PollInterval: 1 * time.Second,
//	    StuckAfter:   5 * time.Minute,
//	})
//	q.Start(wh.ProcessQueued)
//	defer q.Shutdown(ctx)
//	wh.SetQueue(q)
//
// Each worker loops:
//  1. Dequeue (atomic claim).
//  2. Call dispatch (= wh.ProcessQueued).
//  3. On success → DELETE the row.
//  4. On error → release back to 'queued' + increment attempt_count.
//
// A separate sweeper releases rows stuck in 'processing' state longer
// than StuckAfter (crashed worker recovery).
type WebhookQueue struct {
	db   *sql.DB
	opts WebhookQueueOptions

	dispatch func(ctx context.Context, item webhooks.QueueItem) error
	hostID   string

	wg       sync.WaitGroup
	stop     chan struct{}
	stopOnce sync.Once
	logger   *slog.Logger
}

// WebhookQueueOptions configures the durable queue.
type WebhookQueueOptions struct {
	// Workers controls concurrent dispatch goroutines. Default 4.
	Workers int

	// PollInterval is how long an idle worker waits before polling
	// again. Default 1s. (Each worker uses SELECT FOR UPDATE SKIP
	// LOCKED so a busy queue is drained immediately; the interval
	// only matters when the queue is empty.)
	PollInterval time.Duration

	// StuckAfter releases rows stuck in 'processing' state for longer
	// than this. Defaults to 5 minutes. Increase for handlers that
	// genuinely take longer.
	StuckAfter time.Duration

	// MaxAttempts caps retries. 0 = no cap. When exceeded, the row
	// stays in 'queued' state but the impl logs + sleeps before the
	// next try (caller's per-event policy decides whether to give up).
	MaxAttempts int

	// Logger is the structured logger workers use for dequeue /
	// dispatch / sweeper errors. Defaults to slog.Default(). Pass an
	// app-configured logger so the queue's output lands in the same
	// pipeline as the rest of the app (Datadog/Loki/etc.) instead of
	// the previous stderr-only behavior.
	Logger *slog.Logger
}

// NewWebhookQueue constructs a Postgres-backed durable queue.
// Call Start before SetQueue-ing it into the webhook handler.
func NewWebhookQueue(db *sql.DB, opts WebhookQueueOptions) *WebhookQueue {
	if db == nil {
		panic("postgres.NewWebhookQueue: db is required")
	}
	if opts.Workers <= 0 {
		opts.Workers = 4
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = time.Second
	}
	if opts.StuckAfter <= 0 {
		opts.StuckAfter = 5 * time.Minute
	}
	hostname, _ := os.Hostname()
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &WebhookQueue{
		db:     db,
		opts:   opts,
		hostID: fmt.Sprintf("%s-%d", hostname, os.Getpid()),
		stop:   make(chan struct{}),
		logger: logger,
	}
}

var _ webhooks.Queue = (*WebhookQueue)(nil)

// Enqueue inserts the item into the queue table. Duplicate event ids
// are ignored (ON CONFLICT DO NOTHING) so re-delivery of the same
// event before the worker processes it doesn't bloat the queue.
func (q *WebhookQueue) Enqueue(ctx context.Context, item webhooks.QueueItem) error {
	_, err := q.db.ExecContext(ctx, `
		INSERT INTO stripe_connector_webhook_queue
			(event_id, event_type, namespace, body, token)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (event_id) DO NOTHING
	`, item.EventID, item.EventType, item.Namespace, item.Body, item.Token)
	if err != nil {
		return fmt.Errorf("postgres.WebhookQueue.Enqueue: %w", err)
	}
	return nil
}

// Start spawns the worker pool. dispatch is what each worker calls
// for each dequeued item — typically wh.ProcessQueued.
//
// Panics if called twice or with nil dispatch.
func (q *WebhookQueue) Start(dispatch func(ctx context.Context, item webhooks.QueueItem) error) {
	if dispatch == nil {
		panic("postgres.WebhookQueue.Start: dispatch is required")
	}
	if q.dispatch != nil {
		panic("postgres.WebhookQueue.Start: already started")
	}
	q.dispatch = dispatch
	for i := 0; i < q.opts.Workers; i++ {
		q.wg.Add(1)
		go q.worker(i)
	}
	q.wg.Add(1)
	go q.sweeper()
}

// Shutdown stops the worker pool and waits for in-flight handlers.
// Idempotent.
func (q *WebhookQueue) Shutdown(ctx context.Context) error {
	q.stopOnce.Do(func() { close(q.stop) })
	done := make(chan struct{})
	go func() { q.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (q *WebhookQueue) worker(id int) {
	defer q.wg.Done()
	// workerCtx is derived from a fresh context that the worker
	// cancels when q.stop closes. Using context.Background()
	// directly (as the previous impl did) meant in-flight dequeue /
	// dispatch / complete / releaseFailed calls couldn't observe
	// the shutdown signal — they'd run to natural completion
	// regardless of how long Stripe / Postgres took.
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	go func() {
		select {
		case <-q.stop:
		case <-workerCtx.Done():
		}
		cancelWorker()
	}()
	defer cancelWorker()

	idleTimer := time.NewTimer(q.opts.PollInterval)
	defer idleTimer.Stop()
	for {
		select {
		case <-q.stop:
			return
		default:
		}
		item, ok, err := q.dequeue(workerCtx)
		if err != nil {
			q.logger.Warn("postgres.WebhookQueue: worker dequeue failed",
				"worker_id", id, "err", err)
			select {
			case <-q.stop:
				return
			case <-time.After(q.opts.PollInterval):
			}
			continue
		}
		if !ok {
			// Empty queue → wait before polling again.
			select {
			case <-q.stop:
				return
			case <-time.After(q.opts.PollInterval):
			}
			continue
		}
		dispatchCtx, cancel := context.WithTimeout(workerCtx, 5*time.Minute)
		dispatchErr := q.dispatch(dispatchCtx, item)
		cancel()
		if dispatchErr != nil {
			// ErrPermanentFailure (malformed body, schema mismatch) means
			// the item cannot succeed on retry — complete it so the queue
			// stops loading the worker on a poison message.
			if errors.Is(dispatchErr, webhooks.ErrPermanentFailure) {
				q.logger.Error("postgres.WebhookQueue: permanent failure, dead-lettering",
					"event_id", item.EventID, "event_type", item.EventType,
					"err", dispatchErr)
				if err := q.complete(workerCtx, item.EventID); err != nil {
					q.logger.Warn("postgres.WebhookQueue: complete (after permanent failure)",
						"event_id", item.EventID, "err", err)
				}
				continue
			}
			if err := q.releaseFailed(workerCtx, item.EventID); err != nil {
				q.logger.Warn("postgres.WebhookQueue: releaseFailed",
					"event_id", item.EventID, "err", err)
			}
			continue
		}
		if err := q.complete(workerCtx, item.EventID); err != nil {
			q.logger.Warn("postgres.WebhookQueue: complete",
				"event_id", item.EventID, "err", err)
		}
	}
}

// dequeue atomically claims the next queued row (oldest first) using
// SELECT … FOR UPDATE SKIP LOCKED + UPDATE.
func (q *WebhookQueue) dequeue(ctx context.Context) (webhooks.QueueItem, bool, error) {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return webhooks.QueueItem{}, false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var item webhooks.QueueItem
	err = tx.QueryRowContext(ctx, `
		SELECT event_id, event_type, namespace, body, token
		  FROM stripe_connector_webhook_queue
		 WHERE state = 'queued'
		 ORDER BY enqueued_at
		 LIMIT 1
		 FOR UPDATE SKIP LOCKED
	`).Scan(&item.EventID, &item.EventType, &item.Namespace, &item.Body, &item.Token)
	if errors.Is(err, sql.ErrNoRows) {
		return webhooks.QueueItem{}, false, nil
	}
	if err != nil {
		return webhooks.QueueItem{}, false, fmt.Errorf("select: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE stripe_connector_webhook_queue
		   SET state = 'processing',
		       locked_at = now(),
		       locked_by = $2,
		       attempt_count = attempt_count + 1
		 WHERE event_id = $1
	`, item.EventID, q.hostID)
	if err != nil {
		return webhooks.QueueItem{}, false, fmt.Errorf("update lock: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return webhooks.QueueItem{}, false, fmt.Errorf("commit: %w", err)
	}
	return item, true, nil
}

// complete removes the row after a successful dispatch.
func (q *WebhookQueue) complete(ctx context.Context, eventID string) error {
	_, err := q.db.ExecContext(ctx,
		`DELETE FROM stripe_connector_webhook_queue WHERE event_id = $1`, eventID)
	if err != nil {
		return fmt.Errorf("complete: %w", err)
	}
	return nil
}

// releaseFailed pushes the row back to 'queued' state so it can be
// re-dequeued. attempt_count was already incremented in dequeue.
func (q *WebhookQueue) releaseFailed(ctx context.Context, eventID string) error {
	_, err := q.db.ExecContext(ctx, `
		UPDATE stripe_connector_webhook_queue
		   SET state = 'queued', locked_at = NULL, locked_by = NULL
		 WHERE event_id = $1
	`, eventID)
	if err != nil {
		return fmt.Errorf("release: %w", err)
	}
	return nil
}

// sweeper releases rows stuck in 'processing' longer than StuckAfter
// (likely from a crashed worker).
func (q *WebhookQueue) sweeper() {
	defer q.wg.Done()
	// Sweep on the same cadence as the configured stuck window, but
	// no faster than once per minute to keep load low.
	interval := q.opts.StuckAfter
	if interval < time.Minute {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-q.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			res, err := q.db.ExecContext(ctx, `
				UPDATE stripe_connector_webhook_queue
				   SET state = 'queued', locked_at = NULL, locked_by = NULL
				 WHERE state = 'processing' AND locked_at < $1
			`, time.Now().Add(-q.opts.StuckAfter))
			cancel()
			if err != nil {
				q.logger.Warn("postgres.WebhookQueue: sweeper failed", "err", err)
				continue
			}
			if n, _ := res.RowsAffected(); n > 0 {
				q.logger.Info("postgres.WebhookQueue: sweeper released stuck rows", "count", n)
			}
		}
	}
}
