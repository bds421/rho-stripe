// Package connectortest bundles the in-memory test helpers consumers
// need when writing their own tests against the connector + its
// subsystems. Importing this package never pulls in stripe-go (the
// helpers are all in-memory), so it's safe to depend on from
// production test code.
//
// Naming: ADR-0010 originally described this as `connector/testing`,
// but a package named `testing` would shadow the stdlib `testing`
// import in any test file that consumed it. We follow Go's
// httptest/iotest/chartest convention instead.
//
// SECURITY WARNING — these helpers are TEST-ONLY:
//   - MemoryRepos persist nothing across process restarts.
//   - The EventLog returned exposes raw event payloads (including PII)
//     without any access control.
//   - SignedBody helpers use a known test signing secret.
//
// Do NOT wire any of these in a production HTTP path. Apps building
// admin dashboards should bring their own Postgres-backed repos +
// proper authz; the helpers in connectortest exist to make YOUR tests
// of YOUR webhook handlers tractable, not to be a production stack.
package connectortest

import (
	"github.com/bds421/rho-kit/data/v2/idempotency"
	"github.com/bds421/rho-stripe/checkout"
	"github.com/bds421/rho-stripe/credits"
	"github.com/bds421/rho-stripe/webhooks"
)

// NewMemoryEventStore returns an in-memory idempotency.Store suitable
// for unit tests of webhook handlers.
func NewMemoryEventStore() idempotency.Store {
	return idempotency.NewMemoryStore()
}

// NewMemoryCustomerRepo returns an in-memory CustomerRepo for tests.
func NewMemoryCustomerRepo() checkout.CustomerRepo {
	return checkout.NewMemoryCustomerRepo()
}

// NewMemoryCreditRepo returns an in-memory CreditRepo for tests. The
// grant side honors SourceRef-based dedup so handler-replay tests
// behave like production.
func NewMemoryCreditRepo() credits.CreditRepo {
	return credits.NewMemoryRepo()
}

// NewSigner constructs a webhook signer for test events. Pass the
// same secret string to webhooks.Config.SigningSecret.
func NewSigner(secret string) *webhooks.Signer {
	return webhooks.NewSigner(secret)
}

// MemoryRepos bundles every in-memory repo in one struct, useful when
// wiring connector.Config in a test:
//
//	r := connectortest.NewMemoryRepos()
//	conn, _ := connector.New(ctx, connector.Config{
//	    Customers: r.Customers, Events: r.Events, Credits: r.Credits,
//	    // ... rest of config
//	})
//
// The fields use the lib's interface types so they can be replaced
// piecemeal if a test wants a fault-injecting fake for one repo.
type MemoryRepos struct {
	Customers checkout.CustomerRepo
	Events    idempotency.Store
	Credits   credits.CreditRepo
}

// NewMemoryRepos returns a fresh, independent set.
func NewMemoryRepos() MemoryRepos {
	return MemoryRepos{
		Customers: NewMemoryCustomerRepo(),
		Events:    NewMemoryEventStore(),
		Credits:   NewMemoryCreditRepo(),
	}
}
