//go:build postgres_integration

package postgres_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bds421/rho-stripe/repos/postgres"
	"github.com/bds421/rho-stripe/webhooks"
)

func TestPostgresWebhookQueue_EnqueueAndDispatch(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	q := postgres.NewWebhookQueue(db, postgres.WebhookQueueOptions{
		Workers:      2,
		PollInterval: 50 * time.Millisecond,
	})

	var dispatched atomic.Int32
	done := make(chan webhooks.QueueItem, 4)
	q.Start(func(_ context.Context, item webhooks.QueueItem) error {
		dispatched.Add(1)
		done <- item
		return nil
	})
	t.Cleanup(func() { _ = q.Shutdown(context.Background()) })

	for i := 0; i < 3; i++ {
		err := q.Enqueue(t.Context(), webhooks.QueueItem{
			EventID:   "evt_pg_" + itoa(i),
			EventType: "test.event",
			Body:      []byte(`{"id":"evt_pg_x","type":"test.event"}`),
			Token:     "tok_" + itoa(i),
		})
		if err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}

	// Wait for all 3 to dispatch.
	deadline := time.After(5 * time.Second)
	got := 0
	for got < 3 {
		select {
		case <-done:
			got++
		case <-deadline:
			t.Fatalf("only %d/3 dispatched (atomic count = %d)", got, dispatched.Load())
		}
	}

	// The row DELETE happens in the worker AFTER our dispatch callback returns,
	// so it races the `done` signal we just drained — on a slow runner the last
	// row(s) may not be gone the instant all 3 callbacks have fired. Poll until
	// the queue drains (deterministic) rather than asserting once immediately,
	// which flaked on CI ("expected 0 rows after dispatch, got 1").
	drained := time.After(5 * time.Second)
	for {
		var remaining int
		if err := db.QueryRow(`SELECT COUNT(*) FROM stripe_connector_webhook_queue`).Scan(&remaining); err != nil {
			t.Fatalf("count: %v", err)
		}
		if remaining == 0 {
			break
		}
		select {
		case <-drained:
			t.Fatalf("expected 0 rows after dispatch, got %d", remaining)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func TestPostgresWebhookQueue_DuplicateEnqueueIgnored(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	q := postgres.NewWebhookQueue(db, postgres.WebhookQueueOptions{
		Workers:      1,
		PollInterval: time.Hour, // effectively pause workers; we check rows directly
	})
	q.Start(func(context.Context, webhooks.QueueItem) error { return nil })
	t.Cleanup(func() { _ = q.Shutdown(context.Background()) })

	item := webhooks.QueueItem{
		EventID:   "evt_dup",
		EventType: "test.event",
		Body:      []byte(`{}`),
		Token:     "t",
	}
	if err := q.Enqueue(t.Context(), item); err != nil {
		t.Fatalf("Enqueue 1: %v", err)
	}
	if err := q.Enqueue(t.Context(), item); err != nil {
		t.Fatalf("Enqueue 2: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM stripe_connector_webhook_queue WHERE event_id = $1`, "evt_dup").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 row after duplicate enqueue, got %d (ON CONFLICT failed)", n)
	}
}

func TestPostgresWebhookQueue_DispatchErrorReleasesForRetry(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	q := postgres.NewWebhookQueue(db, postgres.WebhookQueueOptions{
		Workers:      1,
		PollInterval: 50 * time.Millisecond,
	})

	var attempts atomic.Int32
	q.Start(func(_ context.Context, _ webhooks.QueueItem) error {
		if attempts.Add(1) < 2 {
			return errFakeTransient
		}
		return nil
	})
	t.Cleanup(func() { _ = q.Shutdown(context.Background()) })

	if err := q.Enqueue(t.Context(), webhooks.QueueItem{
		EventID:   "evt_retry",
		EventType: "test.event",
		Body:      []byte(`{}`),
		Token:     "t",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for attempts.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("attempts=%d, want at least 2", attempts.Load())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

var errFakeTransient = newTestErr("transient")

type testErr string

func (e testErr) Error() string { return string(e) }

func newTestErr(s string) testErr { return testErr(s) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
