package metrics_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/bds421/rho-kit/data/v2/idempotency"
	scmetrics "github.com/bds421/rho-stripe/observability/metrics"
	"github.com/bds421/rho-stripe/webhooks"
	"github.com/prometheus/client_golang/prometheus"
)

func TestCollector_WrapWebhookHandlerRecordsStatus(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := scmetrics.New(reg)

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
	handler := m.WrapWebhookHandler(wh)
	signer := webhooks.NewSigner("whsec_test")

	body := []byte(`{"id":"evt_m","object":"event","api_version":"2025-08-27.basil","type":"checkout.session.completed","created":1700000000,"data":{"object":{}}}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", signer.SignNow(body))
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if called.Load() != 1 {
		t.Errorf("handler called %d times", called.Load())
	}

	// Verify the metric registered + observed.
	got := readCounter(t, reg, "stripe_connector_webhook_requests_total")
	if got < 1 {
		t.Errorf("expected counter >= 1, got %d", got)
	}
}

func TestCollector_WrapProcessQueuedRecordsErrors(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := scmetrics.New(reg)

	dispatch := m.WrapProcessQueued(func(context.Context, webhooks.QueueItem) error {
		return errors.New("simulated failure")
	})
	err := dispatch(context.Background(), webhooks.QueueItem{EventType: "test.event"})
	if err == nil {
		t.Fatal("expected error to bubble")
	}
	if got := readCounter(t, reg, "stripe_connector_queue_dispatch_errors_total"); got < 1 {
		t.Errorf("error counter not incremented: got %d", got)
	}
}

func TestCollector_SetQueueDepth(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := scmetrics.New(reg)
	m.SetQueueDepth("default", 42)
	if got := readGauge(t, reg, "stripe_connector_queue_depth"); got != 42 {
		t.Errorf("gauge = %v, want 42", got)
	}
}

// readCounter reads the sum of a registered counter family.
func readCounter(t *testing.T, reg *prometheus.Registry, name string) int64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var total float64
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if c := m.GetCounter(); c != nil {
				total += c.GetValue()
			}
		}
	}
	return int64(total)
}

func readGauge(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if g := m.GetGauge(); g != nil {
				return g.GetValue()
			}
		}
	}
	return 0
}
