package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bds421/rho-kit/data/v2/idempotency"
	stripe "github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/webhook"
)

// DefaultDedupTTL is how long a processed event is remembered for
// dedup. Stripe retries delivery for up to 3 days; the default leaves
// headroom for re-deliveries plus operational replays.
const DefaultDedupTTL = 7 * 24 * time.Hour

// DefaultSignatureTolerance is the maximum clock skew allowed between
// Stripe's signature timestamp and the local clock. Stripe's own SDK
// uses 5 minutes; this matches.
const DefaultSignatureTolerance = 5 * time.Minute

// DefaultSyncDispatchTimeout bounds how long a synchronous handler
// invocation runs before the lib cancels its context. Set well below
// Stripe's ~30s ACK timeout so a stuck handler doesn't trigger Stripe
// retries (which spawn more stuck handlers). Async dispatch path
// uses the queue's own per-item timeout instead (5 min in the
// Postgres queue impl).
const DefaultSyncDispatchTimeout = 20 * time.Second

// okResponse is the cached-response value we stash in the idempotency
// store for processed events. Stripe only inspects the HTTP status
// code; body content is irrelevant on retry-of-already-processed.
var okResponse = idempotency.CachedResponse{StatusCode: http.StatusOK}

// DefaultMaxBodyBytes is the default cap on inbound webhook payload
// size. Stripe events are well under 100KiB in practice; 1MiB leaves
// generous headroom while preventing a malicious actor from streaming
// arbitrary-sized bodies to exhaust memory.
//
// Memory footprint per in-flight event = ~MaxBodyBytes (the raw body
// is held until dispatch completes; for async dispatch, until the
// worker drains it from the queue). At 1MiB and a 256-deep async
// queue, peak in-flight = 256 MiB. Apps with tight memory budgets
// should lower MaxBodyBytes — Stripe will never legitimately send
// more than a few KiB.
const DefaultMaxBodyBytes int64 = 1 << 20 // 1 MiB

// Config configures Webhooks.
type Config struct {
	// SigningSecret is the webhook endpoint's primary signing secret
	// (whsec_…). Required.
	SigningSecret string

	// AdditionalSigningSecrets accepts ZERO OR MORE legacy/staging
	// secrets that should also be tried during signature verification.
	// Use this during webhook secret rotation: deploy the new secret as
	// SigningSecret, keep the old one(s) here for the duration of the
	// rotation window, then remove from AdditionalSigningSecrets once
	// Stripe has cycled to the new secret across all your endpoints.
	//
	// Empty or nil disables the multi-secret path entirely.
	AdditionalSigningSecrets []string

	// Store deduplicates events; required.
	//
	// Performance note: each webhook does ONE Store roundtrip for
	// dedup-claim. At ~1k events/sec, that's 1k DB round-trips/sec
	// against Postgres — well within reasonable bounds but not
	// batch-amortized. For higher throughput (>10k req/s), the
	// AsyncWebhooks queue path lets the dispatch happen off the
	// signature-verify hot path so claims happen in workers.
	// Native batch-claim support is a future rho-kit enhancement.
	Store idempotency.Store

	// Namespace is this app's namespace. When non-empty the dispatcher
	// skips events whose metadata.app_namespace differs (events meant
	// for other apps in a shared Stripe account, per adr-0007). Events
	// without any namespace stamp are dispatched to OnOtherEvent only
	// (treated as account-level / unrouteable).
	//
	// When empty the dispatcher routes every event without filtering —
	// useful only for single-app setups or for testing.
	Namespace string

	// Handlers is the typed-handler registry.
	Handlers Handlers

	// DedupTTL overrides DefaultDedupTTL.
	DedupTTL time.Duration

	// SignatureTolerance overrides DefaultSignatureTolerance.
	SignatureTolerance time.Duration

	// Logger receives structured logs about dedup, dispatch, and
	// failures. Defaults to slog.Default().
	Logger *slog.Logger

	// StrictAPIVersion rejects events whose api_version field differs
	// from the stripe-go SDK's. Default false (lenient): a mismatch
	// only triggers a one-time warning. Most apps don't need strict
	// matching because the lib mostly reads ids and metadata, not
	// deeply-nested fields that change shape across API versions.
	StrictAPIVersion bool

	// EventLog optionally persists per-event metadata + payload so
	// apps can replay events and inspect stuck ones via the
	// Webhooks.StuckEvents / Webhooks.RetrieveEvent / Webhooks.PruneOldEvents
	// helpers. When nil, those helpers return empty results / no-op.
	EventLog EventLog

	// Queue optionally enables async dispatch: Handle returns 200 to
	// Stripe as soon as the event is verified + claimed, and the
	// actual handler runs on a worker draining the queue. When nil,
	// dispatch is synchronous (the default).
	//
	// Two equivalent ways to wire a queue: set this Config.Queue at
	// New time, OR call wh.SetQueue(q) after construction. SetQueue
	// exists because the in-memory queue's dispatch callback needs
	// wh.ProcessQueued, which doesn't exist until after wh = New().
	// Apps using the connector facade don't deal with this — the
	// facade wires it correctly via SetQueue.
	Queue Queue

	// PerEventPolicy optionally overrides MaxAttempts / give-up
	// behavior per event type. Map key is the Stripe event type
	// (e.g. "invoice.paid"); unset event types use the default
	// (retry-forever-with-alert per adr-0006). Useful when some
	// events should be allowed to die after N attempts (write-offs,
	// account-level admin events).
	PerEventPolicy map[string]EventPolicy

	// SyncDispatchTimeout bounds the per-event sync-dispatch handler
	// runtime. A handler exceeding this gets its context canceled;
	// the lib returns 500 to Stripe (which then retries).
	//
	// Defaults to DefaultSyncDispatchTimeout (20s, comfortably below
	// Stripe's ~30s ACK timeout). Set to -1 to disable (DANGEROUS —
	// a stuck handler then ties up the HTTP goroutine indefinitely
	// AND triggers Stripe retries that spawn MORE stuck goroutines).
	SyncDispatchTimeout time.Duration

	// MaxBodyBytes caps the size of webhook payloads the handler will
	// accept. Defaults to DefaultMaxBodyBytes (1 MiB) — well above any
	// Stripe event Stripe has ever sent. Set to 0 to use the default;
	// set to -1 to disable (DANGEROUS — opens DoS surface).
	//
	// A request larger than this returns 413 Payload Too Large
	// without reading the full body, so attackers can't waste memory.
	MaxBodyBytes int64

	// AllowedSourceCIDRs optionally restricts inbound webhook requests
	// to source IPs in the supplied list of CIDRs (defense-in-depth on
	// top of signature verification). When empty, no IP check runs.
	//
	// To restrict to Stripe's published webhook IPs, pass
	// webhooks.StripeWebhookCIDRs. Stripe rarely changes the list;
	// when they do you'll need to redeploy with an updated slice.
	//
	// Set RealIPHeader if your app sits behind a load balancer that
	// terminates TLS — otherwise r.RemoteAddr is the LB's IP, not
	// Stripe's, and every request will be rejected.
	AllowedSourceCIDRs []string

	// RealIPHeader is the HTTP header carrying the real client IP when
	// the app is behind a proxy / load balancer. Common values:
	// "X-Forwarded-For" (Cloudflare / AWS ELB) or "X-Real-IP" (nginx).
	// Required when AllowedSourceCIDRs is set and the app is not
	// directly reachable from the public internet.
	RealIPHeader string
}

// EventPolicy configures per-event-type failure behavior.
type EventPolicy struct {
	// MaxAttempts is the cap after which the handler stops trying.
	// 0 = no cap (default; retry forever).
	MaxAttempts int

	// OnExhausted is invoked when MaxAttempts is reached. Return nil
	// to stop Stripe retries (200), or an error to keep them going
	// (500). Default behavior (nil OnExhausted) returns 500 so Stripe
	// keeps retrying.
	OnExhausted func(ctx context.Context, evt LoggedEvent) error
}

// Webhooks is the lib's webhook handler. Construct via New, then call
// Handle from any HTTP framework (passing raw http.ResponseWriter and
// *http.Request).
type Webhooks struct {
	signingSecret     string
	additionalSecrets []string
	store             idempotency.Store
	namespace         string
	handlers          Handlers
	dedupTTL          time.Duration
	sigTolerance      time.Duration
	logger            *slog.Logger
	strictAPIVersion  bool
	eventLog          EventLog
	perEventPolicy    map[string]EventPolicy

	// queue may be swapped at runtime via SetQueue (typically to wire
	// an in-memory queue after construction so the worker dispatch
	// can refer to wh.ProcessQueued). queueMu guards reads + swaps.
	queueMu sync.RWMutex
	queue   Queue

	// allowedCIDRs is held under cidrMu for thread-safe updates from
	// IPRefresher. Initial value is populated from Config.AllowedSourceCIDRs;
	// SetAllowedCIDRs replaces atomically at runtime.
	cidrMu       sync.RWMutex
	allowedCIDRs []*net.IPNet
	realIPHeader string

	maxBodyBytes        int64
	syncDispatchTimeout time.Duration

	// customMu guards customHandlers. Lives on *Webhooks (not on
	// Handlers) so Handlers stays a pure value-type that's safe to
	// copy through Config plumbing without tripping go vet's
	// copylocks analyzer.
	customMu       sync.RWMutex
	customHandlers map[string]func(context.Context, Event) error

	// sigFailLimiter throttles signature-failure log emissions. An
	// attacker firing junk signatures at the endpoint would otherwise
	// produce one log line per request, DoS-ing the logger.
	sigFailLimiter logRateLimiter
}

// logRateLimiter implements a per-(sliding-second) throttle: at most
// burstPerSec logs per second, with the suppressed count emitted on
// the next allowed log call. Zero value is usable (burst defaults
// to 10/sec).
type logRateLimiter struct {
	mu             sync.Mutex
	lastSec        int64
	emittedThisSec int
	suppressed     int64
	burstPerSec    int
}

// recordAndShouldLog returns the number of suppressed messages since
// the last allowed log (>=0 means "emit this line, include the count"),
// or -1 meaning "suppress this line".
func (l *logRateLimiter) recordAndShouldLog() int64 {
	const defaultBurst = 10
	now := time.Now().Unix()
	l.mu.Lock()
	defer l.mu.Unlock()
	burst := l.burstPerSec
	if burst <= 0 {
		burst = defaultBurst
	}
	if now != l.lastSec {
		l.lastSec = now
		l.emittedThisSec = 0
	}
	if l.emittedThisSec >= burst {
		l.suppressed++
		return -1
	}
	l.emittedThisSec++
	dropped := l.suppressed
	l.suppressed = 0
	return dropped
}

// New constructs Webhooks. Panics on missing required config — webhook
// misconfiguration causes silent payment loss, so failing fast at
// startup is correct.
func New(cfg Config) *Webhooks {
	if cfg.SigningSecret == "" {
		panic("webhooks.New: SigningSecret is required")
	}
	if cfg.Store == nil {
		panic("webhooks.New: Store is required")
	}
	w := &Webhooks{
		signingSecret:     cfg.SigningSecret,
		additionalSecrets: cfg.AdditionalSigningSecrets,
		store:             cfg.Store,
		namespace:         cfg.Namespace,
		handlers:          cfg.Handlers,
		dedupTTL:          cfg.DedupTTL,
		sigTolerance:      cfg.SignatureTolerance,
		logger:            cfg.Logger,
		strictAPIVersion:  cfg.StrictAPIVersion,
		eventLog:          cfg.EventLog,
		queue:             cfg.Queue,
		perEventPolicy:    cfg.PerEventPolicy,
	}
	if w.dedupTTL == 0 {
		w.dedupTTL = DefaultDedupTTL
	}
	if w.sigTolerance == 0 {
		w.sigTolerance = DefaultSignatureTolerance
	}
	if w.logger == nil {
		w.logger = slog.Default()
	}
	for _, cidr := range cfg.AllowedSourceCIDRs {
		_, parsed, err := net.ParseCIDR(cidr)
		if err != nil {
			panic("webhooks.New: invalid CIDR " + cidr + ": " + err.Error())
		}
		w.allowedCIDRs = append(w.allowedCIDRs, parsed)
	}
	w.realIPHeader = cfg.RealIPHeader
	switch {
	case cfg.MaxBodyBytes == 0:
		w.maxBodyBytes = DefaultMaxBodyBytes
	case cfg.MaxBodyBytes < 0:
		w.maxBodyBytes = 0 // disabled (DANGEROUS)
	default:
		w.maxBodyBytes = cfg.MaxBodyBytes
	}
	switch {
	case cfg.SyncDispatchTimeout == 0:
		w.syncDispatchTimeout = DefaultSyncDispatchTimeout
	case cfg.SyncDispatchTimeout < 0:
		w.syncDispatchTimeout = 0 // disabled (DANGEROUS)
	default:
		w.syncDispatchTimeout = cfg.SyncDispatchTimeout
	}
	return w
}

// StripeWebhookCIDRs is a *bundled fallback* snapshot of Stripe's
// published webhook source IPs, suitable for offline / egress-blocked
// environments. PREFER FetchStripeWebhookIPs at startup (and ideally
// IPRefresher for long-running processes) — Stripe rotates IPs and
// the bundled list will go stale.
//
// Snapshot validated against StripeWebhookIPsURL on
// StripeWebhookCIDRsLastUpdated. If your deployment is older than
// ~6 months and you can't reach Stripe for the live list, expect
// false-rejects of legitimate webhooks.
//
// Stripe currently publishes only IPv4 addresses for webhook
// delivery. If they add IPv6, append /128 entries here AND update
// FetchStripeWebhookIPs to handle the v6 case (currently it would
// pass them through as IPs but apps using CIDRs() would need /128).
var (
	StripeWebhookCIDRs = []string{
		"3.18.12.63/32",
		"3.69.109.8/32",
		"3.120.168.93/32",
		"3.130.192.231/32",
		"13.235.14.237/32",
		"13.235.122.149/32",
		"18.211.135.69/32",
		"35.154.171.200/32",
		"35.157.207.129/32",
		"52.15.183.38/32",
		"54.88.130.119/32",
		"54.88.130.237/32",
		"54.187.174.169/32",
		"54.187.205.235/32",
		"54.187.216.72/32",
	}
	// StripeWebhookCIDRsLastUpdated is the date the constant was last
	// reconciled against StripeWebhookIPsURL. Format: YYYY-MM-DD.
	StripeWebhookCIDRsLastUpdated = "2026-05-27"
)

// extractClientIP returns the request's client IP, honoring
// RealIPHeader (X-Forwarded-For etc.) when configured. Returns "" if
// no IP could be determined.
func (w *Webhooks) extractClientIP(r *http.Request) string {
	if w.realIPHeader != "" {
		h := r.Header.Get(w.realIPHeader)
		if h != "" {
			// X-Forwarded-For is a comma-separated list; the first
			// entry is the original client.
			if i := strings.IndexByte(h, ','); i >= 0 {
				h = h[:i]
			}
			return strings.TrimSpace(h)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ipInAllowlist returns true if the supplied IP falls inside any
// configured CIDR.
func (w *Webhooks) ipInAllowlist(ipStr string) bool {
	w.cidrMu.RLock()
	cidrs := w.allowedCIDRs
	w.cidrMu.RUnlock()
	if len(cidrs) == 0 {
		return true
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, n := range cidrs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// allowlistConfigured returns true when the IP filter is active. Used
// by Handle to skip the check entirely when no allowlist is set.
func (w *Webhooks) allowlistConfigured() bool {
	w.cidrMu.RLock()
	defer w.cidrMu.RUnlock()
	return len(w.allowedCIDRs) > 0
}

// SetAllowedCIDRs atomically replaces the runtime IP allowlist.
// Invalid CIDRs return an error and leave the existing list intact.
//
// Used by IPRefresher to apply newly-fetched Stripe IPs without
// restarting the process.
func (w *Webhooks) SetAllowedCIDRs(cidrs []string) error {
	parsed := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return fmt.Errorf("webhooks.SetAllowedCIDRs: %s: %w", c, err)
		}
		parsed = append(parsed, n)
	}
	w.cidrMu.Lock()
	w.allowedCIDRs = parsed
	w.cidrMu.Unlock()
	return nil
}

// Handle is the HTTP entrypoint apps wire into their server. It reads
// the raw body, verifies the Stripe signature, deduplicates by
// event.id, and dispatches to the typed handler.
//
// Status codes returned to Stripe:
//   - 200: event processed (or duplicate / no handler).
//   - 400: signature verification failed.
//   - 500: handler returned an error; Stripe will retry per its schedule.
//
// CRITICAL: no middleware may consume r.Body before this handler
// runs. See docs/INTEGRATION.md for per-framework guidance.
func (w *Webhooks) Handle(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if w.allowlistConfigured() {
		clientIP := w.extractClientIP(r)
		if !w.ipInAllowlist(clientIP) {
			w.logger.WarnContext(ctx, "webhook: source IP rejected",
				"client_ip", clientIP, "remote_addr", r.RemoteAddr)
			http.Error(rw, "source IP not allowed", http.StatusForbidden)
			return
		}
	}
	if w.maxBodyBytes > 0 {
		r.Body = http.MaxBytesReader(rw, r.Body, w.maxBodyBytes)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		// http.MaxBytesReader returns *http.MaxBytesError on exceeding
		// the limit; differentiate so the operator sees the actual
		// failure mode in logs.
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			w.logger.WarnContext(ctx, "webhook: request body exceeds MaxBodyBytes (rejecting before signature check)",
				"limit", w.maxBodyBytes, "remote_addr", r.RemoteAddr)
			http.Error(rw, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		w.logger.WarnContext(ctx, "webhook: read body failed", "err", err)
		http.Error(rw, "read body", http.StatusBadRequest)
		return
	}
	signature := r.Header.Get("Stripe-Signature")

	stripeEvent, err := w.verifySignature(body, signature)
	if err != nil {
		// Rate-limit logs to prevent log-storm DoS: an attacker
		// spraying bad signatures would otherwise force one log line
		// per request. We log the first failure-per-bucket-window
		// and a running suppressed-count on the next emit.
		if dropped := w.sigFailLimiter.recordAndShouldLog(); dropped >= 0 {
			attrs := []any{
				"err", err,
				"signature_present", signature != "",
				"remote_addr", r.RemoteAddr,
			}
			if dropped > 0 {
				attrs = append(attrs, "suppressed_since_last_log", dropped)
			}
			w.logger.WarnContext(ctx, "webhook: signature verification failed", attrs...)
		}
		http.Error(rw, "signature verification failed", http.StatusBadRequest)
		return
	}

	evt := fromStripeEvent(&stripeEvent)

	// Dedup: try to claim the event id. If already claimed AND finished,
	// it's a duplicate delivery — return 200 silently.
	if cached, found, err := w.store.Get(ctx, evt.ID, nil); err != nil {
		w.logger.ErrorContext(ctx, "webhook: dedup store get failed",
			"err", err, "event_id", evt.ID)
		http.Error(rw, "dedup store unavailable", http.StatusInternalServerError)
		return
	} else if found {
		w.logger.DebugContext(ctx, "webhook: duplicate delivery suppressed",
			"event_id", evt.ID, "event_type", evt.Type)
		writeReplay(rw, cached)
		return
	}

	token, mismatch, ok, err := w.store.TryLock(ctx, evt.ID, nil, w.dedupTTL)
	if err != nil {
		w.logger.ErrorContext(ctx, "webhook: dedup TryLock failed",
			"err", err, "event_id", evt.ID)
		http.Error(rw, "dedup lock unavailable", http.StatusInternalServerError)
		return
	}
	if mismatch {
		// Same event id with a different body — should never happen for
		// real Stripe events. Treat as suspicious and refuse.
		w.logger.WarnContext(ctx, "webhook: event id reused with different body",
			"event_id", evt.ID)
		http.Error(rw, "event id reused", http.StatusBadRequest)
		return
	}
	if !ok {
		// Another worker is currently processing this event. Return
		// 200; the in-flight worker is responsible for completion.
		w.logger.DebugContext(ctx, "webhook: event already claimed by another worker",
			"event_id", evt.ID)
		rw.WriteHeader(http.StatusOK)
		return
	}

	ns, _ := extractAppNamespace(&stripeEvent)
	decision := w.classifyNamespace(&stripeEvent)
	switch decision {
	case filterSkip:
		w.logger.DebugContext(ctx, "webhook: event skipped (different app_namespace)",
			"event_id", evt.ID, "event_type", evt.Type)
		// Mark processed so a retry doesn't re-evaluate the same event.
		w.markProcessed(ctx, evt.ID, token)
		w.recordToLog(ctx, LoggedEvent{
			EventID: evt.ID, EventType: evt.Type, Status: "skipped",
			Payload: body, Namespace: ns,
		})
		rw.WriteHeader(http.StatusOK)
		return
	case filterUnrouted:
		// Route only to OnOtherEvent; typed handlers don't fire on
		// unstamped events (account-level events fall here).
		if err := runIfSet(ctx, evt, w.handlers.OnOtherEvent); err != nil {
			if unlockErr := w.store.Unlock(ctx, evt.ID, token); unlockErr != nil {
				w.logger.WarnContext(ctx, "webhook: unlock after dispatch failure also failed",
					"err", unlockErr, "event_id", evt.ID)
			}
			w.logger.ErrorContext(ctx, "webhook: OnOtherEvent returned error",
				"err", err, "event_id", evt.ID, "event_type", evt.Type)
			w.recordToLog(ctx, LoggedEvent{
				EventID: evt.ID, EventType: evt.Type, Status: "failed",
				LastError: err.Error(), AttemptCount: 1, Payload: body, Namespace: ns,
			})
			http.Error(rw, "handler failed", http.StatusInternalServerError)
			return
		}
		w.markProcessed(ctx, evt.ID, token)
		now := time.Now()
		w.recordToLog(ctx, LoggedEvent{
			EventID: evt.ID, EventType: evt.Type, Status: "processed",
			ProcessedAt: &now, Payload: body, Namespace: ns,
		})
		rw.WriteHeader(http.StatusOK)
		return
	}

	// Async dispatch path: hand off to the queue + return 200 now.
	// The queue's worker MUST call w.ProcessQueued(ctx, item) so the
	// dedup store is updated and replays are suppressed. Built-in
	// MemoryQueue does this when constructed with wh.ProcessQueued as
	// the dispatch callback.
	w.queueMu.RLock()
	queue := w.queue
	w.queueMu.RUnlock()
	if queue != nil {
		// Record a "claimed" log entry so observability tools (and
		// StuckEvents) can see the event was accepted even before
		// the worker runs.
		w.recordToLog(ctx, LoggedEvent{
			EventID: evt.ID, EventType: evt.Type, Status: "claimed",
			Payload: body, Namespace: ns,
		})
		if err := queue.Enqueue(ctx, QueueItem{
			EventID: evt.ID, EventType: evt.Type, Namespace: ns,
			Body: body, Token: token,
		}); err != nil {
			// Queue full / closed → fall through to sync dispatch so
			// we either succeed-and-mark-processed or surface 500 to
			// Stripe for retry. Either way the dedup store stays
			// consistent with what Stripe sees.
			w.logger.WarnContext(ctx, "webhook: enqueue failed; falling back to sync dispatch",
				"err", err, "event_id", evt.ID)
		} else {
			rw.WriteHeader(http.StatusOK)
			return
		}
	}

	dispatchCtx := ctx
	var cancelDispatch context.CancelFunc
	if w.syncDispatchTimeout > 0 {
		dispatchCtx, cancelDispatch = context.WithTimeout(ctx, w.syncDispatchTimeout)
		defer cancelDispatch()
	}
	dispatchErr := w.dispatchEvent(dispatchCtx, evt)
	if dispatchErr != nil {
		// EventLog: attempt count increments if a prior log entry
		// exists. A transient EventLog.Get failure would reset
		// attempt=1 and break the per-event-policy MaxAttempts
		// give-up logic — log loudly when that happens so operators
		// can see why give-up isn't firing.
		prev, _, getErr := w.eventLogGet(ctx, evt.ID)
		if getErr != nil {
			w.logger.WarnContext(ctx, "webhook: EventLog.Get failed; per-event-policy attempt count may be inaccurate this cycle",
				"err", getErr, "event_id", evt.ID)
		}
		attempt := prev.AttemptCount + 1

		// Per-event-type policy: when MaxAttempts is hit, defer to
		// OnExhausted (default: keep returning 500 so Stripe retries).
		if policy, ok := w.perEventPolicy[evt.Type]; ok && policy.MaxAttempts > 0 && attempt >= policy.MaxAttempts {
			w.logger.ErrorContext(ctx, "webhook: max attempts reached per policy",
				"event_id", evt.ID, "event_type", evt.Type, "attempts", attempt)
			w.recordToLog(ctx, LoggedEvent{
				EventID: evt.ID, EventType: evt.Type, Status: "failed",
				LastError: dispatchErr.Error(), AttemptCount: attempt, Payload: body, Namespace: ns,
			})
			var giveUpErr error
			if policy.OnExhausted != nil {
				giveUpErr = policy.OnExhausted(ctx, LoggedEvent{
					EventID: evt.ID, EventType: evt.Type, Status: "failed",
					LastError: dispatchErr.Error(), AttemptCount: attempt, Payload: body, Namespace: ns,
				})
			}
			if giveUpErr == nil {
				// Mark processed + return 200 so Stripe stops retrying.
				w.markProcessed(ctx, evt.ID, token)
				rw.WriteHeader(http.StatusOK)
				return
			}
			// OnExhausted returned an error → keep Stripe retrying.
		}

		// Release the lock so Stripe's next retry can claim and reprocess.
		if unlockErr := w.store.Unlock(ctx, evt.ID, token); unlockErr != nil {
			w.logger.WarnContext(ctx, "webhook: unlock after dispatch failure also failed",
				"err", unlockErr, "event_id", evt.ID)
		}
		w.logger.ErrorContext(ctx, "webhook: handler returned error",
			"err", dispatchErr, "event_id", evt.ID, "event_type", evt.Type)
		w.recordToLog(ctx, LoggedEvent{
			EventID: evt.ID, EventType: evt.Type, Status: "failed",
			LastError: dispatchErr.Error(), AttemptCount: attempt,
			Payload: body, Namespace: ns,
		})
		http.Error(rw, "handler failed", http.StatusInternalServerError)
		return
	}

	if err := w.store.Set(ctx, evt.ID, token, okResponse, w.dedupTTL); err != nil {
		// Processing succeeded; only the dedup mark failed. Log loudly
		// but still return 200 — we don't want Stripe to retry an
		// already-processed event.
		if !errors.Is(err, idempotency.ErrLockLost) {
			w.logger.ErrorContext(ctx, "webhook: failed to mark event processed",
				"err", err, "event_id", evt.ID)
		}
	}
	now := time.Now()
	w.recordToLog(ctx, LoggedEvent{
		EventID: evt.ID, EventType: evt.Type, Status: "processed",
		ProcessedAt: &now, Payload: body, Namespace: ns,
	})
	rw.WriteHeader(http.StatusOK)
}

// eventLogGet is a nil-safe helper so the dispatch path doesn't
// branch on w.eventLog repeatedly.
func (w *Webhooks) eventLogGet(ctx context.Context, eventID string) (LoggedEvent, bool, error) {
	if w.eventLog == nil {
		return LoggedEvent{}, false, nil
	}
	return w.eventLog.Get(ctx, eventID)
}

// markProcessed transitions the dedup store to "processed" for evt.ID
// and logs (without failing the HTTP response) if the store write
// fails. We don't return 500 on store-write-failure because the
// handler ALREADY succeeded — failing the response would re-deliver
// the event and re-run the handler.
//
// Operators seeing repeated "failed to mark event processed" warnings
// should investigate dedup-store health; under sustained failure the
// app will reprocess every event Stripe redelivers.
func (w *Webhooks) markProcessed(ctx context.Context, eventID, token string) {
	if err := w.store.Set(ctx, eventID, token, okResponse, w.dedupTTL); err != nil {
		if !errors.Is(err, idempotency.ErrLockLost) {
			w.logger.ErrorContext(ctx, "webhook: failed to mark event processed",
				"err", err, "event_id", eventID)
		}
	}
}

// TestSignatureVerification is a startup self-test that constructs a
// known-good payload and asserts the configured secret accepts it.
// Apps call this once during init; failure means the secret is wrong
// or middleware is mutating bytes.
func (w *Webhooks) TestSignatureVerification() error {
	body := []byte(fmt.Sprintf(
		`{"id":"evt_startup_test","object":"event","api_version":%q,"type":"ping"}`,
		stripe.APIVersion,
	))
	signed, err := signPayload(w.signingSecret, body, time.Now())
	if err != nil {
		return fmt.Errorf("test: sign payload: %w", err)
	}
	_, err = webhook.ConstructEventWithOptions(body, signed, w.signingSecret,
		webhook.ConstructEventOptions{
			Tolerance:                w.sigTolerance,
			IgnoreAPIVersionMismatch: !w.strictAPIVersion,
		},
	)
	if err != nil {
		return fmt.Errorf("test: signature verification failed: %w", err)
	}
	return nil
}

func writeReplay(rw http.ResponseWriter, cached *idempotency.CachedResponse) {
	if cached == nil {
		rw.WriteHeader(http.StatusOK)
		return
	}
	status := cached.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	rw.WriteHeader(status)
}

// --- Operational helpers (no-op when EventLog is not configured) ---
//
// IMPORTANT — AUTHZ: these methods return privileged data:
//
//   - StuckEvents reveals which Stripe events the app is failing to
//     process — useful operational signal but also enumerable
//     customer-impact information.
//
//   - RetrieveEvent returns the full event payload, which includes
//     customer PII (email, address, payment-method last4, etc.).
//
//   - ReplayEvent triggers handler re-execution — financial side
//     effects (subscription cancel, credit grant) can run.
//
// The lib does NOT add an authz check; apps that expose these via
// admin HTTP endpoints MUST gate them on operator authentication
// + audit-log the call. Do not wire them onto an unauthenticated
// public endpoint.

// StuckEvents returns events whose status isn't "processed" after
// minAttempts retries. Apps use this for ops dashboards / alerts on
// repeatedly-failing webhooks. PRIVILEGED — see authz note above.
func (w *Webhooks) StuckEvents(ctx context.Context, minAttempts int) ([]LoggedEvent, error) {
	if w.eventLog == nil {
		return nil, nil
	}
	return w.eventLog.StuckEvents(ctx, minAttempts)
}

// RetrieveEvent returns the LoggedEvent for an id, or (zero, false) if
// not logged. PRIVILEGED — the LoggedEvent.Body contains the raw
// Stripe payload including customer PII. See authz note above.
func (w *Webhooks) RetrieveEvent(ctx context.Context, eventID string) (LoggedEvent, bool, error) {
	if w.eventLog == nil {
		return LoggedEvent{}, false, nil
	}
	return w.eventLog.Get(ctx, eventID)
}

// ReplayEvent reconstructs a previously-logged event from the
// EventLog and runs it back through the typed-handler dispatch
// (bypassing signature verification + dedup, which already happened
// the first time). Useful for:
//
//   - Re-running handlers after a fix (handler had a bug; replay the
//     events it failed on).
//   - Manual recovery from per-event-policy "give up" decisions.
//
// Returns ErrEventNotInLog when the EventLog has no record of the id.
// Other errors come from the handler itself.
func (w *Webhooks) ReplayEvent(ctx context.Context, eventID string) error {
	if w.eventLog == nil {
		return ErrEventLogNotConfigured
	}
	logged, ok, err := w.eventLog.Get(ctx, eventID)
	if err != nil {
		return fmt.Errorf("webhooks.ReplayEvent: log get: %w", err)
	}
	if !ok {
		return ErrEventNotInLog
	}
	if len(logged.Payload) == 0 {
		return fmt.Errorf("webhooks.ReplayEvent: logged event %s has no payload", eventID)
	}
	var stripeEvent stripe.Event
	if err := json.Unmarshal(logged.Payload, &stripeEvent); err != nil {
		return fmt.Errorf("webhooks.ReplayEvent: unmarshal payload: %w", err)
	}
	evt := fromStripeEvent(&stripeEvent)
	return w.dispatchEvent(ctx, evt)
}

// ErrEventLogNotConfigured indicates the caller asked for an
// operation that requires EventLog while Config.EventLog was nil.
var ErrEventLogNotConfigured = errors.New("webhooks: EventLog not configured")

// ErrPermanentFailure marks an event that cannot succeed on retry —
// malformed body, schema-unparseable, or a handler that explicitly
// indicates "do not retry." Queue implementations check
// errors.Is(err, ErrPermanentFailure) and dead-letter / complete the
// item rather than releasing it back for another attempt. Without this
// sentinel, a malformed payload loops forever between queue and
// ProcessQueued, accumulating attempt_count without ever succeeding.
var ErrPermanentFailure = errors.New("webhooks: permanent failure (do not retry)")

// ErrEventNotInLog indicates the requested event id wasn't found in
// the EventLog (never received, or pruned).
var ErrEventNotInLog = errors.New("webhooks: event not in log")

// PruneOldEvents deletes EventLog rows older than `before`. Apps
// schedule this for retention. Returns 0 / nil when EventLog isn't
// configured.
func (w *Webhooks) PruneOldEvents(ctx context.Context, before time.Time) (int, error) {
	if w.eventLog == nil {
		return 0, nil
	}
	return w.eventLog.PruneOlderThan(ctx, before)
}

// StartAutoPrune spawns a goroutine that periodically prunes EventLog
// rows older than maxAge. Returns a stop func to halt the loop
// (typically registered as a connector shutdown hook).
//
// Stripe events carry PII (customer email, address, last-4); the
// EventLog persists payloads indefinitely without this. Apps storing
// the EventLog in Postgres should call this — without it, the lib's
// design retains customer PII forever, which is a GDPR liability.
//
// Pruned events become unrecoverable for ReplayEvent. Choose maxAge
// based on your replay window (90 days covers Stripe's 30-day retry
// + 60-day operator response budget). interval should be hourly to
// daily; sub-second intervals waste DB.
//
// Safe to call with EventLog==nil (no-op, returns a no-op stop func).
func (w *Webhooks) StartAutoPrune(maxAge, interval time.Duration) (stop func()) {
	if w.eventLog == nil || maxAge <= 0 || interval <= 0 {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Prune immediately on startup so a long-stopped instance
		// doesn't sit on stale data until the first tick fires.
		_, _ = w.PruneOldEvents(ctx, time.Now().Add(-maxAge))
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				n, err := w.PruneOldEvents(ctx, time.Now().Add(-maxAge))
				if err != nil {
					w.logger.WarnContext(ctx, "webhook: auto-prune failed",
						"err", err, "max_age", maxAge)
					continue
				}
				if n > 0 {
					w.logger.InfoContext(ctx, "webhook: auto-prune",
						"deleted_events", n, "max_age", maxAge)
				}
			}
		}
	}()
	return cancel
}

// recordToLog is the internal logging hook the dispatch path calls
// at every outcome (skipped / processed / failed). Safe with a nil
// EventLog (does nothing).
func (w *Webhooks) recordToLog(ctx context.Context, e LoggedEvent) {
	if w.eventLog == nil {
		return
	}
	if err := w.eventLog.Record(ctx, e); err != nil {
		w.logger.WarnContext(ctx, "webhook: EventLog.Record failed",
			"err", err, "event_id", e.EventID)
	}
}

// verifySignature tries the primary signing secret first, then any
// AdditionalSigningSecrets in order. Returns the parsed event on the
// first success, or the primary-secret error if all attempts fail.
//
// Secret rotation lets you deploy a new whsec_… and accept events
// signed with either the old or new key for the rotation window
// (Stripe takes minutes to switch endpoints over).
func (w *Webhooks) verifySignature(body []byte, signature string) (stripe.Event, error) {
	opts := webhook.ConstructEventOptions{
		Tolerance:                w.sigTolerance,
		IgnoreAPIVersionMismatch: !w.strictAPIVersion,
	}
	evt, primaryErr := webhook.ConstructEventWithOptions(body, signature, w.signingSecret, opts)
	if primaryErr == nil {
		return evt, nil
	}
	for _, alt := range w.additionalSecrets {
		if alt == "" {
			continue
		}
		evt, err := webhook.ConstructEventWithOptions(body, signature, alt, opts)
		if err == nil {
			return evt, nil
		}
	}
	return stripe.Event{}, primaryErr
}

// SetQueue attaches an async-dispatch Queue after construction. Use
// this to break the chicken-and-egg: build the Webhooks, then build
// a MemoryQueue whose worker dispatch is wh.ProcessQueued, then
// SetQueue(queue) to activate async mode for subsequent Handle calls.
//
// Calling SetQueue(nil) returns to synchronous dispatch.
func (w *Webhooks) SetQueue(q Queue) {
	w.queueMu.Lock()
	w.queue = q
	w.queueMu.Unlock()
}

// Register installs a handler for the given Stripe event type
// dynamically (in addition to typed Handlers fields). Use for event
// types the lib doesn't yet have typed slots for — e.g.
// "setup_intent.succeeded", "payment_method.attached",
// "quote.accepted" — without waiting for the next lib release.
//
// A registered handler takes precedence over the matching typed field
// when both are set. Calling Register again for the same event type
// replaces the previous registration. Pass fn=nil to unregister.
//
// Safe to call concurrently with Handle / dispatch.
func (w *Webhooks) Register(eventType string, fn func(context.Context, Event) error) {
	if eventType == "" {
		return
	}
	w.customMu.Lock()
	defer w.customMu.Unlock()
	if fn == nil {
		delete(w.customHandlers, eventType)
		return
	}
	if w.customHandlers == nil {
		w.customHandlers = make(map[string]func(context.Context, Event) error)
	}
	w.customHandlers[eventType] = fn
}

// lookupCustomHandler returns the registered handler for eventType,
// or nil. Read-locked so dispatch can run concurrently with Register.
func (w *Webhooks) lookupCustomHandler(eventType string) func(context.Context, Event) error {
	w.customMu.RLock()
	defer w.customMu.RUnlock()
	return w.customHandlers[eventType]
}

// QueueForStats returns the currently-wired Queue (or nil) so observability
// code can downcast to a depth-reporting implementation. Apps shouldn't
// use this for dispatch — only for read-only depth / capacity queries.
func (w *Webhooks) QueueForStats() Queue {
	w.queueMu.RLock()
	defer w.queueMu.RUnlock()
	return w.queue
}

// ProcessQueued is the dispatch entry point a Queue's worker calls
// for each enqueued QueueItem. It reconstructs the event from the
// raw body, runs the typed-handler dispatch, and updates the dedup
// store + EventLog with the final outcome.
//
// Returns nil on success (and on per-event-policy give-up) or the
// handler error otherwise. Queue implementations may surface the
// error for their own retry / dead-letter logic; the dedup store is
// already updated by the time ProcessQueued returns, so a Queue retry
// will NOT cause double-handling.
//
// CRITICAL: every Queue worker MUST call ProcessQueued exactly once
// per QueueItem. Calling it twice does no harm (idempotent via the
// store) but skipping it leaves the event claim outstanding until
// the dedup TTL expires, at which point Stripe redelivery would
// reprocess. Using MemoryQueue constructed with wh.ProcessQueued
// as the dispatch satisfies this automatically.
func (w *Webhooks) ProcessQueued(ctx context.Context, item QueueItem) error {
	// Body was signature-verified at receive time (in Handle, before
	// the enqueue); the worker just needs the JSON. We deliberately
	// don't re-run webhook.ConstructEventWithOptions here because the
	// QueueItem doesn't carry the original Stripe-Signature header
	// (durable queues typically wouldn't persist it).
	var stripeEvent stripe.Event
	if err := json.Unmarshal(item.Body, &stripeEvent); err != nil {
		// Malformed bytes in the queue can never succeed on retry.
		// Mark processed so the dedup-store record reflects the failure;
		// if the store write itself fails we log + still surface the
		// permanent-failure sentinel so the Queue can dead-letter the
		// item rather than re-queue it indefinitely.
		w.logger.ErrorContext(ctx, "webhook: ProcessQueued parse failed",
			"err", err, "event_id", item.EventID)
		if markErr := w.markQueuedProcessed(ctx, item, "failed", err.Error(), 1); markErr != nil {
			w.logger.ErrorContext(ctx, "webhook: ProcessQueued markFailed write failed (dedup store unreachable)",
				"event_id", item.EventID, "err", markErr)
		}
		return fmt.Errorf("%w: parse event body: %w", ErrPermanentFailure, err)
	}
	evt := fromStripeEvent(&stripeEvent)
	if err := w.dispatchEvent(ctx, evt); err != nil {
		return w.handleQueuedFailure(ctx, item, err)
	}
	return w.markQueuedProcessed(ctx, item, "processed", "", 1)
}

// handleQueuedFailure handles the per-event policy + EventLog flow for
// a queued event that failed dispatch. Mirrors the sync-mode policy
// branch in Handle.
func (w *Webhooks) handleQueuedFailure(ctx context.Context, item QueueItem, dispatchErr error) error {
	prev, _, _ := w.eventLogGet(ctx, item.EventID)
	attempt := prev.AttemptCount + 1

	policy, hasPolicy := w.perEventPolicy[item.EventType]
	if hasPolicy && policy.MaxAttempts > 0 && attempt >= policy.MaxAttempts {
		w.logger.ErrorContext(ctx, "webhook: async max attempts reached per policy",
			"event_id", item.EventID, "event_type", item.EventType, "attempts", attempt)
		lg := LoggedEvent{
			EventID: item.EventID, EventType: item.EventType, Status: "failed",
			LastError: dispatchErr.Error(), AttemptCount: attempt,
			Payload: item.Body, Namespace: item.Namespace,
		}
		var giveUpErr error
		if policy.OnExhausted != nil {
			giveUpErr = policy.OnExhausted(ctx, lg)
		}
		if giveUpErr == nil {
			// Permanently mark processed so a re-enqueue is a no-op.
			_ = w.markQueuedProcessed(ctx, item, "failed", dispatchErr.Error(), attempt)
			return nil
		}
		// OnExhausted said keep trying. Release lock so queue can re-enqueue.
		_ = w.store.Unlock(ctx, item.EventID, item.Token)
		w.recordToLog(ctx, lg)
		return dispatchErr
	}

	// No policy exhaustion → release lock so a re-enqueue can re-claim.
	if unlockErr := w.store.Unlock(ctx, item.EventID, item.Token); unlockErr != nil {
		w.logger.WarnContext(ctx, "webhook: async unlock after dispatch failure also failed",
			"err", unlockErr, "event_id", item.EventID)
	}
	w.logger.ErrorContext(ctx, "webhook: async handler returned error",
		"err", dispatchErr, "event_id", item.EventID, "event_type", item.EventType)
	w.recordToLog(ctx, LoggedEvent{
		EventID: item.EventID, EventType: item.EventType, Status: "failed",
		LastError: dispatchErr.Error(), AttemptCount: attempt,
		Payload: item.Body, Namespace: item.Namespace,
	})
	return dispatchErr
}

// markQueuedProcessed transitions the dedup store + event log to a
// terminal state. Idempotent.
func (w *Webhooks) markQueuedProcessed(ctx context.Context, item QueueItem, status, lastErr string, attempt int) error {
	if err := w.store.Set(ctx, item.EventID, item.Token, okResponse, w.dedupTTL); err != nil {
		if !errors.Is(err, idempotency.ErrLockLost) {
			w.logger.ErrorContext(ctx, "webhook: async failed to mark event processed",
				"err", err, "event_id", item.EventID)
		}
	}
	now := time.Now()
	lg := LoggedEvent{
		EventID: item.EventID, EventType: item.EventType, Status: status,
		Payload: item.Body, Namespace: item.Namespace,
		AttemptCount: attempt,
	}
	if status == "processed" {
		lg.ProcessedAt = &now
	}
	if lastErr != "" {
		lg.LastError = lastErr
	}
	w.recordToLog(ctx, lg)
	return nil
}
