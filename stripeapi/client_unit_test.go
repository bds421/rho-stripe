package stripeapi

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// TestNewClient_AppliesRoundTripperMiddleware verifies that the
// Config.RoundTripperMiddleware hook is invoked and wraps the
// transport stripe-go ultimately uses.
func TestNewClient_AppliesRoundTripperMiddleware(t *testing.T) {
	var wrapCount atomic.Int32
	cfg := Config{
		SecretKey: "sk_test_unit",
		Timeout:   1 * time.Second,
		RoundTripperMiddleware: func(next http.RoundTripper) http.RoundTripper {
			wrapCount.Add(1)
			return next
		},
	}
	_ = NewClient(cfg)
	if wrapCount.Load() != 1 {
		t.Fatalf("middleware called %d times, want 1", wrapCount.Load())
	}
}

// TestNewClient_MaxNetworkRetriesPropagates verifies a configured
// retry count reaches the underlying backend. We can't inspect
// stripe-go's internal backend config from outside the package, but
// we CAN exercise the construction path with each variant and verify
// it doesn't panic — the goal is regression coverage that the wiring
// stays intact.
func TestNewClient_MaxNetworkRetriesVariants(t *testing.T) {
	cases := []struct {
		name    string
		retries int
	}{
		{"default", 0},
		{"explicit 5", 5},
		{"disabled", -1},
		{"high", 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sc := NewClient(Config{
				SecretKey:         "sk_test_unit",
				MaxNetworkRetries: c.retries,
			})
			if sc == nil {
				t.Fatalf("NewClient returned nil for %s", c.name)
			}
		})
	}
}

// TestNewClient_RequiresSecretKey verifies the panic-on-empty-key
// guard.
func TestNewClient_RequiresSecretKey(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on empty SecretKey")
		}
	}()
	_ = NewClient(Config{})
}
