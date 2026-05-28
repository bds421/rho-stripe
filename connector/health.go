package connector

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bds421/rho-kit/core/v2/clock"
)

// HealthSnapshot is the JSON body emitted by HealthHandler.
//
// Apps typically wire two endpoints:
//
//	/healthz (liveness)  → always returns 200 if the process is up.
//	/readyz  (readiness) → returns 503 when the connector hasn't yet
//	                       warmed its catalog or Stripe is unreachable.
//
// HealthHandler covers /readyz semantics. For /healthz, just return
// a 200 from a one-line handler — process-aliveness doesn't depend
// on Stripe being reachable.
type HealthSnapshot struct {
	OK              bool      `json:"ok"`
	UptimeSeconds   float64   `json:"uptime_seconds"`
	CatalogWarmed   bool      `json:"catalog_warmed"`
	StripeReachable bool      `json:"stripe_reachable"`
	StripeError     string    `json:"stripe_error,omitempty"`
	LastStripeCheck time.Time `json:"last_stripe_check"`
	CheckedAt       time.Time `json:"checked_at"`

	// QueueDepth is the current depth of the in-memory async webhook
	// queue, when one is wired. Useful for spotting a saturating
	// queue from /readyz without setting up Prometheus scraping.
	// Zero with QueueCapacity=0 means no queue is wired (sync
	// dispatch mode).
	QueueDepth    int `json:"queue_depth"`
	QueueCapacity int `json:"queue_capacity"`
}

// healthState tracks the last live-probe outcome so /readyz hits
// don't burn a Stripe API call per request.
type healthState struct {
	started         time.Time
	mu              sync.RWMutex
	lastStripeOK    atomic.Bool
	lastStripeErr   atomic.Value // string
	lastStripeCheck atomic.Int64 // unix
	probeInterval   time.Duration
	now             clock.Func
}

func newHealthState() *healthState {
	now := clock.System()
	return &healthState{
		started:       now(),
		probeInterval: 30 * time.Second,
		now:           now,
	}
}

func (h *healthState) snapshot(catalogWarmed bool, qd, qc int) HealthSnapshot {
	errStr, _ := h.lastStripeErr.Load().(string)
	checkUnix := h.lastStripeCheck.Load()
	var lastCheck time.Time
	if checkUnix > 0 {
		lastCheck = time.Unix(checkUnix, 0).UTC()
	}
	reachable := h.lastStripeOK.Load()
	now := h.now()
	return HealthSnapshot{
		OK:              catalogWarmed && reachable,
		UptimeSeconds:   now.Sub(h.started).Seconds(),
		CatalogWarmed:   catalogWarmed,
		StripeReachable: reachable,
		StripeError:     errStr,
		LastStripeCheck: lastCheck,
		CheckedAt:       now.UTC(),
		QueueDepth:      qd,
		QueueCapacity:   qc,
	}
}

func (h *healthState) recordStripeProbe(err error) {
	h.lastStripeCheck.Store(h.now().Unix())
	if err != nil {
		h.lastStripeOK.Store(false)
		h.lastStripeErr.Store(err.Error())
		return
	}
	h.lastStripeOK.Store(true)
	h.lastStripeErr.Store("")
}

// HealthHandler returns an http.Handler emitting HealthSnapshot JSON
// for use as a /readyz probe.
//
// Status code: 200 when OK, 503 otherwise. Bodies always carry the
// full snapshot so dashboards / human operators can diagnose without
// hitting another endpoint.
//
// Stripe reachability is checked at most once per probeInterval
// (default 30s) — the result is cached, so a Kubernetes-style 1Hz
// probe doesn't slam Stripe's `/v1/account` endpoint.
//
// healthState is initialized in connector.New so concurrent first
// calls to HealthHandler can't race on the field write.
func (c *Connector) HealthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.maybeProbeStripe(r.Context())
		qd, qc := c.queueStats()
		snap := c.health.snapshot(c.Catalog != nil && c.Catalog.Warmed(), qd, qc)
		w.Header().Set("Content-Type", "application/json")
		if !snap.OK {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(snap)
	})
}

// maybeProbeStripe issues at most one Stripe API call per
// probeInterval, regardless of incoming probe rate.
func (c *Connector) maybeProbeStripe(ctx context.Context) {
	last := c.health.lastStripeCheck.Load()
	if last > 0 && c.health.now().Sub(time.Unix(last, 0)) < c.health.probeInterval {
		return
	}
	if c.Stripe == nil {
		// No client to probe — record the snapshot without forcing a call.
		c.health.recordStripeProbe(nil)
		return
	}
	c.health.mu.Lock()
	defer c.health.mu.Unlock()
	// Re-check inside the lock; another goroutine may have probed.
	last = c.health.lastStripeCheck.Load()
	if last > 0 && c.health.now().Sub(time.Unix(last, 0)) < c.health.probeInterval {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := c.Stripe.V1Accounts.Retrieve(probeCtx, nil)
	c.health.recordStripeProbe(err)
}

// SetHealthClock replaces the now-function used by the health probe
// + uptime counter. Used by tests to drive the 30s probe-cache TTL
// deterministically without sleeping.
func (c *Connector) SetHealthClock(fn clock.Func) {
	c.health.now = clock.OrSystem(fn)
	c.health.started = c.health.now()
}

// queueStats returns the in-memory webhook queue depth + capacity,
// or (0, 0) when no MemoryQueue is wired (sync dispatch mode, OR a
// non-MemoryQueue impl like the Postgres queue is in use).
func (c *Connector) queueStats() (depth, capacity int) {
	if c.Webhooks == nil {
		return 0, 0
	}
	type depthCapacitier interface {
		Depth() int
		Capacity() int
	}
	if q, ok := c.Webhooks.QueueForStats().(depthCapacitier); ok {
		return q.Depth(), q.Capacity()
	}
	return 0, 0
}
