package main

import (
	"github.com/bds421/rho-stripe/cli"
	"github.com/bds421/rho-stripe/examples/per_seat_metered"
)

func main() { cli.Main(per_seat_metered.Spec()) }
