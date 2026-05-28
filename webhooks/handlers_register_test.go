package webhooks

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/bds421/rho-kit/data/v2/idempotency"
)

func newWebhooksForRegisterTest() *Webhooks {
	return New(Config{
		SigningSecret: "whsec_test",
		Store:         idempotency.NewMemoryStore(),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestRegister_DispatchesCustomEventType(t *testing.T) {
	var called bool
	w := newWebhooksForRegisterTest()
	w.Register("setup_intent.succeeded", func(_ context.Context, _ Event) error {
		called = true
		return nil
	})
	if err := w.dispatchEvent(context.Background(), Event{Type: "setup_intent.succeeded"}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !called {
		t.Fatal("registered handler not invoked")
	}
}

func TestRegister_OverridesTypedHandler(t *testing.T) {
	var typedCalls, customCalls int
	w := New(Config{
		SigningSecret: "whsec_test",
		Store:         idempotency.NewMemoryStore(),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Handlers: Handlers{
			OnInvoicePaid: func(_ context.Context, _ Event) error { typedCalls++; return nil },
		},
	})
	w.Register("invoice.paid", func(_ context.Context, _ Event) error { customCalls++; return nil })
	if err := w.dispatchEvent(context.Background(), Event{Type: "invoice.paid"}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if customCalls != 1 || typedCalls != 0 {
		t.Fatalf("override broken: custom=%d typed=%d", customCalls, typedCalls)
	}
}

func TestRegister_NilUnregisters(t *testing.T) {
	var called bool
	w := newWebhooksForRegisterTest()
	w.Register("foo", func(_ context.Context, _ Event) error { called = true; return nil })
	w.Register("foo", nil)
	if err := w.dispatchEvent(context.Background(), Event{Type: "foo"}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if called {
		t.Fatal("nil Register did not unregister")
	}
}

func TestRegister_EmptyEventTypeIsNoop(t *testing.T) {
	w := newWebhooksForRegisterTest()
	w.Register("", func(_ context.Context, _ Event) error { return errors.New("should not run") })
	if got := w.lookupCustomHandler(""); got != nil {
		t.Fatal("empty event type should not register")
	}
}

func TestRegister_ConcurrentSafe(t *testing.T) {
	w := newWebhooksForRegisterTest()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.Register("event", func(_ context.Context, _ Event) error { return nil })
			_ = w.dispatchEvent(context.Background(), Event{Type: "event"})
		}()
	}
	wg.Wait()
}

func TestRegister_PropagatesErrorFromCustomHandler(t *testing.T) {
	want := errors.New("boom")
	w := newWebhooksForRegisterTest()
	w.Register("event", func(_ context.Context, _ Event) error { return want })
	if got := w.dispatchEvent(context.Background(), Event{Type: "event"}); got != want {
		t.Fatalf("error not propagated: got %v want %v", got, want)
	}
}
