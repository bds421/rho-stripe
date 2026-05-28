package webhooks

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bds421/rho-kit/data/v2/idempotency"
)

// BenchmarkHandle_SuccessfulDispatch measures end-to-end webhook
// handling: signature verify + JSON decode + dedup-claim + typed
// dispatch. Establishes the per-request cost ceiling.
func BenchmarkHandle_SuccessfulDispatch(b *testing.B) {
	secret := "whsec_bench_secret_with_enough_length"
	wh := New(Config{
		SigningSecret: secret,
		Store:         idempotency.NewMemoryStore(),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Handlers: Handlers{
			OnPaymentSucceeded: func(_ context.Context, _ Event) error { return nil },
		},
	})

	signer := NewSigner(secret)
	body := mustBuildPayloadForBench("evt_bench", "payment_intent.succeeded")
	sig := signer.SignNow(body)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
			req.Header.Set("Stripe-Signature", sig)
			wh.Handle(rec, req)
			if rec.Code != http.StatusOK {
				b.Fatalf("status: %d", rec.Code)
			}
		}
	})
}

// BenchmarkHandle_DuplicateEvent measures the dedup-hit path (every
// request hits the same event id, so only the first does real work).
func BenchmarkHandle_DuplicateEvent(b *testing.B) {
	secret := "whsec_bench_secret_with_enough_length"
	wh := New(Config{
		SigningSecret: secret,
		Store:         idempotency.NewMemoryStore(),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Handlers: Handlers{
			OnPaymentSucceeded: func(_ context.Context, _ Event) error { return nil },
		},
	})
	signer := NewSigner(secret)
	body := mustBuildPayloadForBench("evt_bench_dup", "payment_intent.succeeded")
	sig := signer.SignNow(body)
	// Prime the dedup cache.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", sig)
	wh.Handle(rec, req)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
		req.Header.Set("Stripe-Signature", sig)
		wh.Handle(rec, req)
	}
}

func mustBuildPayloadForBench(id, eventType string) []byte {
	evt := map[string]any{
		"id":          id,
		"object":      "event",
		"type":        eventType,
		"livemode":    false,
		"created":     time.Now().Unix(),
		"api_version": "2025-08-27.basil",
		"data": map[string]any{
			"object": map[string]any{
				"id":     "pi_bench",
				"object": "payment_intent",
				"amount": 1000,
				"status": "succeeded",
				"metadata": map[string]string{
					"app_namespace": "",
				},
			},
		},
	}
	out, err := json.Marshal(evt)
	if err != nil {
		panic(err)
	}
	return out
}
