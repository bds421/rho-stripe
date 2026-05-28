# Kubernetes deployment example

Reference manifests for deploying an app that uses rho-stripe,
in a multi-replica, multi-process configuration with a Postgres-backed
webhook queue + drift detector + observability.

## Files

- `secret.yaml` — Stripe keys (creates a Secret; encrypt your real one with sealed-secrets / SOPS / etc.)
- `configmap.yaml` — non-secret app config
- `deployment.yaml` — 3 replicas of the app (webhook server + business logic)
- `service.yaml` — ClusterIP exposing the webhook endpoint
- `ingress.yaml` — TLS termination + Stripe's IPs allowlist
- `cronjob-drift-check.yaml` — daily drift-check (alerts on dashboard tampering)

## Architecture

```
                ┌─────────────────────────────────────────────┐
                │                  Kubernetes                  │
                │                                              │
   Stripe       │   ┌──────────┐   ┌──────────┐  ┌──────────┐  │
   webhook ────────▶│ app-pod  │   │ app-pod  │  │ app-pod  │  │
   (POST)       │   │  (Go)    │   │  (Go)    │  │  (Go)    │  │
                │   └────┬─────┘   └────┬─────┘  └────┬─────┘  │
                │        │              │             │        │
                │        └──────┬───────┴─────────────┘        │
                │               ▼                              │
                │   ┌─────────────────────────┐                │
                │   │   Postgres (RDS / pg)    │                │
                │   │   ─ webhook queue table  │                │
                │   │   ─ idempotency store    │                │
                │   │   ─ subscription mirror  │                │
                │   │   ─ credit ledger        │                │
                │   └─────────────────────────┘                │
                └──────────────────────────────────────────────┘
```

3 replicas all run `conn.Webhooks.SetQueue(pgrepo.NewWebhookQueue(db, …))`.
Stripe POSTs go to whatever pod the LoadBalancer picks; that pod enqueues
the row; whichever pod's worker grabs it first (via SELECT FOR UPDATE
SKIP LOCKED) processes it. No leader election needed for webhook flow.

The drift detector runs as a CronJob (one pod per run) so we don't pay
the cost of N replicas all pinging Stripe every 15 minutes.

## Apply

```bash
# 1. Edit secret.yaml — replace the placeholders with your real keys
#    (and don't commit this version to git!)
kubectl apply -f secret.yaml

kubectl apply -f configmap.yaml
kubectl apply -f deployment.yaml
kubectl apply -f service.yaml
kubectl apply -f ingress.yaml
kubectl apply -f cronjob-drift-check.yaml

# 2. Configure Stripe webhook endpoint to point at your ingress:
#    https://api.your-domain.com/webhook
#    Stripe Dashboard → Developers → Webhooks → "Add endpoint"
```

## Secret management

For production, replace the `secret.yaml` here with a proper
secret-management workflow:

- **sealed-secrets**: encrypt with the cluster's public key, commit
  to git, in-cluster controller decrypts.
- **External Secrets Operator + AWS Secrets Manager / GCP Secret
  Manager / Vault**: secret lives outside the cluster; ESO syncs
  it as a Kubernetes Secret.

Don't commit the plain `secret.yaml` from this example to your repo.
