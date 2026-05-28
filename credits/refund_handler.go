package credits

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/bds421/rho-stripe/webhooks"
)

// ApplyRefundReversal is the auto-handler that revokes credit grants
// tied to a refunded charge / payment_intent. The connector wraps the
// app's OnRefundCreated with this when Config.AutoRevokeCreditsOnRefund
// is true.
//
// Resolution order for the source-ref to look up:
//
//  1. evt.Data.Object["payment_intent"] (most accurate; refunds carry it)
//  2. evt.Data.Object["charge"] (fallback)
//  3. evt.Data.Object["id"] (the refund's own id — last resort, only matches
//     grants that recorded the refund id as their SourceRef, rare)
//
// Each found grant is RevokeGrant'd with reason="refund:<refund-id>".
// Idempotent — re-deliveries of the same refund event re-revoke
// already-revoked grants harmlessly (RevokeGrant is a no-op for
// already-zero rows).
//
// Returns nil even when no grants matched (refund of a payment that
// didn't grant credits is normal — apps refund subscription invoices,
// not just credit-pack purchases).
func ApplyRefundReversal(ctx context.Context, repo CreditRepo, evt webhooks.Event, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	refundID, pi, charge, err := parseRefundEvent(evt)
	if err != nil {
		logger.WarnContext(ctx, "credits.ApplyRefundReversal: parse refund failed",
			"err", err, "event_id", evt.ID)
		return nil // don't break the handler chain for parse errors
	}

	candidates := []string{}
	if pi != "" {
		candidates = append(candidates, pi)
	}
	if charge != "" {
		candidates = append(candidates, charge)
	}
	if refundID != "" {
		candidates = append(candidates, refundID)
	}

	var revoked int
	for _, ref := range candidates {
		grants, err := repo.FindGrantsBySourceRef(ctx, ref)
		if err != nil {
			return fmt.Errorf("credits.ApplyRefundReversal: lookup %q: %w", ref, err)
		}
		for _, g := range grants {
			if g.Metadata != nil && g.Metadata["revoked_reason"] != "" {
				continue // already revoked (re-delivery)
			}
			if err := repo.RevokeGrant(ctx, g.ID, "refund:"+refundID); err != nil {
				return fmt.Errorf("credits.ApplyRefundReversal: revoke grant %s: %w", g.ID, err)
			}
			revoked++
			logger.InfoContext(ctx, "credits.ApplyRefundReversal: revoked grant",
				"grant_id", g.ID, "subject", g.SubjectID, "bucket", g.Bucket,
				"amount_initial", g.AmountInitial, "refund_id", refundID)
		}
	}
	if revoked == 0 {
		logger.DebugContext(ctx, "credits.ApplyRefundReversal: no grants matched",
			"event_id", evt.ID, "refund_id", refundID, "tried_refs", candidates)
	}
	return nil
}

// parseRefundEvent extracts (refundID, paymentIntentID, chargeID) from
// the event's data.object. Stripe sends slightly different shapes for
// "charge.refunded" vs "refund.created" — this normalizes both.
func parseRefundEvent(evt webhooks.Event) (refundID, paymentIntent, charge string, err error) {
	if evt.Raw == nil || evt.Raw.Data == nil || len(evt.Raw.Data.Raw) == 0 {
		return "", "", "", fmt.Errorf("empty event data")
	}
	var shape struct {
		ID            string `json:"id"`
		Object        string `json:"object"`
		PaymentIntent any    `json:"payment_intent"` // string OR object
		Charge        any    `json:"charge"`
		LatestRefund  any    `json:"latest_refund"` // sometimes set on charge.refunded
		Refunds       struct {
			Data []struct {
				ID            string `json:"id"`
				PaymentIntent any    `json:"payment_intent"`
				Charge        any    `json:"charge"`
			} `json:"data"`
		} `json:"refunds"`
	}
	if jsonErr := json.Unmarshal(evt.Raw.Data.Raw, &shape); jsonErr != nil {
		return "", "", "", jsonErr
	}

	switch shape.Object {
	case "refund":
		refundID = shape.ID
		paymentIntent = idFromAny(shape.PaymentIntent)
		charge = idFromAny(shape.Charge)
	case "charge":
		charge = shape.ID
		paymentIntent = idFromAny(shape.PaymentIntent)
		refundID = idFromAny(shape.LatestRefund)
		if refundID == "" && len(shape.Refunds.Data) > 0 {
			refundID = shape.Refunds.Data[len(shape.Refunds.Data)-1].ID
		}
	default:
		refundID = shape.ID
		paymentIntent = idFromAny(shape.PaymentIntent)
		charge = idFromAny(shape.Charge)
	}
	return refundID, paymentIntent, charge, nil
}

// idFromAny normalises Stripe's "field is sometimes a string, sometimes
// an expanded object" shape — Stripe webhook payloads can include either.
func idFromAny(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case map[string]any:
		if id, ok := x["id"].(string); ok {
			return id
		}
	}
	return ""
}
