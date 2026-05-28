# Observability — tracing + metrics

rho-stripe ships with two optional observability packages:

- `observability/otel` — OpenTelemetry tracing wrappers
- `observability/metrics` — Prometheus metrics collector

Both are **opt-in** and **dependency-free for non-users** (the OTel
API is already a transitive dep; the Prometheus dep is only pulled
when you import the metrics package).

## OpenTelemetry tracing

```go
import (
    scotel "github.com/bds421/rho-stripe/observability/otel"
    "go.opentelemetry.io/otel"
)

tracer := otel.Tracer("myapp.stripe")

// Wrap the webhook handler so each event POST gets a child span:
mux.HandleFunc("/webhook", scotel.WrapWebhookHandler(conn.Webhooks, tracer))

// For async dispatch, wrap the worker dispatch so the worker side
// shows up as its own span:
q := webhooks.NewMemoryQueue(256, 4, scotel.WrapProcessQueued(conn.Webhooks.ProcessQueued, tracer))
conn.Webhooks.SetQueue(q)
```

What gets traced:

| Span | Attributes |
|---|---|
| `stripe.webhook.handle` | `http.method`, `http.target`, `http.status_code` |
| `stripe.webhook.enqueue` | `stripe.event.id`, `stripe.event.type`, `stripe.app_namespace` |
| `stripe.webhook.dispatch` | same |

Tracer comes from your app's TracerProvider — the lib doesn't pick
one. Use the standard OTel SDK + your exporter of choice (OTLP, Jaeger,
Honeycomb, …).

## Prometheus metrics

```go
import (
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promhttp"
    scmetrics "github.com/bds421/rho-stripe/observability/metrics"
)

reg := prometheus.NewRegistry()
m := scmetrics.New(reg)

// Wrap the webhook handler for HTTP-level metrics:
mux.HandleFunc("/webhook", m.WrapWebhookHandler(conn.Webhooks))

// Wrap async dispatch for worker-side metrics:
q := webhooks.NewMemoryQueue(256, 4, m.WrapProcessQueued(conn.Webhooks.ProcessQueued))
conn.Webhooks.SetQueue(q)

// Expose for Prometheus to scrape:
mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
```

Metrics exposed:

| Name | Type | Labels | Meaning |
|---|---|---|---|
| `stripe_connector_webhook_handle_seconds` | Histogram | `event_type`, `status` | latency of `Webhooks.Handle` |
| `stripe_connector_webhook_requests_total` | Counter | `event_type`, `status` | total `Handle` invocations |
| `stripe_connector_queue_dispatch_seconds` | Histogram | `event_type`, `outcome` | async dispatch latency |
| `stripe_connector_queue_dispatch_errors_total` | Counter | `event_type` | dispatch errors |
| `stripe_connector_queue_depth` | Gauge | `queue` | set via `m.SetQueueDepth(...)` |

Apps that want per-event-type metrics on `Handle` should set the
labels inside their own typed handler (the wrapper only sees the
opaque HTTP boundary — it can't know the event type without
re-parsing the body).

For `queue_depth`: rho-stripe doesn't push the gauge itself.
Schedule a sampler:

```go
go func() {
    t := time.NewTicker(time.Minute)
    for range t.C {
        n := countPgQueueRows(db)  // SELECT COUNT(*) FROM stripe_connector_webhook_queue
        m.SetQueueDepth("default", float64(n))
    }
}()
```

## Combining

You can use both wrappers together — order doesn't matter
(both are pass-through, only adding their own instrumentation):

```go
handler := scotel.WrapWebhookHandler(conn.Webhooks, tracer)
handler = m.WrapWebhookHandler(/* but it takes Webhooks, not handler */)
```

Wait — both take a `*webhooks.Webhooks`. So one wraps the other's
output:

```go
// Compose by chaining at the http.HandlerFunc layer:
mux.HandleFunc("/webhook", scotel.WrapWebhookHandler(conn.Webhooks, tracer))
// (metrics happen inside the OTel-wrapped Handle, since both wrap Handle
//  directly. Choose ONE wrapper if you want non-duplicated instrumentation,
//  OR write a thin app-side wrapper that calls both.)
```

In practice, most apps pick OTel (which gives you spans + can derive
metrics from spans via the OTel collector) OR Prometheus, not both.
If you do want both, write an app-side `mux.HandleFunc` that calls
both wrappers' instrumentation around `wh.Handle`.

## Logs

The lib's structured logs go through `slog.Logger` (`Config.Logger`).
Wire your own handler:

```go
logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
cfg := connector.Config{Logger: logger, /* … */}
```

Every lifecycle event the lib emits is labelled (`webhook: …`,
`drift-check: …`, `catalog drift check failed`, etc.) — grep-friendly.
