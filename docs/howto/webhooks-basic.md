# Webhooks — minimum viable wiring

```go
conn, err := connector.New(ctx, connector.Config{
    SecretKey:     os.Getenv("STRIPE_SECRET_KEY"),
    WebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"),
    AppNamespace:  "myapp",
    Catalog:       spec,
    Customers:     pgrepo.NewCustomerRepo(db),
    Events:        idempotency.NewMemoryStore(),  // or pgstore for multi-replica
    Handlers: webhooks.Handlers{
        OnCheckoutCompleted: func(ctx context.Context, evt webhooks.Event) error {
            return provisionAccess(ctx, evt)
        },
    },
})
http.HandleFunc("/webhook", conn.Webhooks.Handle)
```

That's the minimum. The library handles signature verification, dedup,
namespace filtering (so multi-app accounts route correctly), and
auto-handlers (recurring grants on `invoice.paid`, subscription mirror
on `customer.subscription.*` events).

## Status codes Stripe sees

- **200**: handler ran (or duplicate / no-op). Stripe stops retrying.
- **400**: signature verification failed or event ID was reused with a different body.
- **500**: handler returned an error. Stripe retries per its schedule.

## Critical: don't consume the body before Handle

```go
// WRONG — body-buffering middleware breaks signature verification:
r.Use(bodyBufferingMiddleware)
r.HandleFunc("/webhook", conn.Webhooks.Handle)

// CORRECT — Handle reads r.Body itself; raw bytes must reach it:
r.HandleFunc("/webhook", conn.Webhooks.Handle)
```

## Local testing

```bash
# Terminal 1: forward Stripe events to localhost.
stripe listen --forward-to localhost:8080/webhook
# Copy the whsec_… it prints.

# Terminal 2: start your server with that secret.
STRIPE_WEBHOOK_SECRET=whsec_… go run ./cmd/example-webhook

# Terminal 3: fire a test event.
stripe trigger checkout.session.completed
```

Or use [`docker-compose.yml`](../../docker-compose.yml) which wires
all three together (see `cd ../.. && docker compose up`).

## Typed handlers available

| Event | Field |
|---|---|
| `checkout.session.completed` | `OnCheckoutCompleted` |
| `payment_intent.succeeded` | `OnPaymentSucceeded` |
| `payment_intent.payment_failed` | `OnPaymentFailed` |
| `invoice.paid` | `OnInvoicePaid` |
| `invoice.payment_failed` | `OnInvoicePaymentFailed` |
| `customer.subscription.created` | `OnSubscriptionCreated` |
| `customer.subscription.updated` | `OnSubscriptionUpdated` |
| `customer.subscription.deleted` | `OnSubscriptionCanceled` |
| `customer.subscription.trial_will_end` | `OnSubscriptionTrialWillEnd` |
| `charge.refunded` / `refund.created` | `OnRefundCreated` |
| `charge.dispute.*` (5 events) | `OnDisputeCreated`/`OnDisputeUpdated`/`OnDisputeClosed`/`OnDisputeFundsWithdrawn`/`OnDisputeFundsReinstated` |
| anything else | `OnOtherEvent` |

## Next

- [webhooks-async-dispatch.md](webhooks-async-dispatch.md) — async dispatch for high-volume endpoints
- [webhooks-secret-rotation.md](webhooks-secret-rotation.md) — rotating the signing secret
- [webhooks-replay.md](webhooks-replay.md) — replaying an event after a handler fix
