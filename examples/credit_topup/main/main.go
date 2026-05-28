package main

import (
	"github.com/bds421/rho-stripe/cli"
	"github.com/bds421/rho-stripe/examples/credit_topup"
)

func main() { cli.Main(credit_topup.Spec()) }
