# Outbound Stripe metrics

The `observability/metrics` Collector exposes both inbound webhook
metrics (slice 48) and outbound Stripe API call metrics (slice 51).
This page covers the outbound side.

## Wire

```go
reg := prometheus.NewRegistry()
m := metrics.New(reg)

sc := stripeapi.NewClient(stripeapi.Config{
    SecretKey:              os.Getenv("STRIPE_SECRET_KEY"),
    RoundTripperMiddleware: m.StripeRoundTripper,   // ← here
})

mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
```

That's it — every Stripe call now records latency, status, in-flight
count, and retry attempts.

## Metrics emitted

| Metric | Type | Labels | What |
|---|---|---|---|
| `stripe_connector_outbound_request_seconds` | Histogram | `path`, `method`, `status` | Latency of outbound calls |
| `stripe_connector_outbound_requests_total` | Counter | `path`, `method`, `status` | Outbound call count |
| `stripe_connector_outbound_in_flight` | Gauge | `path` | In-flight outbound calls |
| `stripe_connector_outbound_retries_total` | Counter | `path` | Retry attempts (stripe-go set `Stripe-Retry-Attempt` header) |

## Label cardinality

`path` is the first two URL segments: `/v1/customers`,
`/v1/payment_intents`, `/v1/invoices`, etc. Resource IDs are
trimmed off so you don't get one label per customer (which would
melt Prometheus).

`status` is the literal HTTP status code as a string (`200`, `429`,
`500`, `error` for transport-level failures).

## Useful PromQL

```promql
# p99 latency by Stripe endpoint
histogram_quantile(0.99,
  rate(stripe_connector_outbound_request_seconds_bucket[5m]))
  by (path, le)

# Error rate per endpoint
sum(rate(stripe_connector_outbound_requests_total{status=~"5.."}[5m])) by (path)
/
sum(rate(stripe_connector_outbound_requests_total[5m])) by (path)

# Retry pressure (= Stripe degraded for us)
sum(rate(stripe_connector_outbound_retries_total[5m]))

# Saturating any endpoint
max(stripe_connector_outbound_in_flight) by (path)
```

## Alerting

Recommended starting alerts:

- `stripe_connector_outbound_requests_total{status="429"}` rising —
  approaching Stripe's rate limit. Reduce call volume or request
  a limit increase from Stripe.
- p99 latency on `/v1/checkout/sessions` > 2s for 5min — checkout
  page-load is suffering.
- `outbound_retries_total` rate > 1/s sustained — Stripe is degraded,
  consider degrading non-critical flows.

## Inbound + outbound: complete picture

Combine with the inbound webhook metrics for the full request lifecycle:

```
inbound webhook  →  stripe_connector_webhook_handle_seconds  (apps perspective)
                                ↓
                       (your handler runs)
                                ↓
outbound API call  →  stripe_connector_outbound_request_seconds  (downstream cost)
```
