# Security policy

## Reporting a vulnerability

If you discover a security issue, **please email
markus@markusnissl.com** with:

- A description of the vulnerability
- Steps to reproduce
- The version / commit you're running on
- Any proof-of-concept code

Do **not** open a public GitHub issue for security reports.

We'll acknowledge receipt within 48 hours and aim to provide a fix
or mitigation within 14 days for high-severity issues.

## Scope

In-scope:

- Signature verification / replay attacks on the webhook handler
- Idempotency / dedup bypass leading to double-processing
- Credit-ledger race conditions allowing double-spend
- Postgres injection via the lib's repos
- Secret leakage via logs / error messages
- Cross-tenant data leaks (one app's webhook reaching another's handlers)

Out of scope:

- Vulnerabilities in stripe-go itself — report directly to Stripe
- Vulnerabilities in Postgres / rho-kit / your own infrastructure
- Theoretical attacks requiring physical access or root on the host
- Denial-of-service via unauthenticated input (rate-limit at your
  edge; the library deliberately doesn't ship its own rate limiter)

## Disclosure

We follow coordinated disclosure: after a fix ships in a tagged
release, we publish a security advisory crediting the reporter
(unless you ask to remain anonymous) and document the fix in
[CHANGELOG.md](CHANGELOG.md).

## Past advisories

(none yet)
