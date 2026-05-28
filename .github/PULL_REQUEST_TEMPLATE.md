## Summary

(1-3 sentences: what changed and why)

## Test plan

- [ ] Unit tests pass (`go test ./...`)
- [ ] Postgres integration tests pass (`cd repos/postgres && go test -tags=postgres_integration ./...`)
- [ ] Race detector clean on touched packages (`go test -race ./<pkg>/...`)
- [ ] Live Stripe tests pass (if API-touching change; `go test -tags=live_stripe ./...`)
- [ ] CHANGELOG.md updated under [Unreleased]
- [ ] How-to / GoDoc updated for new public API

## Live verification (if applicable)

(paste the Stripe artifact ids your live tests created — proves
the change works against real Stripe)

## Breaking changes

(does this change a public API or a webhook contract? If yes,
describe migration path)

## Related

(issues / discussions / Stripe docs)
