package credits

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/bds421/rho-stripe/catalog"
	"github.com/bds421/rho-stripe/webhooks"
)

// ApplyRecurringGrantsFromInvoice is the webhook auto-handler that
// applies catalog-declared RecurringGrants when an invoice is paid.
// One Grant per (subscription, invoice, product) — idempotent because
// the SourceRef uses the invoice id, and the repo's Grant honors the
// (Subject, Bucket, SourceRef) dedup contract.
//
// The connector facade wires this as a pre-step on OnInvoicePaid when
// both Credits and a catalog Spec are configured. Apps don't call
// directly.
func ApplyRecurringGrantsFromInvoice(ctx context.Context, repo CreditRepo, spec *catalog.Spec, evt webhooks.Event, logger *slog.Logger) error {
	if repo == nil || spec == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	if evt.Type != "invoice.paid" && evt.Type != "invoice.payment_succeeded" {
		return nil
	}

	inv, err := decodeInvoice(evt)
	if err != nil {
		return fmt.Errorf("credits: parse invoice: %w", err)
	}
	if inv.Subscription == "" {
		return nil // not a subscription invoice
	}
	subjectID := SubjectID(inv.Metadata["subject_id"])
	if subjectID == "" {
		// Try subscription_details.metadata.subject_id — Stripe puts
		// subscription metadata there on invoice payloads.
		if inv.SubscriptionDetails != nil {
			subjectID = SubjectID(inv.SubscriptionDetails.Metadata["subject_id"])
		}
	}
	if subjectID == "" {
		logger.WarnContext(ctx, "credits: invoice.paid without subject_id metadata; cannot apply recurring grants",
			"invoice", inv.ID, "subscription", inv.Subscription)
		return nil
	}

	// Walk line items; for each line whose price's lookup_key matches
	// a catalog product with RecurringGrant, apply the grant.
	if inv.Lines == nil {
		return nil
	}
	for _, line := range inv.Lines.Data {
		if line.Price == nil || line.Price.LookupKey == "" {
			continue
		}
		productKey, _, ok := splitNamespacedKey(spec, line.Price.LookupKey)
		if !ok {
			continue
		}
		product, ok := spec.Products[productKey]
		if !ok || product.RecurringGrant == nil {
			continue
		}
		rg := product.RecurringGrant

		// SourceRef ties the grant to this specific invoice — re-deliveries
		// of the same invoice.paid event dedup at the repo.
		_, err := repo.Grant(ctx, GrantInput{
			SubjectID:   subjectID,
			Bucket:    rg.Bucket,
			Amount:    rg.Amount,
			ValidDays: rg.ValidDaysFromGrant,
			Source:    SourceStripePayment,
			SourceRef: "invoice:" + inv.ID + ":" + productKey,
			Metadata: map[string]string{
				"invoice":      inv.ID,
				"subscription": inv.Subscription,
				"product_key":  productKey,
			},
		})
		if err != nil {
			return fmt.Errorf("credits: apply recurring grant for product %s: %w", productKey, err)
		}
		logger.InfoContext(ctx, "credits: recurring grant applied",
			"subject", subjectID, "bucket", rg.Bucket, "amount", rg.Amount, "invoice", inv.ID, "product_key", productKey)
	}
	return nil
}

// invoiceShape captures only the fields the recurring-grant handler
// needs.
type invoiceShape struct {
	ID                  string                      `json:"id"`
	Subscription        string                      `json:"subscription"`
	Metadata            map[string]string           `json:"metadata"`
	SubscriptionDetails *invoiceSubscriptionDetails `json:"subscription_details"`
	Lines               *invoiceLineList            `json:"lines"`
}

type invoiceSubscriptionDetails struct {
	Metadata map[string]string `json:"metadata"`
}

type invoiceLineList struct {
	Data []invoiceLine `json:"data"`
}

type invoiceLine struct {
	Price *invoiceLinePrice `json:"price"`
}

type invoiceLinePrice struct {
	ID        string `json:"id"`
	LookupKey string `json:"lookup_key"`
}

func decodeInvoice(evt webhooks.Event) (*invoiceShape, error) {
	if evt.Raw == nil || evt.Raw.Data == nil || len(evt.Raw.Data.Raw) == 0 {
		return nil, fmt.Errorf("event has empty data")
	}
	var inv invoiceShape
	if err := json.Unmarshal(evt.Raw.Data.Raw, &inv); err != nil {
		return nil, err
	}
	return &inv, nil
}

// splitNamespacedKey parses "<namespace>.<product>.<price>" into
// (productKey, priceKey, ok=true) when the namespace matches the
// spec's, else ok=false.
func splitNamespacedKey(spec *catalog.Spec, lookupKey string) (string, string, bool) {
	prefix := spec.Namespace + "."
	if len(lookupKey) <= len(prefix) || lookupKey[:len(prefix)] != prefix {
		return "", "", false
	}
	rest := lookupKey[len(prefix):]
	for i := 0; i < len(rest); i++ {
		if rest[i] == '.' {
			return rest[:i], rest[i+1:], true
		}
	}
	return "", "", false
}
