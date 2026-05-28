package main

import (
	"github.com/bds421/rho-stripe/cli"
	"github.com/bds421/rho-stripe/examples/included_quota"
)

func main() { cli.Main(included_quota.Spec()) }
