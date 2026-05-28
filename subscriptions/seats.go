package subscriptions

import (
	"context"
	"errors"
	"fmt"
)

// SetSeats sets the per-seat item quantity to newCount on the
// subscription identified by stripeSubID. The seat-priced item is
// located by matching priceKey against the subscription's mirrored
// items.
//
// Returns ErrSeatPriceNotInSubscription when the price key isn't
// part of the subscription (typical cause: the app's "seat product"
// catalog key doesn't match what was originally purchased).
func (o *Operations) SetSeats(ctx context.Context, subjectID SubjectID, stripeSubID, priceKey string, newCount int64, prorate bool) error {
	if newCount <= 0 {
		return errors.New("subscriptions.SetSeats: newCount must be positive (use CancelNow to remove seat product entirely)")
	}
	if stripeSubID == "" {
		return errors.New("subscriptions.SetSeats: stripeSubID is required")
	}
	if priceKey == "" {
		return errors.New("subscriptions.SetSeats: priceKey is required")
	}

	nskey := namespacedKeyOrSelf(o.spec, priceKey)
	itemID, current, err := o.resolveSeatItem(ctx, subjectID, stripeSubID, nskey)
	if err != nil {
		return err
	}
	if current == newCount {
		return nil // no-op
	}

	return o.backend.UpdateItems(ctx, stripeSubID, []ItemChange{
		{StripeItemID: itemID, Quantity: newCount},
	}, UpdateOptions{ProrationBehavior: prorationBehavior(prorate)})
}

// resolveSeatItem returns (stripeItemID, currentQty, err) for the
// price key on the subscription. Resolution order:
//  1. Mirror — fast path, no network. Requires subject ownership.
//  2. Stripe fallback — used when the mirror has no record of the
//     subscription (typical for newly created subs whose webhook hasn't
//     landed yet). The fallback skips the subject check because the
//     mirror is the only source of subject identity.
//
// Returns ErrSubscriptionNotForSubject when the mirror knows the sub
// but it belongs to someone else (an authorization failure, NOT a
// race). Returns ErrSeatPriceNotInSubscription when neither source has
// the price.
func (o *Operations) resolveSeatItem(ctx context.Context, subjectID SubjectID, stripeSubID, nskey string) (string, int64, error) {
	sub, ok, err := o.repo.GetByStripeID(ctx, stripeSubID)
	if err != nil {
		return "", 0, err
	}
	if ok {
		if sub.SubjectID != subjectID {
			return "", 0, ErrSubscriptionNotForSubject
		}
		for _, it := range sub.Items {
			if it.PriceKey == nskey {
				return it.StripeID, it.Quantity, nil
			}
		}
		return "", 0, ErrSeatPriceNotInSubscription
	}
	// Mirror miss → fallback to Stripe so newly-created subs don't
	// race against webhook delivery.
	items, err := o.backend.GetSubscriptionItems(ctx, stripeSubID)
	if err != nil {
		return "", 0, fmt.Errorf("subscriptions: GetSubscriptionItems fallback: %w", err)
	}
	for _, it := range items {
		if it.PriceKey == nskey {
			return it.StripeID, it.Quantity, nil
		}
	}
	return "", 0, ErrSeatPriceNotInSubscription
}

// SeatCount returns the current quantity of the priceKey item on the
// subscription, as known by the mirror. Returns 0 when the price
// isn't part of the subscription.
func (o *Operations) SeatCount(ctx context.Context, subjectID SubjectID, stripeSubID, priceKey string) (int64, error) {
	nskey := namespacedKeyOrSelf(o.spec, priceKey)
	_, current, err := o.resolveSeatItem(ctx, subjectID, stripeSubID, nskey)
	if errors.Is(err, ErrSeatPriceNotInSubscription) {
		// Distinguish "price not on sub" from "no sub at all" by
		// returning 0 with nil — matches the prior contract for the
		// mirror-hit path where the price key simply wasn't found.
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return current, nil
}

// AddSeats increments seat count by delta. Convenience over SetSeats
// when callers think in deltas ("invite added a user → +1").
func (o *Operations) AddSeats(ctx context.Context, subjectID SubjectID, stripeSubID, priceKey string, delta int64, prorate bool) error {
	if delta == 0 {
		return nil
	}
	cur, err := o.SeatCount(ctx, subjectID, stripeSubID, priceKey)
	if err != nil {
		return err
	}
	next := cur + delta
	if next <= 0 {
		return errors.New("subscriptions.AddSeats: result would be <= 0; use CancelNow to remove the subscription")
	}
	return o.SetSeats(ctx, subjectID, stripeSubID, priceKey, next, prorate)
}

var (
	// ErrSubscriptionNotForSubject means the mirror has the subscription
	// but it belongs to a different subject. Surface as 403/404 in apps.
	ErrSubscriptionNotForSubject = errors.New("subscriptions: subscription does not belong to subject")

	// ErrSeatPriceNotInSubscription means no item on the subscription
	// matches the given price key.
	ErrSeatPriceNotInSubscription = errors.New("subscriptions: price key is not part of the subscription")
)
