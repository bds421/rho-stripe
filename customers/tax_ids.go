package customers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bds421/rho-stripe/stripeapi"
	stripe "github.com/stripe/stripe-go/v82"
)

// TaxID represents a customer-attached tax registration. Used for B2B
// invoicing — Stripe puts the customer's tax ID on the rendered
// invoice PDF and (for EU VAT) validates it against the VIES system.
//
// Stripe accepts ~100 tax-ID types. Use stripe's constants like
// "eu_vat", "gb_vat", "au_abn", "us_ein", "ca_bn", "br_cnpj", etc.
// See https://docs.stripe.com/api/customer_tax_ids for the full list.
type TaxID struct {
	StripeID         string // ti_…
	StripeCustomerID string // cus_…
	Type             string // e.g. "eu_vat", "us_ein"
	Value            string // the registration number itself
	Country          string // 2-letter ISO; populated by Stripe after validation
	Verification     string // "pending" | "verified" | "unverified" | "" (not yet checked)
	VerificationName string // for EU VAT: name VIES has on file
	Created          time.Time
}

// AddTaxIDInput configures a tax-ID registration on a customer.
type AddTaxIDInput struct {
	SubjectID SubjectID
	// Type is the Stripe tax-ID type constant (e.g. "eu_vat").
	Type string
	// Value is the registration number (e.g. "DE123456789").
	Value string
}

// AddTaxID attaches a tax registration to the customer's Stripe
// record. Stripe validates the format synchronously; for EU VAT it
// queues an asynchronous check against the VIES system whose result
// arrives later via customer.tax_id.updated webhook.
func (o *Operations) AddTaxID(ctx context.Context, in AddTaxIDInput) (*TaxID, error) {
	if in.SubjectID == "" {
		return nil, errors.New("customers.AddTaxID: SubjectID is required")
	}
	if in.Type == "" || in.Value == "" {
		return nil, errors.New("customers.AddTaxID: Type and Value are required")
	}
	stripeID, err := o.resolveStripeID(ctx, in.SubjectID)
	if err != nil {
		return nil, err
	}
	params := &stripe.TaxIDCreateParams{
		Customer: stripe.String(string(stripeID)),
		Type:     stripe.String(in.Type),
		Value:    stripe.String(in.Value),
	}
	// Idempotency-key derived from customer+type+value: re-adding
	// the same registration is a no-op (returns the existing record
	// within Stripe's 24h dedup window).
	stripeapi.ApplyIdempotencyKey(params, ctx, "customers.tax_id.add",
		string(stripeID), in.Type, in.Value)
	created, err := o.cfg.StripeClient.V1TaxIDs.Create(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("customers.AddTaxID: TaxIDs.Create: %w", err)
	}
	return projectTaxID(created, string(stripeID)), nil
}

// ListTaxIDs returns every tax registration on the customer.
func (o *Operations) ListTaxIDs(ctx context.Context, s SubjectID) ([]*TaxID, error) {
	stripeID, err := o.resolveStripeID(ctx, s)
	if err != nil {
		return nil, err
	}
	params := &stripe.TaxIDListParams{Customer: stripe.String(string(stripeID))}
	var out []*TaxID
	for t, err := range o.cfg.StripeClient.V1TaxIDs.List(ctx, params) {
		if err != nil {
			return nil, fmt.Errorf("customers.ListTaxIDs: %w", err)
		}
		out = append(out, projectTaxID(t, string(stripeID)))
	}
	return out, nil
}

// RemoveTaxID detaches a tax registration. The TaxID's Stripe id (ti_…)
// is required; lookup via ListTaxIDs if the caller only has the value.
func (o *Operations) RemoveTaxID(ctx context.Context, s SubjectID, taxIDStripeID string) error {
	if taxIDStripeID == "" {
		return errors.New("customers.RemoveTaxID: taxIDStripeID is required")
	}
	stripeID, err := o.resolveStripeID(ctx, s)
	if err != nil {
		return err
	}
	params := &stripe.TaxIDDeleteParams{Customer: stripe.String(string(stripeID))}
	if _, err := o.cfg.StripeClient.V1TaxIDs.Delete(ctx, taxIDStripeID, params); err != nil {
		return fmt.Errorf("customers.RemoveTaxID: TaxIDs.Delete(%s): %w", taxIDStripeID, err)
	}
	return nil
}

func projectTaxID(t *stripe.TaxID, customerID string) *TaxID {
	out := &TaxID{
		StripeID:         t.ID,
		StripeCustomerID: customerID,
		Type:             string(t.Type),
		Value:            t.Value,
		Country:          t.Country,
		Created:          time.Unix(t.Created, 0).UTC(),
	}
	if t.Verification != nil {
		out.Verification = string(t.Verification.Status)
		out.VerificationName = t.Verification.VerifiedName
	}
	return out
}
