# Credits under load — concurrency + retries

The Postgres credit ledger is safe under heavy concurrency, but the
safety mechanism (per-subject `pg_try_advisory_xact_lock`) trades
queuing for failure. Under enough simultaneous deductions on the same
subject, some calls return a lock-acquisition error instead of
queuing indefinitely.

## What we measured

Load test in `repos/postgres/credit_load_test.go`:

| Setup | Result |
|---|---|
| Single grant of 500 units, 1000 concurrent `TryDeduct(amount=1)` on the **same subject** | ~290 succeed, ~710 hit transient lock errors |
| Balance after: exact `grant - successful_deducts` | No double-spend |
| Throughput: ~690 attempts/sec on commodity Postgres | — |

Read the headline: **under unrealistic worst case (1000 simultaneous
deductions on one subject), no double-spend occurs, but ~70% of
requests fail with transient errors.** Production apps almost never
hit this profile — typical traffic is across many subjects, where
each subject's lock is uncontended.

## When this matters in production

Realistic high-load scenarios:

- API platform billing each API call: deductions spread across N
  customers → per-subject contention low → no errors.
- Batch processor deducting 1000 minutes in one go for one customer:
  use ONE `TryDeduct(amount=1000)` call, not 1000 calls.
- Real-time game charging per action: deductions spread across players.

## When this does NOT scale

- Auction-style flash sales where 10,000 users all spend from a
  shared pool in the same second. Don't use this lib's credit ledger
  for that — use Redis or a write-aside cache.

## Recommended app-side retry policy

```go
const maxRetries = 5
var bal credits.Balance
var ok bool
var err error

for attempt := 0; attempt < maxRetries; attempt++ {
    ok, bal, err = conn.Credits.TryDeduct(ctx, credits.DeductInput{...})
    if err == nil {
        break // success or insufficient — both are terminal
    }
    // Transient lock error → backoff + retry.
    time.Sleep(time.Duration(attempt*attempt) * 10 * time.Millisecond)  // 0, 10, 40, 90, 160ms
}
if err != nil {
    return fmt.Errorf("deduct after %d retries: %w", maxRetries, err)
}
if !ok {
    return ErrInsufficient
}
```

## Sharding by subject

If a single subject genuinely needs >100 deducts/sec, split the
ledger into per-shard buckets:

```go
shard := hash(subject) % 16
bucket := fmt.Sprintf("credits_shard_%d", shard)
conn.Credits.Deduct(ctx, subject, bucket, 1, ...)
```

Then balance-reporting sums across shards. Trade-off: more
bookkeeping; only do it if measured contention is real.

## See also

- [`credits-included-quota.md`](credits-included-quota.md) — typical recurring-grant pattern
- [`credits-prepaid-packs.md`](credits-prepaid-packs.md) — one-time purchases + auto-refill
