# Replaying a webhook event

You deployed a broken handler; events failed; you fixed the bug.
Replay the failed events to provision the customers correctly.

## Requirement: EventLog must be configured

The replay path reads from `webhooks.EventLog`. Without it, events
aren't persisted past the dedup store's TTL.

```go
cfg := connector.Config{
    WebhookEventLog: pgrepo.NewEventLog(db),  // or webhooks.NewMemoryLog() for dev
    // ...
}
```

## CLI

```bash
rho-stripe webhook replay evt_1Abc…
```

That's the simplest path. The CLI bypasses signature verification +
dedup (the original delivery already passed those) and runs the
event through `Handlers.dispatch`.

`Options.EventLogFactory` + `HandlersFactory` must be wired in your
`cli.Run` setup — typically these point at the same EventLog and
Handlers your production server uses, so the CLI binary can replay
against the same code paths.

## Programmatic

```go
err := conn.Webhooks.ReplayEvent(ctx, "evt_1Abc…")
switch {
case errors.Is(err, webhooks.ErrEventLogNotConfigured):
    // wire EventLog
case errors.Is(err, webhooks.ErrEventNotInLog):
    // event not in log (never received, or pruned)
case err != nil:
    // handler returned an error — fix bug, replay again
}
```

## Finding events to replay

```go
stuck, _ := conn.Webhooks.StuckEvents(ctx, 3) // events that failed >= 3 attempts
for _, e := range stuck {
    fmt.Printf("evt=%s type=%s last_error=%s\n", e.EventID, e.EventType, e.LastError)
}
```

Or query your `EventLog` directly (Postgres-backed log exposes raw SQL access).

## Caveats

- **Replay does NOT dedupe via Store.** Replaying the same id twice
  runs the handler twice. Handlers should be idempotent (they
  usually are because of the same constraint at first-delivery time).
- **`ReplayEvent` uses your current handler chain.** If your fix was
  deployed correctly, the bug-fix handler runs; if you replayed
  against an OLDER deploy you'd re-run the buggy handler.
- **Replay returns the handler's error.** Stripe doesn't get notified
  about replay failures — that's between you and your logs.
