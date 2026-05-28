package stripeapi

import (
	"errors"

	stripe "github.com/stripe/stripe-go/v82"
)

// AsStripeError unwraps err looking for the underlying *stripe.Error
// (the typed error stripe-go returns for non-2xx responses). Returns
// (typed, true) when found, (nil, false) otherwise.
//
// Apps use this to discriminate by `Code` ("card_declined",
// "insufficient_funds", "rate_limit_exceeded", ...) without
// string-matching error messages, which would break across stripe-go
// versions.
//
//	if se, ok := stripeapi.AsStripeError(err); ok {
//	    switch se.Code {
//	    case stripe.ErrorCodeCardDeclined:
//	        // show user-friendly "card declined" UI
//	    case stripe.ErrorCodeInsufficientFunds:
//	        // ...
//	    }
//	}
//
// The standard library's errors.As works equivalently:
//
//	var se *stripe.Error
//	if errors.As(err, &se) { ... }
//
// This helper is a convenience for the typical one-line check.
func AsStripeError(err error) (*stripe.Error, bool) {
	var se *stripe.Error
	if errors.As(err, &se) {
		return se, true
	}
	return nil, false
}

// IsStripeErrorCode is a shorthand for "the error wraps a *stripe.Error
// whose Code matches one of the supplied values". Returns false on
// non-Stripe errors.
//
//	if stripeapi.IsStripeErrorCode(err, stripe.ErrorCodeCardDeclined) {
//	    ...
//	}
func IsStripeErrorCode(err error, codes ...stripe.ErrorCode) bool {
	se, ok := AsStripeError(err)
	if !ok {
		return false
	}
	for _, c := range codes {
		if se.Code == c {
			return true
		}
	}
	return false
}

// IsRateLimited returns true when err wraps a 429 from Stripe. Apps
// use this to back off / shed load proactively rather than relying on
// stripe-go's internal retry loop.
func IsRateLimited(err error) bool {
	se, ok := AsStripeError(err)
	if !ok {
		return false
	}
	return se.HTTPStatusCode == 429
}
