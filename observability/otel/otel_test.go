package otel_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/bds421/rho-kit/data/v2/idempotency"
	scotel "github.com/bds421/rho-stripe/observability/otel"
	"github.com/bds421/rho-stripe/webhooks"
	otelapi "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace/noop"
)

// TestWrapWebhookHandler_NoopTracerDoesNotBreakDispatch verifies the
// happy-path wiring with a no-op TracerProvider — the wrapper must
// not interfere with normal webhook handling.
func TestWrapWebhookHandler_NoopTracerDoesNotBreakDispatch(t *testing.T) {
	tracer := noop.NewTracerProvider().Tracer("test")

	var called atomic.Int32
	wh := webhooks.New(webhooks.Config{
		SigningSecret: "whsec_test",
		Store:         idempotency.NewMemoryStore(),
		Handlers: webhooks.Handlers{
			OnCheckoutCompleted: func(context.Context, webhooks.Event) error {
				called.Add(1)
				return nil
			},
		},
	})
	wrapped := scotel.WrapWebhookHandler(wh, tracer)
	signer := webhooks.NewSigner("whsec_test")

	body := []byte(`{"id":"evt_otel","object":"event","api_version":"2025-08-27.basil","type":"checkout.session.completed","created":1700000000,"data":{"object":{}}}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", signer.SignNow(body))
	rec := httptest.NewRecorder()
	wrapped(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if called.Load() != 1 {
		t.Errorf("handler called %d times, want 1", called.Load())
	}
}

// TestWrapQueue_PassThrough verifies the tracing-queue forwards
// Enqueue calls correctly.
func TestWrapQueue_PassThrough(t *testing.T) {
	tracer := otelapi.Tracer("test")
	inner := webhooks.NewMemoryQueue(4, 1, func(context.Context, webhooks.QueueItem) error { return nil })
	t.Cleanup(func() { _ = inner.Shutdown(context.Background()) })
	wrapped := scotel.WrapQueue(inner, tracer)

	err := wrapped.Enqueue(t.Context(), webhooks.QueueItem{
		EventID: "evt_x", EventType: "test", Body: []byte("{}"), Token: "t",
	})
	if err != nil {
		t.Errorf("Enqueue: %v", err)
	}
}
