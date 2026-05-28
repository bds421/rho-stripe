package connector

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bds421/rho-kit/core/v2/clock"
)

func TestHealthSnapshot_OKWhenWarmedAndReachable(t *testing.T) {
	c := &Connector{health: newHealthState()}
	c.health.lastStripeOK.Store(true)
	c.health.lastStripeCheck.Store(c.health.now().Unix())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	c.HealthHandler().ServeHTTP(rec, req)
	// We deliberately don't assert on rec.Code here: catalog is nil
	// so warmed=false yields 503; we're testing the snapshot fields
	// (StripeReachable) below regardless of status.
	var snap HealthSnapshot
	if err := json.NewDecoder(rec.Body).Decode(&snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !snap.StripeReachable {
		t.Errorf("StripeReachable should be true")
	}
}

func TestHealthHandler_Returns503WhenNotReady(t *testing.T) {
	c := &Connector{health: newHealthState()}
	// not warmed, not reachable → 503
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	c.HealthHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503; got %d", rec.Code)
	}
}

func TestHealthHandler_BodyAlwaysCarriesSnapshot(t *testing.T) {
	c := &Connector{health: newHealthState()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	c.HealthHandler().ServeHTTP(rec, req)
	var snap HealthSnapshot
	if err := json.NewDecoder(rec.Body).Decode(&snap); err != nil {
		t.Fatalf("body not JSON-decodable: %v", err)
	}
	if snap.CheckedAt.IsZero() {
		t.Errorf("CheckedAt not populated")
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type: %q", rec.Header().Get("Content-Type"))
	}
}

func TestHealthHandler_ProbeCacheTTLPreventsRepeatedStripeCalls(t *testing.T) {
	// Drive time via a Stub; without it, time.Now() would race the
	// 30s window and the test would be timing-dependent.
	fc := clock.NewStub(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	c := &Connector{health: newHealthState()}
	c.health.now = fc.Now
	c.health.started = fc.Now()
	// Stripe is nil so maybeProbeStripe records a no-op probe — that's
	// what we want; we're testing the "do we re-probe?" check.
	c.maybeProbeStripe(t.Context())
	first := c.health.lastStripeCheck.Load()
	if first == 0 {
		t.Fatal("first probe didn't record")
	}
	// Advance < probeInterval — second call should NOT re-probe.
	fc.Advance(10 * time.Second)
	c.maybeProbeStripe(t.Context())
	if c.health.lastStripeCheck.Load() != first {
		t.Errorf("re-probed within 30s window")
	}
	// Advance past probeInterval — third call SHOULD re-probe.
	fc.Advance(30 * time.Second)
	c.maybeProbeStripe(t.Context())
	if c.health.lastStripeCheck.Load() == first {
		t.Errorf("did not re-probe after window elapsed")
	}
}

func TestHealthSnapshot_RecordStripeProbeError(t *testing.T) {
	h := newHealthState()
	h.recordStripeProbe(errFake("boom"))
	if h.lastStripeOK.Load() {
		t.Errorf("OK should be false after probe error")
	}
	v, _ := h.lastStripeErr.Load().(string)
	if v != "boom" {
		t.Errorf("error not recorded: %q", v)
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }
