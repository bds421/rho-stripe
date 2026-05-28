# Retries & idempotency

rho-stripe applies two layers of resilience to every outbound
Stripe write, so a transient network blip or a Stripe 5xx never leaves
you with a duplicate charge.

## The two layers

### 1. Stripe-go MaxNetworkRetries

Configured via `stripeapi.Config.MaxNetworkRetries` (default `3`).
Stripe-go retries failed requests with exponential backoff + jitter
when:

- The response includes `Stripe-Should-Retry: true` (Stripe explicitly
  marks the call as safe to replay), or
- The failure is a network-level error (DNS, TCP, TLS handshake).

The retry layer honors Stripe's `Retry-After` header on 429 rate-limit
responses, so a burst that gets throttled is paced automatically.

### 2. Auto-derived Idempotency-Key on every write

Without an Idempotency-Key, retrying a successful-but-disconnected
request creates a duplicate Stripe resource. With one, Stripe returns
the *same* response within a 24h dedup window.

The lib derives a deterministic key from the operation name + canonical
input bytes for every write — `CreateCustomer`, `CreateCheckoutSession`,
`AddTaxID`, `Refund`, every catalog mutation, etc. Two calls with the
*same inputs* deduplicate; two calls with *different inputs* do not.

```go
// First call — creates customer cus_X.
id1, _ := backend.CreateCustomer(ctx, in)

// Network blip, retry — Stripe sees same Idempotency-Key, returns
// cus_X again instead of creating cus_Y.
id2, _ := backend.CreateCustomer(ctx, in)

// id1 == id2 ✓ (verified by TestLive_IdempotencyKeyDedupesCustomers)
```

## Caller-supplied keys

When you want explicit control over dedup scope — e.g. a long-running
external workflow that needs to share the same key across multiple
process restarts — wrap the context:

```go
ctx = stripeapi.WithIdempotencyKey(ctx, "checkout:order-42:retry-3")
session, err := backend.CreateCheckoutSession(ctx, in)
```

The caller-supplied key takes precedence over auto-derivation for the
next outbound write on that ctx.

## When to NOT rely on dedup

Idempotency keys are scoped to a single Stripe API key + 24h window.
They do *not* protect across:

- A Stripe account migration (different account, different key)
- A 24h+ retry gap (Stripe purges keys older than 24h)
- A library upgrade that changes the canonical-input derivation

If you need stronger guarantees, persist the result of every Stripe
write in your own DB (the `webhooks` package's event log is the
canonical example) and treat the Stripe resource id as the source of
truth.

## Verification

`TestLive_IdempotencyKeyDedupesCustomers` proves the contract against
a real Stripe account: two `CreateCustomer` calls with identical
metadata return the same `cus_…` id.

## Knobs

```go
sc := stripeapi.NewClient(stripeapi.Config{
    SecretKey:         os.Getenv("STRIPE_SECRET_KEY"),
    Timeout:           30 * time.Second, // per-request
    MaxNetworkRetries: 5,                // bump for unreliable networks
})
```

Set `MaxNetworkRetries: -1` to disable retries entirely (for tests
that want fast, deterministic failures).
