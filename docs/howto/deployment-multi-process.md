# Multi-process deployment

The defaults work for single-process apps. Two things to swap for
multi-replica setups:

## 1. Use the Postgres webhook queue

`MemoryQueue` (the connector-built default) is in-process — workers
can't share work across replicas. Replace it with `postgres.NewWebhookQueue`:

```go
import (
    pgrepo "github.com/bds421/rho-stripe/repos/postgres"
    "github.com/bds421/rho-stripe/repos/postgres/schema"
)

// 1. Apply the schema (use your migration tool or just exec the strings)
for _, ddl := range schema.All() {
    if _, err := db.Exec(ddl); err != nil { log.Fatal(err) }
}

// 2. Wire the queue (NOTE: don't pass connector.Config.AsyncWebhooks,
//    or the connector will also build an in-memory queue and the
//    durable one won't get used).
q := pgrepo.NewWebhookQueue(db, pgrepo.WebhookQueueOptions{
    Workers:      4,
    PollInterval: 1 * time.Second,
    StuckAfter:   5 * time.Minute,
})

conn, err := connector.New(ctx, connector.Config{
    SecretKey:     os.Getenv("STRIPE_SECRET_KEY"),
    WebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"),
    // ... rest of config — leave AsyncWebhooks nil
})
q.Start(conn.Webhooks.ProcessQueued)
defer q.Shutdown(ctx)
conn.Webhooks.SetQueue(q)
```

Every replica runs this exact code. Stripe's POST goes to whatever
load-balancer slot is free; that process enqueues the row; whichever
replica's worker grabs it first (via `SELECT FOR UPDATE SKIP LOCKED`)
processes it. Crashed workers' rows are recovered by the sweeper
after `StuckAfter`.

## 2. Use Postgres repos for everything else

`MemoryRepo` (for credits, subscriptions, customers, idempotency) is
not shared across replicas. Use the Postgres impls:

```go
conn, err := connector.New(ctx, connector.Config{
    Customers:     pgrepo.NewCustomerRepo(db),
    Events:        pgstore.NewStore(db, "myapp_events"), // rho-kit idempotency
    Subscriptions: pgrepo.NewSubscriptionRepo(db),
    Credits:       pgrepo.NewCreditRepo(db),
    InvoiceNumberRepo: pgrepo.NewInvoiceNumberRepo(db),
    // ...
})
```

All five Postgres impls are race-tested with `pgadvisory` locking
where shared mutation matters (CreditRepo's deduction path, the
webhook queue's claim path).

## 3. Drift detector

Pick one replica to run drift detection (otherwise N replicas all
ping Stripe every interval). Simplest: gate by hostname/env:

```go
if os.Getenv("DRIFT_DETECTOR_ENABLED") == "true" {
    cfg.DriftDetector = &connector.DriftDetectorConfig{Interval: 15*time.Minute, OnReport: alertSlack}
}
```

Set the env var on exactly one replica (the "leader") via your
deployment tool.

## Graceful shutdown

`conn.Shutdown(ctx)` drains:
- the async queue (waits for in-flight workers)
- the drift detector

Call it from your `SIGTERM` handler before exiting:

```go
go func() {
    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
    <-sigCh
    shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    _ = conn.Shutdown(shutCtx)
    _ = srv.Shutdown(shutCtx)
}()
```

See `cmd/example-webhook/main.go` for the full pattern.
