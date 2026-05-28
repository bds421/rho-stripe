// Command main seeds the saas_tiers example catalog to your Stripe
// test account (and prints diff in dry-run mode).
//
// Usage:
//
//	set -a; source .env; set +a
//	go run ./examples/saas_tiers/main             # dry-run diff
//	go run ./examples/saas_tiers/main sync --apply # actually apply
package main

import (
	"github.com/bds421/rho-stripe/cli"
	"github.com/bds421/rho-stripe/examples/saas_tiers"
)

func main() { cli.Main(saas_tiers.Spec()) }
