package postgres

import (
	"database/sql"

	pgstore "github.com/bds421/rho-kit/data/idempotency/pgstore/v2"
	"github.com/bds421/rho-kit/data/v2/idempotency"
	"github.com/bds421/rho-stripe/checkout"
	"github.com/bds421/rho-stripe/credits"
	"github.com/bds421/rho-stripe/metering"
	"github.com/bds421/rho-stripe/subscriptions"
)

// Repos bundles the Postgres-backed implementations of the lib's
// storage interfaces. Constructed via NewRepos; passed piecewise to
// connector.Config.
//
// EventStore reuses rho-kit's pgstore.Store directly — webhook dedup
// is identical to any other idempotency-keyed HTTP flow rho-kit
// already covers, so reimplementing it would be duplication.
type Repos struct {
	Customers     checkout.CustomerRepo
	Credits       credits.CreditRepo
	Events        idempotency.Store
	Subscriptions subscriptions.SubscriptionRepo
	Usage         metering.UsageRepo
}

// NewRepos constructs the repo implementations against db. The caller
// is responsible for applying the schema before first use (see
// schema/embed.go).
func NewRepos(db *sql.DB) Repos {
	if db == nil {
		panic("postgres.NewRepos: db is required")
	}
	return Repos{
		Customers:     NewCustomerRepo(db),
		Credits:       NewCreditRepo(db),
		Events:        pgstore.New(db),
		Subscriptions: NewSubscriptionRepo(db),
		Usage:         NewUsageRepo(db),
	}
}
