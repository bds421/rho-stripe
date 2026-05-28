// example-sync is a tiny demo binary that exercises the CLI against a
// hard-coded sample catalog. Real apps replace `demoCatalog` with their
// own catalog.Spec and call cli.Main from their own cmd/sync/main.go.
//
// Usage:
//
//	set -a; source .env; set +a
//	go run ./cmd/example-sync diff
//	go run ./cmd/example-sync sync --apply
//	go run ./cmd/example-sync verify-account
package main

import (
	"github.com/bds421/rho-stripe/catalog"
	"github.com/bds421/rho-stripe/cli"
)

var demoCatalog = catalog.MustSpec(catalog.Spec{
	Namespace: "scdemo", // "rho-stripe demo"
	Products: map[string]catalog.Product{
		"pro_plan": {
			Name:        "Demo Pro Plan",
			Description: "Sample product created by rho-stripe example-sync",
			TaxCategory: catalog.TaxCategorySaaS,
			Prices: map[string]catalog.Price{
				"monthly_eur": {Amount: 4900, Currency: "eur", Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
				"yearly_eur":  {Amount: 49000, Currency: "eur", Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalYear},
			},
		},
		"credit_pack_1000_ai": {
			Name:        "1000 Demo AI Credits",
			TaxCategory: catalog.TaxCategorySaaS,
			Prices: map[string]catalog.Price{
				"default": {Amount: 1000, Currency: "eur", Type: catalog.PriceTypeOneTime},
			},
			CreditGrant: &catalog.CreditGrant{Bucket: "ai", Amount: 1000, ValidDays: 90},
		},
	},
})

func main() {
	cli.Main(demoCatalog)
}
