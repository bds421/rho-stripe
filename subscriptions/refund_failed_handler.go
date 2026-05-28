package subscriptions

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/bds421/rho-stripe/webhooks"
)

// PaymentIntentToSubscriptionResolver maps a Stripe PaymentIntent id
// to a Subscription id. Required for the auto-cancel-on-failed-refund
// flow because Stripe doesn't directly expose the PI→Subscription
// link in event payloads — apps need their own index (typically
// recorded at checkout-completion time).
//
// Return ("", nil) when the PI isn't tied to a subscription
// (one-time charge); return ("", err) for genuine lookup errors.
type PaymentIntentToSubscriptionResolver func(ctx context.Context, paymentIntentID string) (string, error)

// CancelSubscriptionFromFailedRefund cancels the subscription tied to
// a failed-refund event. The connector wraps the app's OnRefundFailed
// with this when Config.AutoCancelSubscriptionOnFailedRefund is true
// AND Config.PaymentIntentToSubscriptionResolver is supplied.
//
// Without the resolver, the function logs a warning and returns nil
// (it's not safe to guess which sub to cancel).
func CancelSubscriptionFromFailedRefund(ctx context.Context, ops *Operations, resolver PaymentIntentToSubscriptionResolver, evt webhooks.Event, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	if evt.Raw == nil || evt.Raw.Data == nil || len(evt.Raw.Data.Raw) == 0 {
		return fmt.Errorf("CancelSubscriptionFromFailedRefund: empty event data")
	}
	var shape struct {
		PaymentIntent string `json:"payment_intent"`
		Status        string `json:"status"`
	}
	if err := json.Unmarshal(evt.Raw.Data.Raw, &shape); err != nil {
		return fmt.Errorf("CancelSubscriptionFromFailedRefund: parse: %w", err)
	}
	if shape.Status != "failed" {
		// charge.refund.updated fires on non-failure transitions too;
		// we act only on terminal failures.
		return nil
	}
	if shape.PaymentIntent == "" {
		return nil
	}
	if resolver == nil {
		logger.WarnContext(ctx, "CancelSubscriptionFromFailedRefund: no resolver configured; can't auto-cancel",
			"payment_intent", shape.PaymentIntent, "event_id", evt.ID)
		return nil
	}
	subID, err := resolver(ctx, shape.PaymentIntent)
	if err != nil {
		return fmt.Errorf("CancelSubscriptionFromFailedRefund: resolver: %w", err)
	}
	if subID == "" {
		return nil // PI not tied to a sub
	}
	if err := ops.CancelNow(ctx, subID, CancelNowOptions{Reason: "refund_failed:" + evt.ID}); err != nil {
		return fmt.Errorf("CancelSubscriptionFromFailedRefund: cancel %s: %w", subID, err)
	}
	logger.InfoContext(ctx, "subscription auto-canceled after refund failure",
		"sub_id", subID, "payment_intent", shape.PaymentIntent, "event_id", evt.ID)
	return nil
}
