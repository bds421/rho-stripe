package webhooks

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Queue is the optional async-dispatch queue webhook events flow
// through when Config.Queue is set. When enabled, Handle returns 200
// to Stripe as soon as the event is enqueued (signature verified +
// dedup-claimed), and the actual handler dispatch happens on a
// worker goroutine. Apps wire this for high-volume webhook endpoints
// where slow handlers risk Stripe 10s timeouts.
//
// See [adr-0006] for the design rationale.
type Queue interface {
	// Enqueue takes ownership of evt + body for later processing.
	Enqueue(ctx context.Context, item QueueItem) error
}

// QueueItem is one queued event payload, ready to be dispatched.
type QueueItem struct {
	EventID   string
	EventType string
	Namespace string
	Body      []byte // raw request body (already signature-verified)
	Token     string // idempotency-store token from TryLock; used to mark processed on success
}

// MemoryQueue is an in-memory channel-backed Queue suitable for
// development and tests. Production apps wire a durable queue
// (Postgres-backed, NATS, SQS, etc.). The MemoryQueue spawns N
// worker goroutines on construction.
type MemoryQueue struct {
	ch       chan QueueItem
	dispatch func(ctx context.Context, item QueueItem) error
	wg       sync.WaitGroup
	closed   chan struct{}
	logger   *slog.Logger
	// closeMu serializes Shutdown's close-of-ch against Enqueue's
	// send-to-ch. Without it Enqueue can race past the `closed`
	// channel check and try to send to an already-closed ch (panic).
	closeMu sync.RWMutex
	// errors counts dispatch failures since process start. Exposed via
	// DispatchErrors() so /readyz or Prometheus can alert on rising
	// counts; the absolute number is less interesting than its rate of
	// change.
	errors atomic.Uint64
}

// NewMemoryQueue starts `workers` goroutines that drain the queue
// by invoking dispatch on each item. Capacity bounds the channel
// (typically a few hundred); a full channel makes Enqueue block.
//
// Dispatch errors are logged via slog.Default(); use NewMemoryQueueWithLogger
// to route into your structured logger.
func NewMemoryQueue(capacity, workers int, dispatch func(ctx context.Context, item QueueItem) error) *MemoryQueue {
	return NewMemoryQueueWithLogger(capacity, workers, dispatch, nil)
}

// NewMemoryQueueWithLogger is like NewMemoryQueue but lets callers
// supply their own slog.Logger. nil falls back to slog.Default().
func NewMemoryQueueWithLogger(capacity, workers int, dispatch func(ctx context.Context, item QueueItem) error, logger *slog.Logger) *MemoryQueue {
	if dispatch == nil {
		panic("webhooks.NewMemoryQueue: dispatch is required")
	}
	if capacity <= 0 {
		capacity = 256
	}
	if workers <= 0 {
		workers = 4
	}
	if logger == nil {
		logger = slog.Default()
	}
	q := &MemoryQueue{
		ch:       make(chan QueueItem, capacity),
		dispatch: dispatch,
		closed:   make(chan struct{}),
		logger:   logger,
	}
	for i := 0; i < workers; i++ {
		q.wg.Add(1)
		go q.worker()
	}
	return q
}

// DispatchErrors returns the total number of failed dispatches since
// the queue was constructed. Operators alert on the rate of change.
func (q *MemoryQueue) DispatchErrors() uint64 { return q.errors.Load() }

func (q *MemoryQueue) worker() {
	defer q.wg.Done()
	for item := range q.ch {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := q.dispatch(ctx, item); err != nil {
			q.errors.Add(1)
			q.logger.ErrorContext(ctx, "webhooks.MemoryQueue: dispatch failed",
				slog.String("event_id", item.EventID),
				slog.String("event_type", item.EventType),
				slog.String("namespace", item.Namespace),
				slog.String("err", err.Error()),
			)
		}
		cancel()
	}
}

// Depth returns the current count of items waiting in the queue
// (channel buffer length). Useful for /healthz, Prometheus gauges,
// and "are we backed up?" alerts.
//
// Returns 0 after Shutdown. Doesn't block; cheap to call at any rate.
func (q *MemoryQueue) Depth() int {
	return len(q.ch)
}

// Capacity returns the maximum queue depth (the channel buffer size).
// Combined with Depth(), apps compute saturation.
func (q *MemoryQueue) Capacity() int {
	return cap(q.ch)
}

// Enqueue blocks if the queue is full. Returns an error if the queue
// has been Shutdown.
func (q *MemoryQueue) Enqueue(ctx context.Context, item QueueItem) error {
	// Hold the read lock for the duration of the send so Shutdown
	// (which takes the write lock before closing ch) cannot run
	// concurrently. Multiple Enqueues remain concurrent.
	q.closeMu.RLock()
	defer q.closeMu.RUnlock()
	select {
	case <-q.closed:
		return errors.New("webhooks.MemoryQueue: closed")
	default:
	}
	select {
	case q.ch <- item:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-q.closed:
		return errors.New("webhooks.MemoryQueue: closed")
	}
}

// Shutdown waits for in-flight dispatches to complete. Apps call this
// on graceful shutdown. Idempotent.
func (q *MemoryQueue) Shutdown(ctx context.Context) error {
	q.closeMu.Lock()
	select {
	case <-q.closed:
		q.closeMu.Unlock()
		return nil
	default:
		close(q.closed)
		close(q.ch)
	}
	q.closeMu.Unlock()

	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
