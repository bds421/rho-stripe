# Pausing + resuming subscriptions

"Snooze billing for a month without canceling." Stripe's
`pause_collection` field; wrapped via Pause + Resume.

## Pause

```go
err := conn.Subscriptions.Pause(ctx, "sub_…", subscriptions.PauseOptions{
    Behavior: "void", // or "keep_as_draft", "mark_uncollectible"
})
```

What each behavior does during the pause:

| Behavior | Invoices generated? | When to use |
|---|---|---|
| `keep_as_draft` (default) | Yes, as drafts | You want to review them after resume |
| `mark_uncollectible` | Yes, marked uncollectible | You want them on the books but not collected |
| `void` | No | Cleanest "skip the next N invoices" UX |

## Schedule auto-resume

```go
err := conn.Subscriptions.Pause(ctx, "sub_…", subscriptions.PauseOptions{
    Behavior:  "void",
    ResumesAt: time.Now().Add(30 * 24 * time.Hour),
})
```

Stripe auto-resumes at the timestamp. Apps don't need to write
a cron job.

## Resume

```go
err := conn.Subscriptions.Resume(ctx, "sub_…")
```

Clears `pause_collection`. Stripe resumes the normal billing cycle
from now.

## Important distinction

Stripe has TWO "paused" concepts:
- **`pause_collection`** (this wrapper) — subscription stays at
  `status="active"` or `"trialing"`; only the billing is paused.
- **`status="paused"`** — a separate state set by trial-end behavior
  or subscription schedules. Stripe's `Subscriptions.Resume` endpoint
  only works for this status, NOT for `pause_collection`. The lib's
  `Resume()` correctly uses `Update` with an empty `pause_collection`,
  not the Resume endpoint.

(Verified live: passing `subscription_schedule.Resume` to a sub
paused with `pause_collection` returns "You can only resume a
subscription if it is `paused`.")

## Customer access during pause

`pause_collection` doesn't change `Status`. `IsAccessGranting()`
still returns true → customer keeps access. To deny access during
pause, check `sub.PauseCollection != nil` in your access-check
logic (mirror exposes the raw field via `sub.Raw` or a future typed
field).

## Mirror behavior

Pause and Resume both trigger `customer.subscription.updated` webhook
events; the mirror picks up the new state on the next event delivery.
