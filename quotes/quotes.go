// Package quotes wraps Stripe's Quote API for B2B sales workflows.
//
// Typical flow:
//
//  1. Sales rep prepares a quote (line items, discounts, expiry).
//     conn.Quotes.Create(...)
//
//  2. Send the quote to the customer (Stripe emails a PDF).
//     conn.Quotes.Finalize(...) — locks the quote.
//
//  3. Customer accepts via the link in the email, or the rep accepts
//     on their behalf.
//     conn.Quotes.Accept(...)
//
//  4. Acceptance auto-creates the resulting Subscription (or Invoice
//     for one-shot quotes). The lib's subscription-mirror catches up
//     via the customer.subscription.created webhook.
//
// Apps that don't do sales-led motion don't need this package.
package quotes

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/bds421/rho-stripe/meta"
	"github.com/bds421/rho-stripe/stripeapi"
	stripe "github.com/stripe/stripe-go/v82"
)

// Config wires the package's dependencies. Matches the lib-wide
// convention (customers, paymentmethods, checkout, webhooks) of taking
// a typed Config when any operation needs more than the Stripe client.
type Config struct {
	// StripeClient is the configured stripe-go client. Required.
	StripeClient *stripe.Client

	// Namespace stamps metadata.app_namespace on every Quote the lib
	// creates so the webhook dispatcher can route the resulting events
	// back to this app. Optional in test setups; the connector facade
	// always passes the configured AppNamespace.
	Namespace string
}

// Operations is the entry point.
type Operations struct {
	sc        *stripe.Client
	namespace string
}

// New constructs Operations from cfg. Panics if cfg.StripeClient is nil.
func New(cfg Config) *Operations {
	if cfg.StripeClient == nil {
		panic("quotes.New: StripeClient is required")
	}
	return &Operations{sc: cfg.StripeClient, namespace: cfg.Namespace}
}

// CreateInput configures a new Quote.
type CreateInput struct {
	// StripeCustomerID identifies the recipient. Required.
	StripeCustomerID string

	// LineItems lists what's being quoted. Each item references a
	// Stripe Price id (use catalog cache to resolve from logical key).
	// Required (>=1).
	LineItems []QuoteLine

	// Description appears on the rendered PDF. Optional.
	Description string

	// Footer appears at the bottom of the PDF. Optional (uses template
	// default).
	Footer string

	// Header is the top-line label. Optional.
	Header string

	// ExpiresAt sets when the quote expires. Defaults to Stripe's
	// account-default (typically 30 days).
	ExpiresAt time.Time

	// CollectionMethod controls payment-collection behavior after
	// acceptance: "charge_automatically" (default) | "send_invoice".
	CollectionMethod string

	// TrialPeriodDays applies a free trial on the resulting
	// subscription (only relevant when line items contain recurring
	// prices).
	TrialPeriodDays int

	// Metadata is forwarded to the Quote.
	Metadata map[string]string
}

// QuoteLine is one line item on the quote.
type QuoteLine struct {
	StripePriceID string
	Quantity      int64
}

// Quote is the lib's stable projection of stripe.Quote.
type Quote struct {
	StripeID         string
	Number           string
	Status           string // "draft" | "open" | "accepted" | "canceled"
	StripeCustomerID string
	AmountTotal      int64
	AmountSubtotal   int64
	Currency         string
	ExpiresAt        time.Time
	CreatedAt        time.Time
	PDFURL           string // populated after finalize via separate API call
	SubscriptionID   string // populated after accept
	InvoiceID        string // populated after accept (for one-shot quotes)
}

// Create makes a draft Quote. It is NOT yet visible to the customer —
// call Finalize next.
func (o *Operations) Create(ctx context.Context, in CreateInput) (*Quote, error) {
	if in.StripeCustomerID == "" {
		return nil, errors.New("quotes.Create: StripeCustomerID is required")
	}
	if len(in.LineItems) == 0 {
		return nil, errors.New("quotes.Create: at least one LineItem is required")
	}
	params := &stripe.QuoteCreateParams{
		Customer: stripe.String(in.StripeCustomerID),
	}
	if in.Description != "" {
		params.Description = stripe.String(in.Description)
	}
	if in.Footer != "" {
		params.Footer = stripe.String(in.Footer)
	}
	if in.Header != "" {
		params.Header = stripe.String(in.Header)
	}
	if !in.ExpiresAt.IsZero() {
		params.ExpiresAt = stripe.Int64(in.ExpiresAt.Unix())
	}
	if in.CollectionMethod != "" {
		params.CollectionMethod = stripe.String(in.CollectionMethod)
	}
	if in.TrialPeriodDays > 0 {
		params.SubscriptionData = &stripe.QuoteCreateSubscriptionDataParams{
			TrialPeriodDays: stripe.Int64(int64(in.TrialPeriodDays)),
		}
	}
	for _, li := range in.LineItems {
		qty := li.Quantity
		if qty <= 0 {
			qty = 1
		}
		params.LineItems = append(params.LineItems, &stripe.QuoteCreateLineItemParams{
			Price:    stripe.String(li.StripePriceID),
			Quantity: stripe.Int64(qty),
		})
	}
	if o.namespace != "" {
		params.AddMetadata(meta.MetadataKeyNamespace, o.namespace)
	}
	for k, v := range in.Metadata {
		params.AddMetadata(k, v)
	}
	stripeapi.ApplyIdempotencyKey(params, ctx, "quotes.create",
		in.StripeCustomerID, createInputHash(in))
	created, err := o.sc.V1Quotes.Create(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("quotes.Create: Quotes.Create: %w", err)
	}
	return projectQuote(created), nil
}

// Finalize locks the quote — line items can no longer change. The
// quote becomes visible to the customer at its hosted URL. Apps
// typically send the URL via their own email; Stripe doesn't auto-
// email finalized quotes (unlike invoices).
func (o *Operations) Finalize(ctx context.Context, quoteID string) (*Quote, error) {
	if quoteID == "" {
		return nil, errors.New("quotes.Finalize: quoteID is required")
	}
	params := &stripe.QuoteFinalizeQuoteParams{}
	stripeapi.ApplyIdempotencyKey(params, ctx, "quotes.finalize", quoteID)
	finalized, err := o.sc.V1Quotes.FinalizeQuote(ctx, quoteID, params)
	if err != nil {
		return nil, fmt.Errorf("quotes.Finalize(%s): %w", quoteID, err)
	}
	return projectQuote(finalized), nil
}

// Accept moves the quote to accepted state and triggers
// Subscription/Invoice creation. Typically called server-side after
// the customer clicks "accept" in the hosted UI; some apps use it to
// auto-accept on behalf of the customer (e.g. for internal-account
// quotes that bypass external sign-off).
func (o *Operations) Accept(ctx context.Context, quoteID string) (*Quote, error) {
	if quoteID == "" {
		return nil, errors.New("quotes.Accept: quoteID is required")
	}
	params := &stripe.QuoteAcceptParams{}
	stripeapi.ApplyIdempotencyKey(params, ctx, "quotes.accept", quoteID)
	accepted, err := o.sc.V1Quotes.Accept(ctx, quoteID, params)
	if err != nil {
		return nil, fmt.Errorf("quotes.Accept(%s): %w", quoteID, err)
	}
	return projectQuote(accepted), nil
}

// Cancel invalidates a draft / open quote. Already-accepted quotes
// cannot be canceled (they have downstream subscriptions/invoices).
func (o *Operations) Cancel(ctx context.Context, quoteID string) (*Quote, error) {
	if quoteID == "" {
		return nil, errors.New("quotes.Cancel: quoteID is required")
	}
	params := &stripe.QuoteCancelParams{}
	stripeapi.ApplyIdempotencyKey(params, ctx, "quotes.cancel", quoteID)
	canceled, err := o.sc.V1Quotes.Cancel(ctx, quoteID, params)
	if err != nil {
		return nil, fmt.Errorf("quotes.Cancel(%s): %w", quoteID, err)
	}
	return projectQuote(canceled), nil
}

// Retrieve fetches a Quote by id.
func (o *Operations) Retrieve(ctx context.Context, quoteID string) (*Quote, error) {
	if quoteID == "" {
		return nil, errors.New("quotes.Retrieve: quoteID is required")
	}
	q, err := o.sc.V1Quotes.Retrieve(ctx, quoteID, nil)
	if err != nil {
		return nil, fmt.Errorf("quotes.Retrieve(%s): %w", quoteID, err)
	}
	return projectQuote(q), nil
}

func projectQuote(q *stripe.Quote) *Quote {
	out := &Quote{
		StripeID:       q.ID,
		Number:         q.Number,
		Status:         string(q.Status),
		AmountTotal:    q.AmountTotal,
		AmountSubtotal: q.AmountSubtotal,
		Currency:       string(q.Currency),
		CreatedAt:      time.Unix(q.Created, 0).UTC(),
	}
	if q.Customer != nil {
		out.StripeCustomerID = q.Customer.ID
	}
	if q.ExpiresAt > 0 {
		out.ExpiresAt = time.Unix(q.ExpiresAt, 0).UTC()
	}
	if q.Subscription != nil {
		out.SubscriptionID = q.Subscription.ID
	}
	if q.Invoice != nil {
		out.InvoiceID = q.Invoice.ID
	}
	return out
}

// createInputHash hashes every field that influences the resulting
// Stripe Quote, so two calls with even one differing field get
// distinct idempotency keys.
//
// Earlier slice-51 implementation hashed only the line items, which
// meant a retry with a different TrialPeriodDays would silently
// return the original quote — a real-bug class.
func createInputHash(in CreateInput) string {
	fields := map[string]string{
		"Description":      in.Description,
		"Footer":           in.Footer,
		"Header":           in.Header,
		"CollectionMethod": in.CollectionMethod,
		"TrialPeriodDays":  strconv.Itoa(in.TrialPeriodDays),
	}
	if !in.ExpiresAt.IsZero() {
		fields["ExpiresAt"] = strconv.FormatInt(in.ExpiresAt.Unix(), 10)
	}
	// Encode line items as a single canonical string so the field-map
	// representation stays flat; ContentHash sorts its keys but does
	// not recurse.
	parts := make([]string, 0, len(in.LineItems))
	for _, li := range in.LineItems {
		parts = append(parts, li.StripePriceID+"x"+strconv.FormatInt(li.Quantity, 10))
	}
	fields["LineItems"] = stripeapi.CanonicalStrings(parts)
	for k, v := range in.Metadata {
		fields["meta:"+k] = v
	}
	return stripeapi.ContentHash("quotes.create", fields)
}
