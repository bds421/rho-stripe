# Health checks (`/healthz`, `/readyz`)

`conn.HealthHandler()` returns an `http.Handler` that emits a JSON
snapshot suitable for Kubernetes-style readiness probes.

```go
mux := http.NewServeMux()
mux.HandleFunc("POST /webhook", conn.Webhooks.Handle)
mux.Handle("GET /readyz", conn.HealthHandler())
mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
    w.WriteHeader(http.StatusOK) // process-alive only; cheaper than /readyz
})
```

## What gets checked

```json
{
  "ok": true,
  "uptime_seconds": 1834.2,
  "catalog_warmed": true,
  "stripe_reachable": true,
  "stripe_error": "",
  "last_stripe_check": "2026-05-27T10:42:11Z",
  "checked_at": "2026-05-27T11:13:33Z"
}
```

- `catalog_warmed` — `conn.Catalog.Warmed()`. False until `connector.New`
  succeeds in warming the lookup-key cache.
- `stripe_reachable` — result of the last `/v1/account` probe.
- HTTP status — 200 when `ok=true`, 503 otherwise.

## Probe cache

The Stripe probe is cached: at most one `/v1/account` call per
**30 seconds**, regardless of incoming probe rate. A Kubernetes
1Hz `/readyz` probe burns one Stripe call per 30s, not 30.

## liveness vs readiness

| Probe | Purpose | Use `HealthHandler`? |
|---|---|---|
| `/healthz` (liveness) | Process is alive — restart if not | No, return 200 unconditionally |
| `/readyz` (readiness) | Process can serve traffic right now | Yes |

If `/readyz` returns 503 the load balancer should pull the pod from
rotation without restarting it. `/healthz` failure restarts the
pod — restart loops on a Stripe outage are the wrong response.

## Tests with deterministic time

The probe-cache TTL can be tested without sleeping by injecting
a fake clock:

```go
fc := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
conn.SetHealthClock(fc)
// ...
fc.Advance(31 * time.Second)
// next probe will actually re-call Stripe
```
