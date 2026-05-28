module github.com/bds421/rho-stripe/repos/postgres

go 1.26.2

require (
	github.com/bds421/rho-kit/data/idempotency/pgstore/v2 v2.0.0
	github.com/bds421/rho-kit/data/lock/pgadvisory/v2 v2.0.0
	github.com/bds421/rho-kit/data/v2 v2.0.0
	github.com/jackc/pgx/v5 v5.7.6
	github.com/bds421/rho-stripe v0.0.0
)

require (
	github.com/bds421/rho-kit/core/v2 v2.0.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/stripe/stripe-go/v82 v82.5.1 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.43.0 // indirect
	go.opentelemetry.io/otel/metric v1.43.0 // indirect
	go.opentelemetry.io/otel/trace v1.43.0 // indirect
	golang.org/x/crypto v0.37.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/text v0.37.0 // indirect
)

replace (
	github.com/bds421/rho-kit/core/v2 => ../../../rho-kit/core
	github.com/bds421/rho-kit/data/idempotency/pgstore/v2 => ../../../rho-kit/data/idempotency/pgstore
	github.com/bds421/rho-kit/data/lock/pgadvisory/v2 => ../../../rho-kit/data/lock/pgadvisory
	github.com/bds421/rho-kit/data/v2 => ../../../rho-kit/data
	github.com/bds421/rho-kit/observability/v2 => ../../../rho-kit/observability
	github.com/bds421/rho-stripe => ../..
)
