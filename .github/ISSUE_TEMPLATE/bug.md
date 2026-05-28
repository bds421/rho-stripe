---
name: Bug report
about: Something doesn't work as documented
labels: bug
---

## What happened

(brief description; what was the unexpected behavior?)

## What you expected

(what should have happened instead?)

## Reproduction

Minimum code/steps to reproduce. If it involves Stripe webhooks or
catalog state, include the relevant `Spec` excerpt + the event(s)
involved.

```go
// minimal reproduction
```

## Versions

- rho-stripe: (commit hash or tag)
- Go: (go version)
- stripe-go: (from go.mod)
- Postgres (if using the postgres reference repos): (server version)
- OS: (linux/darwin/windows)

## Logs / output

```
(paste any relevant log lines, error messages)
```

## Additional context

(anything else — links to upstream Stripe docs, related issues, etc.)
