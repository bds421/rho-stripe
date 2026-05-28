package credits

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/bds421/rho-stripe/webhooks"
)

// ApplyGrantsFromSession is the webhook auto-handler that turns a
// completed Checkout Session into ledger Grant rows. It reads the
// session's metadata for the encoded PendingGrant list, then calls
// repo.Grant once per grant per quantity unit.
//
// Idempotency: each Grant call passes SourceRef = PaymentIntent id +
// grant index, so repeat webhook deliveries (Stripe retries, manual
// replays) don't create duplicate ledger entries — provided the
// repo's Grant honors the (Subject, Bucket, SourceRef) dedup contract.
func ApplyGrantsFromSession(ctx context.Context, repo CreditRepo, evt webhooks.Event, logger *slog.Logger) error {
	if repo == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	if evt.Type != "checkout.session.completed" && evt.Type != "checkout.session.async_payment_succeeded" {
		return nil
	}

	session, err := decodeSession(evt)
	if err != nil {
		return fmt.Errorf("credits: parse session: %w", err)
	}

	metadataValue := session.Metadata[SessionMetadataKey]
	if metadataValue == "" {
		return nil // session had no credit grants
	}
	grants, err := DecodeSessionMetadata(metadataValue)
	if err != nil {
		return err
	}
	if len(grants) == 0 {
		return nil
	}

	subjectID := SubjectID(session.Metadata["subject_id"])
	if subjectID == "" {
		return fmt.Errorf("credits: session %s missing subject_id metadata", session.ID)
	}

	for i, g := range grants {
		quantity := g.Quantity
		if quantity <= 0 {
			quantity = 1
		}
		for unit := 0; unit < quantity; unit++ {
			input := GrantInput{
				SubjectID:   subjectID,
				Bucket:    g.Bucket,
				Amount:    g.Amount,
				ValidDays: g.ValidDays,
				Source:    SourceStripePayment,
				SourceRef: fmt.Sprintf("%s:%d:%d", session.PaymentIntent, i, unit),
				Metadata: map[string]string{
					"checkout_session": session.ID,
					"product_key":      g.ProductKey,
				},
			}
			if _, err := repo.Grant(ctx, input); err != nil {
				return fmt.Errorf("credits: grant for session %s (grant %d unit %d): %w",
					session.ID, i, unit, err)
			}
			logger.InfoContext(ctx, "credits: granted",
				"subject", subjectID,
				"bucket", g.Bucket,
				"amount", g.Amount,
				"product_key", g.ProductKey,
				"session", session.ID)
		}
	}
	return nil
}

// sessionShape is the slice of a Checkout Session the auto-handler
// reads. Decoding only the fields we need avoids tracking the full
// stripe.CheckoutSession struct's evolution.
type sessionShape struct {
	ID            string            `json:"id"`
	Metadata      map[string]string `json:"metadata"`
	PaymentIntent string            `json:"payment_intent"`
}

func decodeSession(evt webhooks.Event) (*sessionShape, error) {
	if evt.Raw == nil || evt.Raw.Data == nil || len(evt.Raw.Data.Raw) == 0 {
		return nil, fmt.Errorf("event has empty data payload")
	}
	var s sessionShape
	if err := json.Unmarshal(evt.Raw.Data.Raw, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
