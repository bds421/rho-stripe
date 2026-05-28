package webhooks_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bds421/rho-kit/data/v2/idempotency"
	"github.com/bds421/rho-stripe/webhooks"
	stripe "github.com/stripe/stripe-go/v82"
)

const testSecret = "whsec_test_abc123"

// eventJSON constructs a minimal Stripe-event JSON body. id and type
// are caller-controlled so tests can drive dispatch + dedup.
func eventJSON(id, eventType string) []byte {
	return []byte(fmt.Sprintf(
		`{"id":%q,"object":"event","api_version":%q,"type":%q,"livemode":false,"created":%d,"data":{"object":{}}}`,
		id, stripe.APIVersion, eventType, time.Now().Unix(),
	))
}

// eventJSONWithNamespace stamps metadata.app_namespace on the inner
// object so the dispatcher's namespace filter can act on it.
func eventJSONWithNamespace(id, eventType, namespace string) []byte {
	return []byte(fmt.Sprintf(
		`{"id":%q,"object":"event","api_version":%q,"type":%q,"livemode":false,"created":%d,"data":{"object":{"metadata":{"app_namespace":%q}}}}`,
		id, stripe.APIVersion, eventType, time.Now().Unix(), namespace,
	))
}

func newScopedWebhooks(t *testing.T, namespace string, handlers webhooks.Handlers) *webhooks.Webhooks {
	t.Helper()
	return webhooks.New(webhooks.Config{
		SigningSecret: testSecret,
		Store:         idempotency.NewMemoryStore(),
		Namespace:     namespace,
		Handlers:      handlers,
	})
}

func newTestWebhooks(t *testing.T, handlers webhooks.Handlers) *webhooks.Webhooks {
	t.Helper()
	return webhooks.New(webhooks.Config{
		SigningSecret: testSecret,
		Store:         idempotency.NewMemoryStore(),
		Handlers:      handlers,
	})
}

func postSignedEvent(t *testing.T, wh *webhooks.Webhooks, body []byte, signer *webhooks.Signer) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", signer.SignNow(body))
	rec := httptest.NewRecorder()
	wh.Handle(rec, req)
	return rec
}

func TestHandle_ValidSignatureDispatchesAndReturns200(t *testing.T) {
	var called atomic.Int32
	wh := newTestWebhooks(t, webhooks.Handlers{
		OnCheckoutCompleted: func(_ context.Context, evt webhooks.Event) error {
			called.Add(1)
			if evt.ID != "evt_001" {
				t.Errorf("evt.ID = %q, want evt_001", evt.ID)
			}
			return nil
		},
	})
	signer := webhooks.NewSigner(testSecret)

	rec := postSignedEvent(t, wh, eventJSON("evt_001", "checkout.session.completed"), signer)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if called.Load() != 1 {
		t.Errorf("handler called %d times, want 1", called.Load())
	}
}

func TestHandle_InvalidSignatureRejected(t *testing.T) {
	wh := newTestWebhooks(t, webhooks.Handlers{})
	wrongSigner := webhooks.NewSigner("whsec_wrong_secret")

	rec := postSignedEvent(t, wh, eventJSON("evt_bad", "ping"), wrongSigner)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for invalid signature", rec.Code)
	}
}

func TestHandle_MissingSignatureRejected(t *testing.T) {
	wh := newTestWebhooks(t, webhooks.Handlers{})
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(eventJSON("evt_x", "ping")))
	// no Stripe-Signature header
	rec := httptest.NewRecorder()
	wh.Handle(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandle_DuplicateEventNotReprocessed(t *testing.T) {
	var called atomic.Int32
	wh := newTestWebhooks(t, webhooks.Handlers{
		OnCheckoutCompleted: func(_ context.Context, _ webhooks.Event) error {
			called.Add(1)
			return nil
		},
	})
	signer := webhooks.NewSigner(testSecret)
	body := eventJSON("evt_dup", "checkout.session.completed")

	// First delivery
	rec1 := postSignedEvent(t, wh, body, signer)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first delivery status = %d", rec1.Code)
	}

	// Second delivery (duplicate)
	rec2 := postSignedEvent(t, wh, body, signer)
	if rec2.Code != http.StatusOK {
		t.Errorf("duplicate delivery status = %d, want 200", rec2.Code)
	}

	if called.Load() != 1 {
		t.Errorf("handler called %d times for duplicate; expected 1 (dedup failed)", called.Load())
	}
}

func TestHandle_HandlerErrorReturns500AndAllowsRetry(t *testing.T) {
	var attempts atomic.Int32
	wh := newTestWebhooks(t, webhooks.Handlers{
		OnCheckoutCompleted: func(_ context.Context, _ webhooks.Event) error {
			attempts.Add(1)
			if attempts.Load() < 2 {
				return errors.New("transient")
			}
			return nil
		},
	})
	signer := webhooks.NewSigner(testSecret)
	body := eventJSON("evt_retry", "checkout.session.completed")

	rec1 := postSignedEvent(t, wh, body, signer)
	if rec1.Code != http.StatusInternalServerError {
		t.Errorf("first attempt status = %d, want 500 (so Stripe retries)", rec1.Code)
	}

	// Retry should be allowed: the failed attempt released the lock.
	rec2 := postSignedEvent(t, wh, body, signer)
	if rec2.Code != http.StatusOK {
		t.Errorf("retry status = %d, want 200 after success", rec2.Code)
	}
	if attempts.Load() != 2 {
		t.Errorf("handler attempts = %d, want 2 (initial fail + retry succeed)", attempts.Load())
	}
}

func TestHandle_UnknownEventTypeFallsThroughToOnOther(t *testing.T) {
	var called atomic.Int32
	var sawType string
	wh := newTestWebhooks(t, webhooks.Handlers{
		OnOtherEvent: func(_ context.Context, evt webhooks.Event) error {
			called.Add(1)
			sawType = evt.Type
			return nil
		},
	})
	signer := webhooks.NewSigner(testSecret)

	rec := postSignedEvent(t, wh, eventJSON("evt_other", "account.application.deauthorized"), signer)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
	if called.Load() != 1 {
		t.Errorf("OnOtherEvent called %d times, want 1", called.Load())
	}
	if sawType != "account.application.deauthorized" {
		t.Errorf("evt.Type = %q", sawType)
	}
}

func TestHandle_NoHandlerStillMarksEventProcessed(t *testing.T) {
	wh := newTestWebhooks(t, webhooks.Handlers{}) // no handlers at all
	signer := webhooks.NewSigner(testSecret)
	body := eventJSON("evt_nohandler", "checkout.session.completed")

	rec := postSignedEvent(t, wh, body, signer)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	// Re-deliver: should still be 200 (deduped), no handler involvement.
	rec2 := postSignedEvent(t, wh, body, signer)
	if rec2.Code != http.StatusOK {
		t.Errorf("re-delivery status = %d, want 200 (deduped)", rec2.Code)
	}
}

func TestHandle_NamespaceFilter_MatchDispatches(t *testing.T) {
	var typed, other atomic.Int32
	wh := newScopedWebhooks(t, "app1", webhooks.Handlers{
		OnCheckoutCompleted: func(_ context.Context, _ webhooks.Event) error { typed.Add(1); return nil },
		OnOtherEvent:        func(_ context.Context, _ webhooks.Event) error { other.Add(1); return nil },
	})
	signer := webhooks.NewSigner(testSecret)

	rec := postSignedEvent(t, wh, eventJSONWithNamespace("evt_ns1", "checkout.session.completed", "app1"), signer)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
	if typed.Load() != 1 {
		t.Errorf("typed handler called %d times, want 1", typed.Load())
	}
	if other.Load() != 0 {
		t.Errorf("OnOtherEvent should not fire on matched typed event, got %d", other.Load())
	}
}

func TestHandle_NamespaceFilter_MismatchSkipsSilently(t *testing.T) {
	var typed, other atomic.Int32
	wh := newScopedWebhooks(t, "app1", webhooks.Handlers{
		OnCheckoutCompleted: func(_ context.Context, _ webhooks.Event) error { typed.Add(1); return nil },
		OnOtherEvent:        func(_ context.Context, _ webhooks.Event) error { other.Add(1); return nil },
	})
	signer := webhooks.NewSigner(testSecret)

	rec := postSignedEvent(t, wh, eventJSONWithNamespace("evt_ns2", "checkout.session.completed", "app2"), signer)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d (mismatched namespace should still return 200)", rec.Code)
	}
	if typed.Load() != 0 || other.Load() != 0 {
		t.Errorf("no handler should fire on namespace mismatch; got typed=%d other=%d", typed.Load(), other.Load())
	}
}

func TestHandle_NamespaceFilter_MissingStampRoutesToOnOtherOnly(t *testing.T) {
	var typed, other atomic.Int32
	wh := newScopedWebhooks(t, "app1", webhooks.Handlers{
		OnCheckoutCompleted: func(_ context.Context, _ webhooks.Event) error { typed.Add(1); return nil },
		OnOtherEvent:        func(_ context.Context, _ webhooks.Event) error { other.Add(1); return nil },
	})
	signer := webhooks.NewSigner(testSecret)

	// account.application.deauthorized doesn't carry app metadata.
	rec := postSignedEvent(t, wh, eventJSON("evt_acct", "account.application.deauthorized"), signer)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
	if typed.Load() != 0 {
		t.Errorf("typed handler should not fire for unstamped events under namespace mode")
	}
	if other.Load() != 1 {
		t.Errorf("OnOtherEvent should fire once for unstamped events, got %d", other.Load())
	}
}

func TestHandle_NamespaceFilter_EmptyNamespaceDispatchesAll(t *testing.T) {
	var typed atomic.Int32
	wh := newTestWebhooks(t, webhooks.Handlers{
		OnCheckoutCompleted: func(_ context.Context, _ webhooks.Event) error { typed.Add(1); return nil },
	})
	signer := webhooks.NewSigner(testSecret)

	// Different "namespaces" all dispatch when filter is off (empty cfg).
	postSignedEvent(t, wh, eventJSONWithNamespace("evt_a", "checkout.session.completed", "appX"), signer)
	postSignedEvent(t, wh, eventJSONWithNamespace("evt_b", "checkout.session.completed", "appY"), signer)
	if typed.Load() != 2 {
		t.Errorf("with namespace='' all should dispatch; got %d", typed.Load())
	}
}

func TestHandle_NamespaceMismatchStillDeduped(t *testing.T) {
	var typed atomic.Int32
	wh := newScopedWebhooks(t, "app1", webhooks.Handlers{
		OnCheckoutCompleted: func(_ context.Context, _ webhooks.Event) error { typed.Add(1); return nil },
	})
	signer := webhooks.NewSigner(testSecret)
	body := eventJSONWithNamespace("evt_skip_dup", "checkout.session.completed", "app2")

	// First delivery → skipped.
	rec1 := postSignedEvent(t, wh, body, signer)
	if rec1.Code != http.StatusOK {
		t.Errorf("first status = %d", rec1.Code)
	}
	// Retry → still skipped (and deduped).
	rec2 := postSignedEvent(t, wh, body, signer)
	if rec2.Code != http.StatusOK {
		t.Errorf("retry status = %d", rec2.Code)
	}
	if typed.Load() != 0 {
		t.Errorf("typed handler called %d times, want 0", typed.Load())
	}
}

func TestTestSignatureVerification(t *testing.T) {
	wh := newTestWebhooks(t, webhooks.Handlers{})
	if err := wh.TestSignatureVerification(); err != nil {
		t.Errorf("TestSignatureVerification: %v", err)
	}
}

func TestSigner_StableForSameTimestamp(t *testing.T) {
	s := webhooks.NewSigner(testSecret)
	body := []byte(`{"hello":"world"}`)
	t0 := time.Unix(1700000000, 0)
	a := s.Sign(body, t0)
	b := s.Sign(body, t0)
	if a != b {
		t.Errorf("Sign not deterministic for same body+timestamp:\n  a=%s\n  b=%s", a, b)
	}
}

// --- Slice 35: async dispatch queue ---

func TestHandle_AsyncQueueReceivesItemAndReturns200Immediately(t *testing.T) {
	var received atomic.Int32
	dispatched := make(chan webhooks.QueueItem, 1)
	q := webhooks.NewMemoryQueue(4, 1, func(_ context.Context, item webhooks.QueueItem) error {
		received.Add(1)
		dispatched <- item
		return nil
	})
	t.Cleanup(func() { _ = q.Shutdown(context.Background()) })

	wh := webhooks.New(webhooks.Config{
		SigningSecret: testSecret,
		Store:         idempotency.NewMemoryStore(),
		Queue:         q,
		Handlers: webhooks.Handlers{
			// Should NOT be invoked synchronously; the queue worker is
			// our test fake and doesn't dispatch.
			OnCheckoutCompleted: func(_ context.Context, _ webhooks.Event) error {
				t.Errorf("sync dispatch should not fire when queue is set")
				return nil
			},
		},
	})
	signer := webhooks.NewSigner(testSecret)
	rec := postSignedEvent(t, wh, eventJSON("evt_async_1", "checkout.session.completed"), signer)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (async return)", rec.Code)
	}
	select {
	case item := <-dispatched:
		if item.EventID != "evt_async_1" {
			t.Errorf("queued EventID = %q, want evt_async_1", item.EventID)
		}
		if len(item.Body) == 0 {
			t.Error("queued Body is empty")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queue worker never received the item")
	}
	if received.Load() != 1 {
		t.Errorf("worker invocations = %d, want 1", received.Load())
	}
}

func TestMemoryQueue_ShutdownDrainsInflight(t *testing.T) {
	done := make(chan struct{})
	q := webhooks.NewMemoryQueue(1, 1, func(_ context.Context, _ webhooks.QueueItem) error {
		// Hold the worker briefly so Shutdown has to actually wait.
		time.Sleep(50 * time.Millisecond)
		close(done)
		return nil
	})
	if err := q.Enqueue(t.Context(), webhooks.QueueItem{EventID: "evt"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := q.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	select {
	case <-done:
	default:
		t.Error("worker did not finish before Shutdown returned")
	}
}

func TestMemoryQueue_EnqueueAfterShutdownErrors(t *testing.T) {
	q := webhooks.NewMemoryQueue(1, 1, func(_ context.Context, _ webhooks.QueueItem) error { return nil })
	_ = q.Shutdown(context.Background())
	err := q.Enqueue(t.Context(), webhooks.QueueItem{EventID: "x"})
	if err == nil {
		t.Error("expected error enqueueing into a closed queue")
	}
}

// --- Slice 35: per-event-type policy ---

func TestHandle_PerEventPolicy_MaxAttemptsCallsOnExhaustedAndReturns200(t *testing.T) {
	var exhaustedCalled atomic.Int32
	var attempts atomic.Int32

	// EventLog is needed so prior attempt counts are remembered.
	store := idempotency.NewMemoryStore()
	logger := webhooks.NewMemoryLog()

	wh := webhooks.New(webhooks.Config{
		SigningSecret: testSecret,
		Store:         store,
		EventLog:      logger,
		Handlers: webhooks.Handlers{
			OnInvoicePaid: func(_ context.Context, _ webhooks.Event) error {
				attempts.Add(1)
				return errors.New("simulated handler failure")
			},
		},
		PerEventPolicy: map[string]webhooks.EventPolicy{
			"invoice.paid": {
				MaxAttempts: 2,
				OnExhausted: func(_ context.Context, evt webhooks.LoggedEvent) error {
					exhaustedCalled.Add(1)
					if evt.EventID != "evt_pol_1" {
						t.Errorf("OnExhausted EventID = %q", evt.EventID)
					}
					return nil // → return 200 to Stripe
				},
			},
		},
	})
	signer := webhooks.NewSigner(testSecret)
	body := eventJSON("evt_pol_1", "invoice.paid")

	// First attempt: handler fails → 500.
	rec := postSignedEvent(t, wh, body, signer)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("first attempt: status = %d, want 500", rec.Code)
	}
	// Second attempt hits MaxAttempts; OnExhausted returns nil → 200.
	rec = postSignedEvent(t, wh, body, signer)
	if rec.Code != http.StatusOK {
		t.Errorf("second attempt at MaxAttempts: status = %d, want 200", rec.Code)
	}
	if exhaustedCalled.Load() != 1 {
		t.Errorf("OnExhausted invocations = %d, want 1", exhaustedCalled.Load())
	}
}

func TestHandle_PerEventPolicy_OnExhaustedErrorKeepsStripeRetrying(t *testing.T) {
	store := idempotency.NewMemoryStore()
	logger := webhooks.NewMemoryLog()

	wh := webhooks.New(webhooks.Config{
		SigningSecret: testSecret,
		Store:         store,
		EventLog:      logger,
		Handlers: webhooks.Handlers{
			OnInvoicePaid: func(_ context.Context, _ webhooks.Event) error {
				return errors.New("still broken")
			},
		},
		PerEventPolicy: map[string]webhooks.EventPolicy{
			"invoice.paid": {
				MaxAttempts: 1,
				OnExhausted: func(_ context.Context, _ webhooks.LoggedEvent) error {
					return errors.New("operator must investigate")
				},
			},
		},
	})
	signer := webhooks.NewSigner(testSecret)
	rec := postSignedEvent(t, wh, eventJSON("evt_pol_keep", "invoice.paid"), signer)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("OnExhausted returning error: status = %d, want 500 (keep retrying)", rec.Code)
	}
}

// --- Slice 43 EventLog fault injection ---

// faultyEventLog drives every persist call through an error-injecting
// closure so tests can simulate transient log-store outages and verify
// the handler degrades gracefully.
type faultyEventLog struct {
	record func(ctx context.Context, ev webhooks.LoggedEvent) error
	get    func(ctx context.Context, id string) (webhooks.LoggedEvent, bool, error)
	stuck  func(ctx context.Context, min int) ([]webhooks.LoggedEvent, error)
}

func (f *faultyEventLog) Record(ctx context.Context, ev webhooks.LoggedEvent) error {
	if f.record != nil {
		return f.record(ctx, ev)
	}
	return nil
}
func (f *faultyEventLog) Get(ctx context.Context, id string) (webhooks.LoggedEvent, bool, error) {
	if f.get != nil {
		return f.get(ctx, id)
	}
	return webhooks.LoggedEvent{}, false, nil
}
func (f *faultyEventLog) StuckEvents(ctx context.Context, minAttempts int) ([]webhooks.LoggedEvent, error) {
	if f.stuck != nil {
		return f.stuck(ctx, minAttempts)
	}
	return nil, nil
}
func (f *faultyEventLog) PruneOlderThan(_ context.Context, _ time.Time) (int, error) {
	return 0, nil
}

func TestHandle_EventLogPutErrorDoesNotBreakDispatch(t *testing.T) {
	var dispatched atomic.Int32
	flaky := &faultyEventLog{
		record: func(_ context.Context, _ webhooks.LoggedEvent) error {
			return errors.New("event log unavailable")
		},
	}
	wh := webhooks.New(webhooks.Config{
		SigningSecret: testSecret,
		Store:         idempotency.NewMemoryStore(),
		EventLog:      flaky,
		Handlers: webhooks.Handlers{
			OnCheckoutCompleted: func(_ context.Context, _ webhooks.Event) error {
				dispatched.Add(1)
				return nil
			},
		},
	})
	signer := webhooks.NewSigner(testSecret)
	rec := postSignedEvent(t, wh, eventJSON("evt_log_err", "checkout.session.completed"), signer)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (handler succeeded; log failure is non-fatal)", rec.Code)
	}
	if dispatched.Load() != 1 {
		t.Errorf("dispatch count = %d, want 1", dispatched.Load())
	}
}

func TestHandle_EventLogGetErrorDoesNotBreakDispatch(t *testing.T) {
	var dispatched atomic.Int32
	flaky := &faultyEventLog{
		get: func(_ context.Context, _ string) (webhooks.LoggedEvent, bool, error) {
			return webhooks.LoggedEvent{}, false, errors.New("log read failed")
		},
	}
	wh := webhooks.New(webhooks.Config{
		SigningSecret: testSecret,
		Store:         idempotency.NewMemoryStore(),
		EventLog:      flaky,
		Handlers: webhooks.Handlers{
			OnInvoicePaid: func(_ context.Context, _ webhooks.Event) error {
				dispatched.Add(1)
				return errors.New("boom")
			},
		},
		PerEventPolicy: map[string]webhooks.EventPolicy{
			"invoice.paid": {MaxAttempts: 5},
		},
	})
	signer := webhooks.NewSigner(testSecret)
	rec := postSignedEvent(t, wh, eventJSON("evt_log_get_err", "invoice.paid"), signer)
	// Handler error → 500. Log-read failure must not crash dispatch.
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if dispatched.Load() != 1 {
		t.Errorf("dispatch count = %d, want 1", dispatched.Load())
	}
}

// --- Slice 35 follow-up: async dispatch dedup correctness ---

// TestHandle_AsyncQueueDedupsAcrossRedelivery covers the bug surfaced
// in the post-slice-43 audit: the original async path returned 200
// without ever marking the event processed in the dedup store, so a
// Stripe redelivery would re-handle the event after the claim TTL
// expired.
//
// Fix: ProcessQueued (the worker's dispatch entry point) calls
// store.Set after a successful handler run, so a second delivery
// hits the duplicate path and returns 200 without re-invoking the
// handler.
func TestHandle_AsyncQueueDedupsAcrossRedelivery(t *testing.T) {
	var dispatched atomic.Int32
	store := idempotency.NewMemoryStore()

	wh := webhooks.New(webhooks.Config{
		SigningSecret: testSecret,
		Store:         store,
		Handlers: webhooks.Handlers{
			OnCheckoutCompleted: func(_ context.Context, _ webhooks.Event) error {
				dispatched.Add(1)
				return nil
			},
		},
	})

	// Build a synchronous worker for deterministic testing: the queue
	// dispatch returns only after ProcessQueued completes.
	q := webhooks.NewMemoryQueue(4, 1, wh.ProcessQueued)
	t.Cleanup(func() { _ = q.Shutdown(context.Background()) })
	wh.SetQueue(q)

	signer := webhooks.NewSigner(testSecret)
	body := eventJSON("evt_dedup_async", "checkout.session.completed")

	rec := postSignedEvent(t, wh, body, signer)
	if rec.Code != http.StatusOK {
		t.Fatalf("first delivery status = %d, want 200", rec.Code)
	}

	// Wait for the worker to finish processing the first delivery so
	// the dedup store is updated before we send the redelivery.
	waitFor(t, 2*time.Second, func() bool {
		return dispatched.Load() == 1
	})

	// Redeliver: same event id, same body. Must be deduped → handler
	// NOT invoked a second time.
	rec = postSignedEvent(t, wh, body, signer)
	if rec.Code != http.StatusOK {
		t.Fatalf("redelivery status = %d, want 200", rec.Code)
	}

	// Give the queue worker a moment in case the dedup was bypassed.
	time.Sleep(100 * time.Millisecond)
	if got := dispatched.Load(); got != 1 {
		t.Errorf("handler ran %d times on redelivery; want exactly 1 (dedup broken)", got)
	}
}

// TestHandle_AsyncQueueHandlerErrorReleasesLockForRetry verifies that
// when an async handler fails (no per-event-policy exhaustion), the
// dedup lock is released so a Stripe redelivery can re-claim and
// retry, mirroring the sync-mode behavior.
func TestHandle_AsyncQueueHandlerErrorReleasesLockForRetry(t *testing.T) {
	var attempts atomic.Int32
	store := idempotency.NewMemoryStore()
	first := make(chan struct{}, 1)

	wh := webhooks.New(webhooks.Config{
		SigningSecret: testSecret,
		Store:         store,
		Handlers: webhooks.Handlers{
			OnCheckoutCompleted: func(_ context.Context, _ webhooks.Event) error {
				n := attempts.Add(1)
				if n == 1 {
					select {
					case first <- struct{}{}:
					default:
					}
					return errors.New("transient")
				}
				return nil
			},
		},
	})
	q := webhooks.NewMemoryQueue(4, 1, wh.ProcessQueued)
	t.Cleanup(func() { _ = q.Shutdown(context.Background()) })
	wh.SetQueue(q)

	signer := webhooks.NewSigner(testSecret)
	body := eventJSON("evt_async_retry", "checkout.session.completed")

	postSignedEvent(t, wh, body, signer)
	<-first
	// Brief wait so the worker reaches the unlock branch.
	waitFor(t, 1*time.Second, func() bool { return attempts.Load() == 1 })
	time.Sleep(50 * time.Millisecond)

	// Redeliver — claim must be available, second attempt must succeed.
	postSignedEvent(t, wh, body, signer)
	waitFor(t, 2*time.Second, func() bool { return attempts.Load() == 2 })

	if got := attempts.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2 (first fails, redelivery succeeds)", got)
	}
}

func waitFor(t *testing.T, max time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", max)
}

// --- Webhook signing-secret rotation ---

func TestHandle_AdditionalSigningSecretAccepted(t *testing.T) {
	const oldSecret = "whsec_old_secret_123"
	const newSecret = "whsec_new_secret_456"

	var called atomic.Int32
	wh := webhooks.New(webhooks.Config{
		SigningSecret:            newSecret,
		AdditionalSigningSecrets: []string{oldSecret},
		Store:                    idempotency.NewMemoryStore(),
		Handlers: webhooks.Handlers{
			OnCheckoutCompleted: func(_ context.Context, _ webhooks.Event) error {
				called.Add(1)
				return nil
			},
		},
	})

	// Signature signed with the OLD secret should still be accepted.
	oldSigner := webhooks.NewSigner(oldSecret)
	rec := postSignedEvent(t, wh, eventJSON("evt_rotate_1", "checkout.session.completed"), oldSigner)
	if rec.Code != http.StatusOK {
		t.Errorf("old-signed event status = %d, want 200 (rotation acceptance)", rec.Code)
	}

	// New secret also works.
	newSigner := webhooks.NewSigner(newSecret)
	rec = postSignedEvent(t, wh, eventJSON("evt_rotate_2", "checkout.session.completed"), newSigner)
	if rec.Code != http.StatusOK {
		t.Errorf("new-signed event status = %d, want 200", rec.Code)
	}

	// Bogus signature is still rejected.
	badSigner := webhooks.NewSigner("whsec_neither_old_nor_new")
	rec = postSignedEvent(t, wh, eventJSON("evt_rotate_3", "checkout.session.completed"), badSigner)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bogus signature status = %d, want 400", rec.Code)
	}

	if got := called.Load(); got != 2 {
		t.Errorf("handler invocations = %d, want 2 (old + new)", got)
	}
}

// --- Replay ---

func TestReplayEvent_DispatchesFromEventLog(t *testing.T) {
	log := webhooks.NewMemoryLog()
	var called atomic.Int32
	wh := webhooks.New(webhooks.Config{
		SigningSecret: testSecret,
		Store:         idempotency.NewMemoryStore(),
		EventLog:      log,
		Handlers: webhooks.Handlers{
			OnCheckoutCompleted: func(_ context.Context, evt webhooks.Event) error {
				called.Add(1)
				if evt.ID != "evt_replay_1" {
					t.Errorf("evt.ID=%q", evt.ID)
				}
				return nil
			},
		},
	})

	// Drive a normal event so the log captures it.
	signer := webhooks.NewSigner(testSecret)
	postSignedEvent(t, wh, eventJSON("evt_replay_1", "checkout.session.completed"), signer)
	if called.Load() != 1 {
		t.Fatalf("initial dispatch didn't fire (got %d)", called.Load())
	}

	// Replay — handler must fire again.
	if err := wh.ReplayEvent(context.Background(), "evt_replay_1"); err != nil {
		t.Fatalf("ReplayEvent: %v", err)
	}
	if called.Load() != 2 {
		t.Errorf("after replay, handler invocations = %d, want 2", called.Load())
	}
}

func TestReplayEvent_ErrorsWhenLogNotConfigured(t *testing.T) {
	wh := webhooks.New(webhooks.Config{
		SigningSecret: testSecret,
		Store:         idempotency.NewMemoryStore(),
	})
	err := wh.ReplayEvent(context.Background(), "evt_x")
	if !errors.Is(err, webhooks.ErrEventLogNotConfigured) {
		t.Errorf("want ErrEventLogNotConfigured, got %v", err)
	}
}

func TestReplayEvent_ErrorsWhenEventNotInLog(t *testing.T) {
	wh := webhooks.New(webhooks.Config{
		SigningSecret: testSecret,
		Store:         idempotency.NewMemoryStore(),
		EventLog:      webhooks.NewMemoryLog(),
	})
	err := wh.ReplayEvent(context.Background(), "evt_never_seen")
	if !errors.Is(err, webhooks.ErrEventNotInLog) {
		t.Errorf("want ErrEventNotInLog, got %v", err)
	}
}

// --- Typed dispute + trial-will-end handlers ---

func TestHandle_TypedDispatch_DisputeAndTrial(t *testing.T) {
	calls := map[string]*atomic.Int32{
		"dispute_created":          new(atomic.Int32),
		"dispute_updated":          new(atomic.Int32),
		"dispute_closed":           new(atomic.Int32),
		"dispute_funds_withdrawn":  new(atomic.Int32),
		"dispute_funds_reinstated": new(atomic.Int32),
		"trial_will_end":           new(atomic.Int32),
	}
	wh := newTestWebhooks(t, webhooks.Handlers{
		OnDisputeCreated:           func(context.Context, webhooks.Event) error { calls["dispute_created"].Add(1); return nil },
		OnDisputeUpdated:           func(context.Context, webhooks.Event) error { calls["dispute_updated"].Add(1); return nil },
		OnDisputeClosed:            func(context.Context, webhooks.Event) error { calls["dispute_closed"].Add(1); return nil },
		OnDisputeFundsWithdrawn:    func(context.Context, webhooks.Event) error { calls["dispute_funds_withdrawn"].Add(1); return nil },
		OnDisputeFundsReinstated:   func(context.Context, webhooks.Event) error { calls["dispute_funds_reinstated"].Add(1); return nil },
		OnSubscriptionTrialWillEnd: func(context.Context, webhooks.Event) error { calls["trial_will_end"].Add(1); return nil },
	})
	signer := webhooks.NewSigner(testSecret)

	cases := []struct {
		key       string
		eventType string
	}{
		{"dispute_created", "charge.dispute.created"},
		{"dispute_updated", "charge.dispute.updated"},
		{"dispute_closed", "charge.dispute.closed"},
		{"dispute_funds_withdrawn", "charge.dispute.funds_withdrawn"},
		{"dispute_funds_reinstated", "charge.dispute.funds_reinstated"},
		{"trial_will_end", "customer.subscription.trial_will_end"},
	}
	for _, c := range cases {
		t.Run(c.eventType, func(t *testing.T) {
			rec := postSignedEvent(t, wh, eventJSON("evt_"+c.key, c.eventType), signer)
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", rec.Code)
			}
			if calls[c.key].Load() != 1 {
				t.Errorf("handler for %q calls = %d, want 1", c.eventType, calls[c.key].Load())
			}
		})
	}
}
