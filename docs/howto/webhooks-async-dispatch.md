# Turn on async webhook dispatch

By default the lib runs handlers synchronously — Stripe POSTs the
event, the handler runs inside `Handle`, the response goes out. Fine
for low volume.

For high-volume endpoints (or slow handlers) Stripe's 10-second
timeout becomes a problem. Async dispatch returns 200 to Stripe
immediately after the event is verified + claimed, and runs the
handler on a worker.

## In-process (single replica)

```go
conn, err := connector.New(ctx, connector.Config{
    // ...
    AsyncWebhooks: &connector.AsyncWebhookConfig{
        Capacity: 256,  // channel buffer
        Workers:  4,    // goroutines draining the channel
    },
})
defer conn.Shutdown(ctx) // drains in-flight workers
```

That's it. Internally the connector builds a `MemoryQueue` whose
worker calls `Webhooks.ProcessQueued` (which updates the dedup store
on success/failure — Stripe redelivery is safe).

## Multi-process (multiple replicas)

`MemoryQueue` is in-process only. For multi-replica apps, use the
Postgres-backed queue from `repos/postgres`:

```go
import pgrepo "github.com/bds421/rho-stripe/repos/postgres"

q := pgrepo.NewWebhookQueue(db, pgrepo.WebhookQueueOptions{
    Workers:      4,
    PollInterval: time.Second,
    StuckAfter:   5 * time.Minute,
})

conn, err := connector.New(ctx, connector.Config{
    // (omit AsyncWebhooks so the connector doesn't build an in-memory queue)
    // ...
})
q.Start(conn.Webhooks.ProcessQueued)
defer q.Shutdown(ctx)
conn.Webhooks.SetQueue(q)
```

Multiple processes can `Start()` the same queue table; they compete
safely via `SELECT … FOR UPDATE SKIP LOCKED`. Crashed workers' rows
are released by the built-in sweeper after `StuckAfter`.

## Per-event give-up policy

When you want certain event types to stop retrying after N attempts:

```go
WebhookPerEventPolicy: map[string]webhooks.EventPolicy{
    "invoice.payment_failed": {
        MaxAttempts: 5,
        OnExhausted: func(_ context.Context, evt webhooks.LoggedEvent) error {
            // Move to a dead-letter queue / page the team / etc.
            alert.Page("invoice.payment_failed exhausted: %s", evt.EventID)
            return nil // return nil → ack with 200, Stripe stops retrying
            // return err → keep returning 500 so Stripe retries indefinitely
        },
    },
},
```

Requires `WebhookEventLog` to be configured (so attempt counts persist
across deliveries).

## Replay a stuck event

Once `EventLog` is on, `webhook replay` re-runs an event through the
typed-handler chain, bypassing signature verification and dedup:

```bash
rho-stripe webhook replay evt_1Abc…
```

Useful after deploying a handler fix: replay the events the broken
handler errored on.
