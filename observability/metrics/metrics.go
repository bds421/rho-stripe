// Package metrics provides Prometheus metrics for rho-stripe.
//
// Design:
//   - Apps own the prometheus.Registerer. The lib provides a Collector
//     that registers a fixed set of metrics on whatever registry the
//     app passes in.
//   - Metrics are wrapper-based: app constructs a Collector,
//     wraps Webhooks/Queue/etc., and reads metrics via the standard
//     promhttp.Handler on their /metrics endpoint.
//   - All metric names are namespaced under "stripe_connector_…" so
//     they're easy to find in dashboards.
//
// Wire:
//
//	reg := prometheus.NewRegistry()  // or use prometheus.DefaultRegisterer
//	m := metrics.New(reg)
//	mux.HandleFunc("/webhook", m.WrapWebhookHandler(conn.Webhooks))
//	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
package metrics

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/bds421/rho-stripe/webhooks"
	"github.com/prometheus/client_golang/prometheus"
)

// Collector registers rho-stripe metrics on a prometheus.Registerer
// and exposes wrappers that record them.
type Collector struct {
	webhookDuration  *prometheus.HistogramVec
	webhookCount     *prometheus.CounterVec
	queueDispatch    *prometheus.HistogramVec
	queueDispatchErr *prometheus.CounterVec
	queueDepth       *prometheus.GaugeVec

	stripeOutboundDuration *prometheus.HistogramVec
	stripeOutboundCount    *prometheus.CounterVec
	stripeOutboundInFlight *prometheus.GaugeVec
	stripeOutboundRetries  *prometheus.CounterVec

	queueEnqueueBlocked  *prometheus.CounterVec
	syncDispatchDuration *prometheus.HistogramVec
}

// New registers metrics on reg and returns a Collector ready to wrap
// the relevant Stripe-connector surfaces.
//
// Pass prometheus.DefaultRegisterer to use the global registry; pass
// a per-app prometheus.NewRegistry() to keep stripe metrics scoped.
func New(reg prometheus.Registerer) *Collector {
	c := &Collector{
		webhookDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "stripe_connector_webhook_handle_seconds",
			Help:    "Latency of Webhooks.Handle, by Stripe event type and outcome (status code).",
			Buckets: prometheus.ExponentialBuckets(0.001, 4, 8), // 1ms → 16s
		}, []string{"event_type", "status"}),

		webhookCount: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "stripe_connector_webhook_requests_total",
			Help: "Webhook HTTP requests, by Stripe event type and outcome.",
		}, []string{"event_type", "status"}),

		queueDispatch: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "stripe_connector_queue_dispatch_seconds",
			Help:    "Latency of async webhook handler dispatch (excludes time spent waiting in queue).",
			Buckets: prometheus.ExponentialBuckets(0.001, 4, 8),
		}, []string{"event_type", "outcome"}),

		queueDispatchErr: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "stripe_connector_queue_dispatch_errors_total",
			Help: "Async queue dispatch errors, by Stripe event type.",
		}, []string{"event_type"}),

		queueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "stripe_connector_queue_depth",
			Help: "Current depth of the async webhook queue (exposed by Queue impls that report).",
		}, []string{"queue"}),
	}
	c.queueEnqueueBlocked = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "stripe_connector_queue_enqueue_blocked_total",
		Help: "Webhook enqueues that would have blocked because the in-memory queue was full. Each increment is a sign of saturation; alert when rate > 0.",
	}, []string{"queue"})

	c.syncDispatchDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "stripe_connector_sync_dispatch_seconds",
		Help:    "Latency of synchronous (non-queued) webhook handler dispatch, by Stripe event type. Long tails here indicate handlers approaching Stripe's 30s ACK timeout.",
		Buckets: prometheus.ExponentialBuckets(0.001, 4, 8),
	}, []string{"event_type", "outcome"})

	c.stripeOutboundDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "stripe_connector_outbound_request_seconds",
		Help:    "Latency of outbound HTTPS calls to Stripe (api.stripe.com), by API path family and status code.",
		Buckets: prometheus.ExponentialBuckets(0.005, 4, 8), // 5ms → 80s
	}, []string{"path", "method", "status"})
	c.stripeOutboundCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "stripe_connector_outbound_requests_total",
		Help: "Outbound calls to Stripe, by API path family + status.",
	}, []string{"path", "method", "status"})
	c.stripeOutboundInFlight = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "stripe_connector_outbound_in_flight",
		Help: "Outbound Stripe calls currently in flight.",
	}, []string{"path"})
	c.stripeOutboundRetries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "stripe_connector_outbound_retries_total",
		Help: "Outbound calls that were retried (5xx + 429 + network errors). Each retry attempt counts; a single logical call may add multiple here.",
	}, []string{"path"})

	reg.MustRegister(
		c.webhookDuration, c.webhookCount,
		c.queueDispatch, c.queueDispatchErr, c.queueDepth,
		c.stripeOutboundDuration, c.stripeOutboundCount,
		c.stripeOutboundInFlight, c.stripeOutboundRetries,
		c.queueEnqueueBlocked, c.syncDispatchDuration,
	)
	return c
}

// RecordQueueEnqueueBlocked increments the saturation counter. Apps
// wrapping a webhooks.Queue can call this from their own Enqueue
// implementation when they detect blocking; the bundled MemoryQueue
// doesn't expose blocking-vs-non-blocking distinctly so this is for
// custom Queue implementations.
func (c *Collector) RecordQueueEnqueueBlocked(queue string) {
	c.queueEnqueueBlocked.WithLabelValues(queue).Inc()
}

// ObserveSyncDispatch records a single synchronous dispatch latency
// + outcome. Apps wrap their Handlers via this to expose per-event-type
// duration metrics for sync dispatch (the existing WrapProcessQueued
// covers async).
func (c *Collector) ObserveSyncDispatch(eventType, outcome string, dur time.Duration) {
	c.syncDispatchDuration.WithLabelValues(eventType, outcome).Observe(dur.Seconds())
}

// StripeRoundTripper returns an http.RoundTripper that records outbound
// Stripe metrics. Wrap your *http.Client's Transport with it before
// passing the client to stripeapi.NewClient (or pass via stripeapi
// extension).
//
// Path label is the first two segments of the URL path (e.g.
// "/v1/customers", "/v1/payment_intents") so cardinality stays low.
// Retries are inferred from the "Stripe-Retry-Attempt" header
// stripe-go adds on retried requests.
func (c *Collector) StripeRoundTripper(next http.RoundTripper) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	return &stripeRoundTripper{next: next, c: c}
}

type stripeRoundTripper struct {
	next http.RoundTripper
	c    *Collector
}

func (rt *stripeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	path := stripeAPIPathFamily(req.URL.Path)
	rt.c.stripeOutboundInFlight.WithLabelValues(path).Inc()
	defer rt.c.stripeOutboundInFlight.WithLabelValues(path).Dec()

	// Stripe-go sets Stripe-Retry-Attempt: N (N>=1) on retried requests.
	if req.Header.Get("Stripe-Retry-Attempt") != "" {
		rt.c.stripeOutboundRetries.WithLabelValues(path).Inc()
	}

	start := time.Now()
	resp, err := rt.next.RoundTrip(req)
	dur := time.Since(start).Seconds()

	status := "error"
	if resp != nil {
		status = strconv.Itoa(resp.StatusCode)
	}
	rt.c.stripeOutboundDuration.WithLabelValues(path, req.Method, status).Observe(dur)
	rt.c.stripeOutboundCount.WithLabelValues(path, req.Method, status).Inc()
	return resp, err
}

// stripeAPIPathFamily returns the first two URL path segments. Trims
// resource ids from the third position onward so a million unique
// "/v1/customers/cus_..." calls all label as "/v1/customers" rather
// than blowing up metric cardinality.
func stripeAPIPathFamily(p string) string {
	count := 0
	for i := 1; i < len(p); i++ {
		if p[i] == '/' {
			count++
			if count == 2 {
				return p[:i]
			}
		}
	}
	return p
}

// WrapWebhookHandler returns an http.HandlerFunc that records
// stripe_connector_webhook_handle_seconds and …_requests_total by
// event type + status.
//
// Note: event_type is not known until the body is signature-verified
// and parsed (which is what Webhooks.Handle does), so the wrapper
// can't label by type for failed-signature requests. Those land as
// event_type="unknown".
func (c *Collector) WrapWebhookHandler(wh *webhooks.Webhooks) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusCapturingWriter{ResponseWriter: w, status: http.StatusOK}
		wh.Handle(rw, r)
		dur := time.Since(start).Seconds()
		// We can't know the event type without re-parsing the body,
		// so we record under "unknown". Apps that want per-type
		// metrics should wrap the per-event handler inside Handlers,
		// where the type is already known.
		c.webhookDuration.WithLabelValues("unknown", strconv.Itoa(rw.status)).Observe(dur)
		c.webhookCount.WithLabelValues("unknown", strconv.Itoa(rw.status)).Inc()
	}
}

// WrapProcessQueued wraps a Webhooks.ProcessQueued-compatible
// function so each dispatch records its duration + outcome.
// Use when constructing a Queue.
func (c *Collector) WrapProcessQueued(dispatch func(ctx context.Context, item webhooks.QueueItem) error) func(ctx context.Context, item webhooks.QueueItem) error {
	return func(ctx context.Context, item webhooks.QueueItem) error {
		start := time.Now()
		err := dispatch(ctx, item)
		outcome := "ok"
		if err != nil {
			outcome = "error"
			c.queueDispatchErr.WithLabelValues(item.EventType).Inc()
		}
		c.queueDispatch.WithLabelValues(item.EventType, outcome).Observe(time.Since(start).Seconds())
		return err
	}
}

// SetQueueDepth lets apps push a queue-depth gauge (typically from a
// scheduled sampler: SELECT COUNT(*) FROM stripe_connector_webhook_queue
// once per minute → m.SetQueueDepth("default", n)).
func (c *Collector) SetQueueDepth(queue string, depth float64) {
	c.queueDepth.WithLabelValues(queue).Set(depth)
}

type statusCapturingWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusCapturingWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusCapturingWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
		if w.status == 0 {
			w.status = http.StatusOK
		}
	}
	return w.ResponseWriter.Write(b)
}
