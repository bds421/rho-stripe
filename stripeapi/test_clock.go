package stripeapi

import (
	"context"
	"fmt"
	"time"

	stripe "github.com/stripe/stripe-go/v82"
)

// TestClock wraps Stripe's test_clocks API for simulating time
// advancement in test mode. Apps that need to verify "subscription
// renews at period end" or "trial expires" can use a Stripe Test
// Clock to fast-forward Stripe's view of time, then observe the
// resulting webhook events as if real time had passed.
//
// Test-mode only — calling these methods against a live Stripe key
// returns 400.
//
// Stripe docs: https://stripe.com/docs/billing/testing/test-clocks
type TestClock struct {
	StripeID string
	Status   string // "ready" | "advancing" | "internal_failure"
	FrozenAt time.Time
}

// CreateTestClock creates a new Stripe Test Clock frozen at frozenAt.
// Customers + subscriptions created with this clock's id observe its
// time instead of wall time.
func CreateTestClock(ctx context.Context, sc *stripe.Client, frozenAt time.Time, name string) (*TestClock, error) {
	if sc == nil {
		return nil, fmt.Errorf("stripeapi.CreateTestClock: nil StripeClient")
	}
	params := &stripe.TestHelpersTestClockCreateParams{
		FrozenTime: stripe.Int64(frozenAt.Unix()),
	}
	if name != "" {
		params.Name = stripe.String(name)
	}
	tc, err := sc.V1TestHelpersTestClocks.Create(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("stripeapi.CreateTestClock: %w", err)
	}
	return projectTestClock(tc), nil
}

// AdvanceTestClock fast-forwards the Test Clock to advanceTo. Stripe
// performs renewals / trial expirations / dunning transitions as if
// real time had elapsed; webhook events fire normally.
//
// The call is async on Stripe's side — poll with GetTestClock or wait
// for the test_helpers.test_clock.ready event.
func AdvanceTestClock(ctx context.Context, sc *stripe.Client, testClockID string, advanceTo time.Time) (*TestClock, error) {
	if testClockID == "" {
		return nil, fmt.Errorf("stripeapi.AdvanceTestClock: testClockID is required")
	}
	params := &stripe.TestHelpersTestClockAdvanceParams{
		FrozenTime: stripe.Int64(advanceTo.Unix()),
	}
	tc, err := sc.V1TestHelpersTestClocks.Advance(ctx, testClockID, params)
	if err != nil {
		return nil, fmt.Errorf("stripeapi.AdvanceTestClock(%s): %w", testClockID, err)
	}
	return projectTestClock(tc), nil
}

// DeleteTestClock removes a test clock + every customer/subscription
// associated with it. Use in test cleanup.
func DeleteTestClock(ctx context.Context, sc *stripe.Client, testClockID string) error {
	if testClockID == "" {
		return fmt.Errorf("stripeapi.DeleteTestClock: testClockID is required")
	}
	if _, err := sc.V1TestHelpersTestClocks.Delete(ctx, testClockID, nil); err != nil {
		return fmt.Errorf("stripeapi.DeleteTestClock(%s): %w", testClockID, err)
	}
	return nil
}

// GetTestClock fetches the current state of a Test Clock (useful for
// polling after AdvanceTestClock until status="ready").
func GetTestClock(ctx context.Context, sc *stripe.Client, testClockID string) (*TestClock, error) {
	tc, err := sc.V1TestHelpersTestClocks.Retrieve(ctx, testClockID, nil)
	if err != nil {
		return nil, fmt.Errorf("stripeapi.GetTestClock(%s): %w", testClockID, err)
	}
	return projectTestClock(tc), nil
}

func projectTestClock(tc *stripe.TestHelpersTestClock) *TestClock {
	return &TestClock{
		StripeID: tc.ID,
		Status:   string(tc.Status),
		FrozenAt: time.Unix(tc.FrozenTime, 0).UTC(),
	}
}
