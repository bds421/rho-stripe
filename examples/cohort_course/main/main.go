package main

import (
	"github.com/bds421/rho-stripe/cli"
	"github.com/bds421/rho-stripe/examples/cohort_course"
)

func main() { cli.Main(cohort_course.Spec()) }
