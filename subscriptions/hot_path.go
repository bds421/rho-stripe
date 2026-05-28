package subscriptions

import (
	"context"
	"strings"
)

// Helper hot-path queries built on top of SubscriptionRepo. These hit
// only the app DB; no Stripe calls. Apps wire them into request
// handlers to answer "is this org on the Pro plan?" cheaply.

// ListActive returns subscriptions whose Status is access-granting
// (per Status.IsAccessGranting — active / trialing / past_due).
// Excludes incomplete and terminal states.
func ListActive(ctx context.Context, repo SubscriptionRepo, subject SubjectID) ([]*Subscription, error) {
	subs, err := repo.ListBySubject(ctx, subject)
	if err != nil {
		return nil, err
	}
	out := subs[:0]
	for _, s := range subs {
		if s.Status.IsAccessGranting() {
			out = append(out, s)
		}
	}
	return out, nil
}

// HasActivePrice reports whether subject holds any active subscription
// with at least one line item whose PriceKey matches priceKeyOrGlob.
//
// priceKeyOrGlob accepts exact keys ("pro_plan.monthly_eur") OR a
// trailing `*` glob ("pro_plan.*", which matches both monthly and
// yearly). Useful for "do they have any pro plan" without enumerating
// every interval.
func HasActivePrice(ctx context.Context, repo SubscriptionRepo, subject SubjectID, priceKeyOrGlob string) (bool, error) {
	subs, err := ListActive(ctx, repo, subject)
	if err != nil {
		return false, err
	}
	matcher := newPriceKeyMatcher(priceKeyOrGlob)
	for _, s := range subs {
		for _, item := range s.Items {
			if matcher(item.PriceKey) {
				return true, nil
			}
		}
	}
	return false, nil
}

// ListByPriceKey returns active subscriptions matching priceKeyOrGlob.
func ListByPriceKey(ctx context.Context, repo SubscriptionRepo, subject SubjectID, priceKeyOrGlob string) ([]*Subscription, error) {
	subs, err := ListActive(ctx, repo, subject)
	if err != nil {
		return nil, err
	}
	matcher := newPriceKeyMatcher(priceKeyOrGlob)
	var out []*Subscription
	for _, s := range subs {
		for _, item := range s.Items {
			if matcher(item.PriceKey) {
				out = append(out, s)
				break
			}
		}
	}
	return out, nil
}

// newPriceKeyMatcher returns a predicate that matches an exact key
// or a trailing-`*` glob. Single-`*` only — no path components.
func newPriceKeyMatcher(pat string) func(string) bool {
	if strings.HasSuffix(pat, "*") {
		prefix := strings.TrimSuffix(pat, "*")
		return func(k string) bool { return strings.HasPrefix(k, prefix) }
	}
	return func(k string) bool { return k == pat }
}
