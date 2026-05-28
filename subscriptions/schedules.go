package subscriptions

import (
	"context"
	"errors"
	"time"
)

// SchedulePhase is one phase of a multi-phase subscription schedule.
// Example use: "3 months of 50% off, then full price thereafter."
type SchedulePhase struct {
	// PriceKey is the catalog-relative price for this phase
	// ("pro_plan.monthly_eur"). Lib namespaces + resolves at apply time.
	PriceKey string

	// Quantity defaults to 1.
	Quantity int64

	// Iterations bounds the phase as a count of the price's natural
	// billing periods (Stripe's `iterations` field). For a MONTHLY
	// price, Iterations=3 means three months; for a YEARLY price,
	// Iterations=3 means three years. 0 = run to end of schedule.
	//
	// Renamed from DurationMonths (slice 37) because the prior name
	// implied a fixed unit independent of the underlying price's
	// interval, which was a foot-gun: callers using yearly prices
	// got 3 years when they wrote 3 expecting months.
	Iterations int

	// CouponKey, when non-empty, applies the named coupon during
	// this phase only.
	CouponKey string
}

// ScheduleInput configures CreateSchedule.
type ScheduleInput struct {
	StripeCustomerID string
	Phases           []SchedulePhase
	EndBehavior      string // "release" (default — convert to normal sub) or "cancel"
	Metadata         map[string]string
	StartAt          time.Time // zero = now
}

// CreateSchedule creates a Stripe SubscriptionSchedule with multiple
// phases. Phase 7 feature — common for promotional pricing
// ("3 months free, then full price") and trial-to-paid transitions
// with planned price changes.
//
// Requires Operations to be constructed with spec + cache for
// catalog resolution.
func (o *Operations) CreateSchedule(ctx context.Context, in ScheduleInput) (*Schedule, error) {
	if o.spec == nil || o.cache == nil {
		return nil, ErrMigrateNeedsCacheAndSpec
	}
	if in.StripeCustomerID == "" {
		return nil, errors.New("subscriptions.CreateSchedule: StripeCustomerID is required")
	}
	if len(in.Phases) == 0 {
		return nil, errors.New("subscriptions.CreateSchedule: at least one phase is required")
	}

	phases := make([]BackendPhase, 0, len(in.Phases))
	for i, p := range in.Phases {
		nskey := namespacedKeyOrSelf(o.spec, p.PriceKey)
		priceID, ok := o.cache.Lookup(nskey)
		if !ok {
			return nil, &phaseResolveError{Index: i, Key: nskey}
		}
		qty := p.Quantity
		if qty <= 0 {
			qty = 1
		}
		bp := BackendPhase{
			StripePriceID: priceID,
			Quantity:      qty,
			Iterations:    p.Iterations,
		}
		if p.CouponKey != "" {
			bp.StripeCouponID = o.spec.NamespacedCouponID(p.CouponKey)
		}
		phases = append(phases, bp)
	}

	endBehavior := in.EndBehavior
	if endBehavior == "" {
		endBehavior = "release"
	}

	return o.backend.CreateSubscriptionSchedule(ctx, ScheduleBackendInput{
		StripeCustomerID: in.StripeCustomerID,
		Phases:           phases,
		EndBehavior:      endBehavior,
		StartAt:          in.StartAt,
		Metadata:         o.stampNamespace(in.Metadata),
	})
}

// BackendPhase / ScheduleBackendInput are the Backend-level shapes.
type BackendPhase struct {
	StripePriceID  string
	StripeCouponID string
	Quantity       int64
	Iterations     int
}

type ScheduleBackendInput struct {
	StripeCustomerID string
	Phases           []BackendPhase
	EndBehavior      string
	StartAt          time.Time
	Metadata         map[string]string
}

type phaseResolveError struct {
	Index int
	Key   string
}

func (e *phaseResolveError) Error() string {
	return "subscriptions.CreateSchedule: phase " + itoa(e.Index) + " price key not in cache: " + e.Key
}

// AdjustSeats updates an existing subscription's quantity (the
// "seat-based pricing" pattern: per-user plans where the bill scales
// with seat count). Mirror catches up via the resulting webhook event.
func (o *Operations) AdjustSeats(ctx context.Context, stripeSubID, itemID string, newQuantity int64, prorate bool) error {
	if stripeSubID == "" {
		return errors.New("subscriptions.AdjustSeats: stripeSubID is required")
	}
	if itemID == "" {
		return errors.New("subscriptions.AdjustSeats: itemID is required")
	}
	if newQuantity <= 0 {
		return errors.New("subscriptions.AdjustSeats: newQuantity must be positive (use CancelNow to remove the seat product)")
	}
	return o.backend.UpdateItems(ctx, stripeSubID, []ItemChange{
		{StripeItemID: itemID, Quantity: newQuantity},
	}, UpdateOptions{ProrationBehavior: prorationBehavior(prorate)})
}

// tiny non-allocating itoa for the error formatter.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [10]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
