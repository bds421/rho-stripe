package webhooks

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bds421/rho-kit/data/v2/idempotency"
)

func TestHandle_IPAllowlist_NoListAllowsAny(t *testing.T) {
	w := newTestWebhookWithCIDRs(t, nil, "")
	rec := httptest.NewRecorder()
	req := newSignedPing(t)
	req.RemoteAddr = "203.0.113.7:54321"
	w.Handle(rec, req)
	// Without an allowlist, the signature path runs (and will fail
	// because we use a junk body); 400 is fine — we're verifying the
	// IP check didn't reject.
	if rec.Code == http.StatusForbidden {
		t.Fatalf("unconfigured allowlist rejected; got 403")
	}
}

func TestHandle_IPAllowlist_AllowsConfiguredCIDR(t *testing.T) {
	w := newTestWebhookWithCIDRs(t, []string{"203.0.113.0/24"}, "")
	rec := httptest.NewRecorder()
	req := newSignedPing(t)
	req.RemoteAddr = "203.0.113.7:54321"
	w.Handle(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("in-range IP rejected: 203.0.113.7 in 203.0.113.0/24")
	}
}

func TestHandle_IPAllowlist_RejectsOutOfRange(t *testing.T) {
	w := newTestWebhookWithCIDRs(t, []string{"203.0.113.0/24"}, "")
	rec := httptest.NewRecorder()
	req := newSignedPing(t)
	req.RemoteAddr = "198.51.100.1:54321"
	w.Handle(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("out-of-range IP not rejected; got %d", rec.Code)
	}
}

func TestHandle_IPAllowlist_HonorsXForwardedFor(t *testing.T) {
	w := newTestWebhookWithCIDRs(t, []string{"203.0.113.0/24"}, "X-Forwarded-For")
	rec := httptest.NewRecorder()
	req := newSignedPing(t)
	// RemoteAddr is the LB, X-Forwarded-For is the real client.
	req.RemoteAddr = "10.0.0.99:443"
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.99")
	w.Handle(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("XFF first-hop should be allowlisted; got 403")
	}
}

func TestHandle_IPAllowlist_RejectsXForwardedForFromBadIP(t *testing.T) {
	w := newTestWebhookWithCIDRs(t, []string{"203.0.113.0/24"}, "X-Forwarded-For")
	rec := httptest.NewRecorder()
	req := newSignedPing(t)
	req.RemoteAddr = "10.0.0.99:443"
	req.Header.Set("X-Forwarded-For", "198.51.100.1")
	w.Handle(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("XFF outside allowlist not rejected; got %d", rec.Code)
	}
}

func TestNew_InvalidCIDRPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on invalid CIDR")
		}
	}()
	_ = New(Config{
		SigningSecret:      "whsec_test",
		Store:              idempotency.NewMemoryStore(),
		AllowedSourceCIDRs: []string{"not.a.cidr"},
	})
}

func TestSetAllowedCIDRs_AtomicReplace(t *testing.T) {
	w := newTestWebhookWithCIDRs(t, []string{"10.0.0.0/8"}, "")
	if err := w.SetAllowedCIDRs([]string{"203.0.113.0/24"}); err != nil {
		t.Fatalf("SetAllowedCIDRs: %v", err)
	}
	// Old CIDR no longer allowed; new one is.
	rec := httptest.NewRecorder()
	req := newSignedPing(t)
	req.RemoteAddr = "10.0.0.1:80"
	w.Handle(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("after replace, old CIDR should be rejected")
	}

	rec2 := httptest.NewRecorder()
	req2 := newSignedPing(t)
	req2.RemoteAddr = "203.0.113.7:80"
	w.Handle(rec2, req2)
	if rec2.Code == http.StatusForbidden {
		t.Errorf("after replace, new CIDR should be accepted")
	}
}

func TestSetAllowedCIDRs_InvalidCIDRReturnsError(t *testing.T) {
	w := newTestWebhookWithCIDRs(t, nil, "")
	err := w.SetAllowedCIDRs([]string{"bogus"})
	if err == nil {
		t.Fatal("want error on invalid CIDR")
	}
}

// --- helpers ---

func newTestWebhookWithCIDRs(t *testing.T, cidrs []string, realIPHeader string) *Webhooks {
	t.Helper()
	return New(Config{
		SigningSecret:      "whsec_test",
		Store:              idempotency.NewMemoryStore(),
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		AllowedSourceCIDRs: cidrs,
		RealIPHeader:       realIPHeader,
	})
}

// newSignedPing returns a request whose body is a junk JSON event —
// the signature check is expected to fail with 400. The tests only
// care whether the IP check happens BEFORE that 400.
func newSignedPing(t *testing.T) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader([]byte(`{"id":"evt_test"}`)))
	req.Header.Set("Stripe-Signature", "t=0,v1=junk")
	return req
}
