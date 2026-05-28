# Rotating the webhook signing secret

Stripe lets you have multiple signing secrets active simultaneously
on an endpoint — useful for zero-downtime rotation.

## The 3-step rotation

1. **Add a new secret** in Stripe Dashboard → Webhooks → your endpoint
   → "Add new signing secret." Stripe keeps signing with the OLD
   secret AND starts accepting signatures from the new one. You now
   have both secrets valid.
2. **Deploy the lib with both secrets** so signature verification
   accepts events signed with either:
   ```go
   cfg := connector.Config{
       SigningSecret:            os.Getenv("STRIPE_WEBHOOK_SECRET_NEW"),
       AdditionalSigningSecrets: []string{os.Getenv("STRIPE_WEBHOOK_SECRET_OLD")},
       ...
   }
   ```
3. **Promote** in Stripe Dashboard → "Make this secret active."
   Stripe starts signing only with the new secret. Wait a few hours
   for the queue to drain → remove the old secret from
   `AdditionalSigningSecrets` and redeploy.

## Verifying

After step 2, watch your logs for "signature verification failed".
A rotation that's working shows zero failures. If you see them,
double-check that BOTH secrets are in the config.

The library tries `SigningSecret` first; falls back to each
`AdditionalSigningSecrets` entry in order. Returns the primary-secret
error if all fail (to keep error messages predictable for ops).
