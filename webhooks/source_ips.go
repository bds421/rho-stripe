package webhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// StripeWebhookIPsURL is the upstream JSON Stripe publishes containing
// the list of source IPs they deliver webhooks from. Apps fetch this
// at startup (and optionally on a refresh interval) to keep the
// allowlist in sync with Stripe's reality rather than relying on a
// hardcoded constant that goes stale.
const StripeWebhookIPsURL = "https://stripe.com/files/ips/ips_webhooks.json"

// stripeWebhookIPsURL is the URL the package uses. Always equal to
// StripeWebhookIPsURL in production; tests rewrite via fetchFromURL
// to point at a local httptest.Server.
var stripeWebhookIPsURL = StripeWebhookIPsURL

// FetchedIPs is the result of FetchStripeWebhookIPs.
type FetchedIPs struct {
	// IPs is the raw list of "1.2.3.4" addresses Stripe published.
	IPs []string
	// FetchedAt is when this list was retrieved (monotonic time).
	FetchedAt time.Time
	// Source is the URL the list came from (typically StripeWebhookIPsURL).
	Source string
}

// CIDRs returns each IP as a /32 CIDR string, suitable for
// Config.AllowedSourceCIDRs. Stripe currently publishes only IPv4
// addresses; if they add IPv6, those would need /128.
func (f FetchedIPs) CIDRs() []string {
	out := make([]string, 0, len(f.IPs))
	for _, ip := range f.IPs {
		out = append(out, ip+"/32")
	}
	return out
}

// FetchStripeWebhookIPs GETs the upstream JSON and parses it into a
// FetchedIPs. Pass nil for httpClient to use http.DefaultClient with
// a 10s timeout.
//
// Use at startup:
//
//	ips, err := webhooks.FetchStripeWebhookIPs(ctx, nil)
//	if err != nil {
//	    // fall back to the bundled snapshot — better something than
//	    // nothing, but log so the operator sees the staleness
//	    log.Warn("could not fetch live Stripe IPs; using bundled list", "err", err)
//	    cfg.AllowedSourceCIDRs = webhooks.StripeWebhookCIDRs
//	} else {
//	    cfg.AllowedSourceCIDRs = ips.CIDRs()
//	}
func FetchStripeWebhookIPs(ctx context.Context, httpClient *http.Client) (FetchedIPs, error) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, stripeWebhookIPsURL, nil)
	if err != nil {
		return FetchedIPs{}, fmt.Errorf("webhooks.FetchStripeWebhookIPs: build request: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return FetchedIPs{}, fmt.Errorf("webhooks.FetchStripeWebhookIPs: GET %s: %w", stripeWebhookIPsURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return FetchedIPs{}, fmt.Errorf("webhooks.FetchStripeWebhookIPs: status=%d body=%q", resp.StatusCode, body)
	}
	var payload struct {
		Webhooks []string `json:"WEBHOOKS"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return FetchedIPs{}, fmt.Errorf("webhooks.FetchStripeWebhookIPs: parse JSON: %w", err)
	}
	if len(payload.Webhooks) == 0 {
		return FetchedIPs{}, fmt.Errorf("webhooks.FetchStripeWebhookIPs: upstream returned empty WEBHOOKS list (suspicious — refusing to update)")
	}
	return FetchedIPs{
		IPs:       payload.Webhooks,
		FetchedAt: time.Now().UTC(),
		Source:    stripeWebhookIPsURL,
	}, nil
}

// IPRefresher periodically re-fetches the upstream list and atomically
// updates the Webhooks instance's allowlist. Use when the process
// runs long enough that Stripe might rotate IPs (days+).
//
// Pattern:
//
//	r := webhooks.NewIPRefresher(nil, 24*time.Hour)
//	if err := r.Start(ctx, conn.Webhooks); err != nil { ... }
//	defer r.Stop()
//
// Start synchronously does the first fetch (so the allowlist is
// populated before the HTTP server begins accepting traffic).
type IPRefresher struct {
	client   *http.Client
	interval time.Duration

	mu      sync.Mutex
	stop    context.CancelFunc
	wg      sync.WaitGroup
	cidrs   atomic.Value // []string
	last    atomic.Value // FetchedIPs
	onError func(error)
	cleanup func() // test-only seam; nil in production
}

// NewIPRefresher constructs an IPRefresher.
//
// interval should be >= 1h in production — Stripe rotates webhook IPs
// rarely, and aggressive polling wastes egress to no purpose. Tests
// pass small values (10ms) to drive the refresh loop deterministically.
// Zero/negative interval defaults to 24h.
//
// httpClient may be nil (uses a 10s-timeout default).
func NewIPRefresher(httpClient *http.Client, interval time.Duration) *IPRefresher {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	return &IPRefresher{
		client:   httpClient,
		interval: interval,
	}
}

// SetOnError installs a callback invoked whenever a background refresh
// fails. Default is a no-op (errors are silent so they don't drown the
// operator in noise; the previous successful snapshot keeps protecting).
func (r *IPRefresher) SetOnError(fn func(error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onError = fn
}

// Start fetches once synchronously, then spawns a goroutine that
// re-fetches every interval and updates wh.SetAllowedCIDRs.
//
// Returns the initial fetch error so the caller can decide whether
// to start the HTTP server (fail-closed) or fall back to bundled
// CIDRs (fail-open with a warning).
func (r *IPRefresher) Start(ctx context.Context, wh *Webhooks) error {
	if wh == nil {
		return fmt.Errorf("webhooks.IPRefresher.Start: wh is required")
	}
	ips, err := FetchStripeWebhookIPs(ctx, r.client)
	if err != nil {
		return err
	}
	cidrs := ips.CIDRs()
	r.cidrs.Store(cidrs)
	r.last.Store(ips)
	wh.SetAllowedCIDRs(cidrs)

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stop != nil {
		return fmt.Errorf("webhooks.IPRefresher.Start: already started")
	}
	loopCtx, cancel := context.WithCancel(context.Background())
	r.stop = cancel
	r.wg.Add(1)
	go r.loop(loopCtx, wh)
	return nil
}

// Stop tears down the background goroutine. Safe to call multiple
// times concurrently — the swap-to-nil-under-lock guarantees the
// cleanup func runs at most once.
func (r *IPRefresher) Stop() {
	r.mu.Lock()
	cancel := r.stop
	cleanup := r.cleanup
	r.stop = nil
	r.cleanup = nil
	r.mu.Unlock()
	// Both branches guarded by the swap above: a concurrent second
	// call sees cancel == nil and cleanup == nil and is a no-op.
	if cancel != nil {
		cancel()
		r.wg.Wait()
	}
	if cleanup != nil {
		cleanup()
	}
}

// Last returns the most recent successful fetch. Useful for /healthz
// or admin diagnostics ("when did we last sync the IP allowlist?").
func (r *IPRefresher) Last() (FetchedIPs, bool) {
	v, ok := r.last.Load().(FetchedIPs)
	return v, ok
}

func (r *IPRefresher) loop(ctx context.Context, wh *Webhooks) {
	defer r.wg.Done()
	tick := time.NewTicker(r.interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			ips, err := FetchStripeWebhookIPs(ctx, r.client)
			if err != nil {
				r.mu.Lock()
				cb := r.onError
				r.mu.Unlock()
				if cb != nil {
					cb(err)
				}
				continue
			}
			cidrs := ips.CIDRs()
			r.cidrs.Store(cidrs)
			r.last.Store(ips)
			if err := wh.SetAllowedCIDRs(cidrs); err != nil {
				// Allowlist update failed — the next webhook arriving from a newly
				// rotated Stripe IP will 403 silently. Surface via the configured
				// callback so operators can wire alerting.
				r.mu.Lock()
				cb := r.onError
				r.mu.Unlock()
				if cb != nil {
					cb(fmt.Errorf("apply Stripe webhook CIDR allowlist: %w", err))
				}
			}
		}
	}
}
