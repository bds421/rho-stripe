package subscriptions

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/bds421/rho-stripe/webhooks"
)

// PriceKeyResolver reverses Stripe price ids back to catalog logical
// keys. Production: *catalog.Cache; tests inject fakes.
type PriceKeyResolver interface {
	// PriceKeyByStripeID returns the namespaced lookup_key for a
	// stripe price id, or ("", false) if unknown.
	PriceKeyByStripeID(stripeID string) (string, bool)
}

// ApplyEventToMirror inspects evt and, if it's a subscription
// lifecycle event, parses the inner object into a Subscription and
// upserts the repo. Out-of-order events are filtered by the repo
// (via StripeUpdatedAt watermark).
//
// The connector facade wires this as a pre-step on the typed
// OnSubscription* handlers (similar to credits.ApplyGrantsFromSession
// on checkout.session.completed). Apps don't call it directly.
//
// resolver may be nil — items whose price isn't in the catalog get
// PriceKey = "stripe:<id>" and a warning logged.
func ApplyEventToMirror(ctx context.Context, repo SubscriptionRepo, resolver PriceKeyResolver, evt webhooks.Event, logger *slog.Logger) error {
	if repo == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}

	switch evt.Type {
	case "customer.subscription.created",
		"customer.subscription.updated",
		"customer.subscription.deleted",
		"customer.subscription.paused",
		"customer.subscription.resumed":
		// fall through
	default:
		return nil
	}

	sub, err := parseSubscription(ctx, evt, resolver, logger)
	if err != nil {
		return fmt.Errorf("subscriptions: parse subscription from %s: %w", evt.Type, err)
	}
	if sub.SubjectID == "" {
		// No subject metadata — likely a subscription created outside
		// the lib's checkout flow (admin-created via dashboard, etc.).
		// Mirror still useful but apps can't route it to an org.
		logger.WarnContext(ctx, "subscriptions: subscription has no subject_id metadata",
			"stripe_id", sub.StripeID, "event_id", evt.ID)
	}

	// .deleted always lands in canceled with EndedAt set, regardless
	// of what the payload says (defensive — Stripe always sets these
	// but the contract is clearer if we enforce it here too).
	if evt.Type == "customer.subscription.deleted" {
		sub.Status = StatusCanceled
		now := time.Now()
		if sub.EndedAt == nil {
			sub.EndedAt = &now
		}
	}

	sub.UpdatedAt = time.Now()
	sub.StripeUpdatedAt = time.Unix(evt.CreatedUnix, 0)
	return repo.Upsert(ctx, sub)
}

// subscriptionShape mirrors only the fields the lib actually reads.
// Avoids carrying stripe-go's *Subscription struct through downstream
// code (ADR-0008: wrappers over re-exports).
type subscriptionShape struct {
	ID                string                     `json:"id"`
	Status            string                     `json:"status"`
	Customer          string                     `json:"customer"`
	CancelAtPeriodEnd bool                       `json:"cancel_at_period_end"`
	CanceledAt        int64                      `json:"canceled_at"`
	EndedAt           int64                      `json:"ended_at"`
	TrialStart        int64                      `json:"trial_start"`
	TrialEnd          int64                      `json:"trial_end"`
	Metadata          map[string]string          `json:"metadata"`
	Items             *subscriptionItemListShape `json:"items"`

	// LatestInvoice is sometimes an expanded object, sometimes just
	// an id string. We unmarshal as RawMessage and parse defensively.
	LatestInvoice json.RawMessage `json:"latest_invoice"`
}

type latestInvoiceShape struct {
	Status             string `json:"status"`
	AttemptCount       int64  `json:"attempt_count"`
	NextPaymentAttempt int64  `json:"next_payment_attempt"`
}

type subscriptionItemListShape struct {
	Data []subscriptionItemShape `json:"data"`
}

type subscriptionItemShape struct {
	ID                 string                  `json:"id"`
	Quantity           int64                   `json:"quantity"`
	CurrentPeriodStart int64                   `json:"current_period_start"`
	CurrentPeriodEnd   int64                   `json:"current_period_end"`
	Price              *subscriptionPriceShape `json:"price"`
}

type subscriptionPriceShape struct {
	ID        string `json:"id"`
	LookupKey string `json:"lookup_key"`
}

func parseSubscription(ctx context.Context, evt webhooks.Event, resolver PriceKeyResolver, logger *slog.Logger) (*Subscription, error) {
	if evt.Raw == nil || evt.Raw.Data == nil || len(evt.Raw.Data.Raw) == 0 {
		return nil, fmt.Errorf("event %s has no data", evt.ID)
	}
	var shape subscriptionShape
	if err := json.Unmarshal(evt.Raw.Data.Raw, &shape); err != nil {
		return nil, fmt.Errorf("unmarshal subscription: %w", err)
	}

	sub := &Subscription{
		StripeID:          shape.ID,
		SubjectID:         SubjectID(shape.Metadata["subject_id"]),
		StripeCustomerID:  shape.Customer,
		Status:            Status(shape.Status),
		CancelAtPeriodEnd: shape.CancelAtPeriodEnd,
		Metadata:          shape.Metadata,
	}

	sub.CanceledAt = unixToTimePtr(shape.CanceledAt)
	sub.EndedAt = unixToTimePtr(shape.EndedAt)
	sub.TrialStart = unixToTimePtr(shape.TrialStart)
	sub.TrialEnd = unixToTimePtr(shape.TrialEnd)

	// Parse latest_invoice — may be expanded object OR plain id string.
	// Only the expanded form gives us dunning data.
	if len(shape.LatestInvoice) > 0 && shape.LatestInvoice[0] == '{' {
		var inv latestInvoiceShape
		if err := json.Unmarshal(shape.LatestInvoice, &inv); err == nil && inv.Status != "" {
			sub.LatestInvoice = &LatestInvoiceState{
				Status:             inv.Status,
				AttemptCount:       inv.AttemptCount,
				NextPaymentAttempt: unixToTimePtr(inv.NextPaymentAttempt),
			}
		}
	}

	if shape.Items != nil {
		for _, raw := range shape.Items.Data {
			item := SubscriptionItem{
				StripeID: raw.ID,
				Quantity: raw.Quantity,
			}
			if raw.Price != nil {
				// resolvePriceKey always returns a non-empty string
				// (worst-case "stripe:<id>") — no nil-check needed.
				item.PriceKey = resolvePriceKey(ctx, raw.Price, resolver, logger, sub.StripeID)
			}
			sub.Items = append(sub.Items, item)

			// Period lives on items in stripe-go v82; use the first item's.
			if sub.CurrentPeriodStart.IsZero() && raw.CurrentPeriodStart > 0 {
				sub.CurrentPeriodStart = time.Unix(raw.CurrentPeriodStart, 0)
				sub.CurrentPeriodEnd = time.Unix(raw.CurrentPeriodEnd, 0)
			}
		}
	}

	return sub, nil
}

func resolvePriceKey(ctx context.Context, price *subscriptionPriceShape, resolver PriceKeyResolver, logger *slog.Logger, subID string) string {
	if resolver != nil {
		if key, ok := resolver.PriceKeyByStripeID(price.ID); ok {
			return key
		}
		// Catalog cache might be cold or the price was added out-of-band
		// (created in dashboard, not via sync). The lookup_key on the
		// Stripe Price object itself is our best fallback.
		if price.LookupKey != "" {
			return price.LookupKey
		}
		logger.WarnContext(ctx, "subscriptions: price not in catalog and has no lookup_key; using stripe id",
			"price_id", price.ID, "subscription", subID)
	} else if price.LookupKey != "" {
		return price.LookupKey
	}
	return "stripe:" + price.ID
}

func unixToTimePtr(unix int64) *time.Time {
	if unix == 0 {
		return nil
	}
	t := time.Unix(unix, 0)
	return &t
}
