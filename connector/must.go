package connector

import (
	"github.com/bds421/rho-stripe/credits"
	"github.com/bds421/rho-stripe/metering"
	"github.com/bds421/rho-stripe/plans"
	"github.com/bds421/rho-stripe/subscriptions"
)

// MustSubscriptions returns c.Subscriptions or panics with a clear
// message if Config.Subscriptions was nil at construction.
//
// Use this in code paths that *require* the subsystem — apps that
// optionally support subscriptions check c.Subscriptions != nil
// instead. Both patterns are valid; MustSubscriptions exists so
// "we use subs everywhere" apps don't sprinkle nil-checks.
func (c *Connector) MustSubscriptions() *subscriptions.Operations {
	if c.Subscriptions == nil {
		panic("connector: Subscriptions is nil — set Config.Subscriptions to a subscriptions.SubscriptionRepo before calling New")
	}
	return c.Subscriptions
}

// MustCredits returns c.Credits or panics if unset.
func (c *Connector) MustCredits() *credits.Operations {
	if c.Credits == nil {
		panic("connector: Credits is nil — set Config.Credits to a credits.CreditRepo before calling New")
	}
	return c.Credits
}

// MustMetering returns c.Metering or panics if unset.
func (c *Connector) MustMetering() *metering.Operations {
	if c.Metering == nil {
		panic("connector: Metering is nil — set Config.Usage to a metering.UsageRepo before calling New")
	}
	return c.Metering
}

// MustPlans returns c.Plans or panics if unset (requires Subscriptions
// to be configured).
func (c *Connector) MustPlans() *plans.Operations {
	if c.Plans == nil {
		panic("connector: Plans is nil — Plans requires Config.Subscriptions to be set (Plans aggregates across the subscription mirror)")
	}
	return c.Plans
}
