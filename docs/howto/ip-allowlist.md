# Webhook source-IP allowlist

Signature verification is the primary defense against spoofed webhook
deliveries. The IP allowlist is **defense in depth** — it rejects
requests at the network layer before the signature check runs, which
helps with:

- DoS amplification (signature verification is HMAC-SHA256; rejecting
  by IP is cheaper)
- Detection of signature secret leakage (a leaked secret can't replay
  webhooks from the attacker's IP if your allowlist matches Stripe's)

## Recommended: fetch from Stripe at startup

Stripe publishes the live list at https://stripe.com/files/ips/ips_webhooks.json.
Hardcoding the list goes stale; **fetch** at startup:

```go
ips, err := webhooks.FetchStripeWebhookIPs(ctx, nil)  // nil → default 10s-timeout client
if err != nil {
    // Fail-open with bundled snapshot + warning, OR fail-closed with
    // a startup error — your call. We recommend bundled fallback so
    // an outage of stripe.com doesn't take YOUR app down.
    log.Warn("could not fetch live Stripe IPs; using bundled snapshot", "err", err)
    cfg.AllowedSourceCIDRs = webhooks.StripeWebhookCIDRs
} else {
    cfg.AllowedSourceCIDRs = ips.CIDRs()
}
```

## For long-running processes: auto-refresh

The published IPs change occasionally. Wire an `IPRefresher` to
re-fetch on an interval:

```go
refresher := webhooks.NewIPRefresher(nil, 24*time.Hour)
refresher.SetOnError(func(err error) {
    log.Warn("Stripe IP refresh failed; previous list still applies", "err", err)
})
if err := refresher.Start(ctx, conn.Webhooks); err != nil {
    return fmt.Errorf("initial Stripe IP fetch failed: %w", err)
}
defer refresher.Stop()
```

`refresher.Start` does the first fetch synchronously, then spawns a
background goroutine. `refresher.Last()` returns the most recent
successful snapshot — handy for /healthz diagnostics.

## Behind a load balancer

If your app sits behind a LB / proxy that terminates TLS, `r.RemoteAddr`
is the LB's IP, not Stripe's. Set `RealIPHeader`:

```go
cfg := webhooks.Config{
    AllowedSourceCIDRs: ips.CIDRs(),
    RealIPHeader:       "X-Forwarded-For",  // or "X-Real-IP"
    // ... rest of config
}
```

The allowlist then evaluates the first hop in X-Forwarded-For (the
original client) rather than the LB's IP.

## Without an allowlist

Leave `AllowedSourceCIDRs` empty (default). The IP check is skipped
entirely; signature verification remains active. Apps that run in
environments where Stripe IPs can change without warning (Fly.io
edge, etc.) often skip the IP check.

## Bundled fallback

`webhooks.StripeWebhookCIDRs` is a snapshot of the published list
validated against `webhooks.StripeWebhookCIDRsLastUpdated`. Use as
the offline / egress-blocked fallback when `FetchStripeWebhookIPs`
isn't an option.
