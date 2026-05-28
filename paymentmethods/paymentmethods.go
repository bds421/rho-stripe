// Package paymentmethods covers card-on-file flows that don't go
// through Checkout: creating SetupIntents (so a customer can save a
// card without a purchase), listing / attaching / detaching saved
// payment methods, and choosing the default for invoices.
//
// Use cases:
//   - "Update payment method" in app settings
//   - Adding a card up-front for a trial that needs one later
//   - Multiple cards per customer (corporate + personal)
//   - B2B invoice flow where the card backs only failed-payment retries
//
// For first-payment flows (purchase + card collection in one step),
// use the Checkout package instead — Stripe Checkout's hosted page
// handles card-data PCI scope reduction automatically.
package paymentmethods

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bds421/rho-stripe/stripeapi"
	"github.com/bds421/rho-stripe/subject"
	stripe "github.com/stripe/stripe-go/v82"
)

// SubjectID re-exports [subject.ID] so paymentmethods API takes a
// package-native type. Same underlying type — assignment-compatible
// with checkout.SubjectID / credits.SubjectID / etc.
type SubjectID = subject.ID

// StripeCustomerID re-exports [subject.StripeCustomerID]; same underlying
// type as checkout.StripeCustomerID.
type StripeCustomerID = subject.StripeCustomerID

// Config wires the package's dependencies.
type Config struct {
	// StripeClient is the configured stripe-go client. Required.
	StripeClient *stripe.Client

	// CustomerRepo resolves SubjectID ↔ StripeCustomerID. Required.
	CustomerRepo subject.CustomerRepo
}

// Operations is the entry point. Construct via New, then call via
// conn.PaymentMethods.{CreateSetupIntent,List,Attach,Detach,SetDefault}.
type Operations struct {
	cfg Config
}

// New validates config and returns an Operations. Panics on missing
// required deps — matches the lib-wide convention (webhooks.New,
// subscriptions.New, etc.) since misconfiguration at startup should
// crash, not return a recoverable error.
func New(cfg Config) *Operations {
	if cfg.StripeClient == nil {
		panic("paymentmethods.New: StripeClient is required")
	}
	if cfg.CustomerRepo == nil {
		panic("paymentmethods.New: CustomerRepo is required")
	}
	return &Operations{cfg: cfg}
}

// SetupIntent is the lib's projection of stripe.SetupIntent. The
// ClientSecret is what the app hands to the Stripe.js Elements UI on
// the frontend; the customer enters card details there, then Stripe
// confirms the SetupIntent and attaches the resulting PaymentMethod
// to the Customer.
type SetupIntent struct {
	StripeID     string
	ClientSecret string
	Status       string // "requires_payment_method" | "requires_confirmation" | "requires_action" | "succeeded" | ...
	CreatedAt    time.Time

	// NextAction surfaces what the customer must do to complete the
	// flow. Non-nil only when Status == "requires_action" — typically
	// a 3DS / SCA challenge. Nil otherwise.
	//
	// The frontend usually handles NextAction transparently via
	// stripe.confirmCardSetup(); apps that build a custom UI need to
	// inspect Type + RedirectToURL themselves.
	NextAction *SetupIntentNextAction
}

// SetupIntentNextAction mirrors stripe.SetupIntentNextAction.
//
// Stripe defines ~15 next_action types: "redirect_to_url",
// "use_stripe_sdk", "verify_with_microdeposits", "wechat_pay_display_qr_code",
// "boleto_display_details", "konbini_display_details", "oxxo_display_details",
// etc. This struct only surfaces RedirectToURL because that's the
// 90%-case for card-on-file flows. Apps needing other action types
// should inspect Type and reach for stripe-go's raw SetupIntent via
// conn.Stripe.SetupIntents.Get — the lib's projection is intentionally
// minimal to keep the surface stable.
type SetupIntentNextAction struct {
	Type          string // see stripe.SetupIntentNextActionType for full list
	RedirectToURL string // populated when Type == "redirect_to_url"
	ReturnURL     string // where Stripe redirects after auth
}

// CreateSetupIntentInput configures a SetupIntent for a subject.
type CreateSetupIntentInput struct {
	SubjectID SubjectID
	// Usage controls how the resulting PaymentMethod can be reused.
	// "off_session" (default) — for future automatic charges (subs).
	// "on_session" — only when the customer is present (one-shot).
	Usage string
	// PaymentMethodTypes filters the Elements UI. Defaults to ["card"].
	PaymentMethodTypes []string
	// Metadata is forwarded to the SetupIntent.
	Metadata map[string]string
}

// CreateSetupIntent provisions a SetupIntent the frontend can confirm.
// Idempotent on (subject, usage) — re-calling within Stripe's 24h
// dedup window returns the same SetupIntent.
func (o *Operations) CreateSetupIntent(ctx context.Context, in CreateSetupIntentInput) (*SetupIntent, error) {
	if in.SubjectID == "" {
		return nil, errors.New("paymentmethods.CreateSetupIntent: SubjectID is required")
	}
	customerID, err := o.resolveCustomer(ctx, in.SubjectID)
	if err != nil {
		return nil, err
	}
	usage := in.Usage
	if usage == "" {
		usage = "off_session"
	}
	pmTypes := in.PaymentMethodTypes
	if len(pmTypes) == 0 {
		pmTypes = []string{"card"}
	}
	params := &stripe.SetupIntentCreateParams{
		Customer: stripe.String(string(customerID)),
		Usage:    stripe.String(usage),
	}
	for _, t := range pmTypes {
		params.PaymentMethodTypes = append(params.PaymentMethodTypes, stripe.String(t))
	}
	for k, v := range in.Metadata {
		params.AddMetadata(k, v)
	}
	stripeapi.ApplyIdempotencyKey(params, ctx, "paymentmethods.setup_intent.create",
		string(customerID), usage)
	si, err := o.cfg.StripeClient.V1SetupIntents.Create(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("paymentmethods.CreateSetupIntent: SetupIntents.Create: %w", err)
	}
	return projectSetupIntent(si), nil
}

// PaymentMethod projects stripe.PaymentMethod into a stable shape.
type PaymentMethod struct {
	StripeID         string
	StripeCustomerID string
	Type             string
	Card             *CardSnapshot
	IsDefault        bool
	CreatedAt        time.Time
	Metadata         map[string]string
}

// CardSnapshot mirrors stripe.PaymentMethodCard for "type=card" pms.
type CardSnapshot struct {
	Brand    string
	Last4    string
	ExpMonth int64
	ExpYear  int64
}

// List returns every saved payment method for the
// subject, marking which is the customer's default for invoices.
//
// When types is empty, defaults to ["card"]. Pass multiple types to
// surface non-card payment methods too (e.g. ["card", "link",
// "sepa_debit", "us_bank_account"]). Stripe requires one List call
// per type, so we walk the list per-type and merge.
func (o *Operations) List(ctx context.Context, s SubjectID, types ...string) ([]*PaymentMethod, error) {
	customerID, err := o.resolveCustomer(ctx, s)
	if err != nil {
		return nil, err
	}
	if len(types) == 0 {
		types = []string{"card"}
	}
	cust, err := o.cfg.StripeClient.V1Customers.Retrieve(ctx, string(customerID), nil)
	if err != nil {
		return nil, fmt.Errorf("paymentmethods.List: Customers.Retrieve: %w", err)
	}
	var defaultPMID string
	if cust.InvoiceSettings != nil && cust.InvoiceSettings.DefaultPaymentMethod != nil {
		defaultPMID = cust.InvoiceSettings.DefaultPaymentMethod.ID
	}

	var out []*PaymentMethod
	for _, pmType := range types {
		listParams := &stripe.PaymentMethodListParams{
			Customer: stripe.String(string(customerID)),
			Type:     stripe.String(pmType),
		}
		listParams.Filters.AddFilter("limit", "", "100")
		for pm, err := range o.cfg.StripeClient.V1PaymentMethods.List(ctx, listParams) {
			if err != nil {
				return nil, fmt.Errorf("paymentmethods.List(type=%s): %w", pmType, err)
			}
			proj := projectPaymentMethod(pm, string(customerID))
			if pm.ID == defaultPMID {
				proj.IsDefault = true
			}
			out = append(out, proj)
		}
	}
	return out, nil
}

// AttachPaymentMethod attaches an existing Stripe PaymentMethod (e.g.
// one created via SetupIntent confirmation on the frontend) to the
// customer. Use when the frontend hands back a payment_method id that
// isn't already attached.
func (o *Operations) AttachPaymentMethod(ctx context.Context, s SubjectID, paymentMethodID string) error {
	if paymentMethodID == "" {
		return errors.New("paymentmethods.AttachPaymentMethod: paymentMethodID is required")
	}
	customerID, err := o.resolveCustomer(ctx, s)
	if err != nil {
		return err
	}
	params := &stripe.PaymentMethodAttachParams{
		Customer: stripe.String(string(customerID)),
	}
	stripeapi.ApplyIdempotencyKey(params, ctx, "paymentmethods.attach",
		string(customerID), paymentMethodID)
	if _, err := o.cfg.StripeClient.V1PaymentMethods.Attach(ctx, paymentMethodID, params); err != nil {
		return fmt.Errorf("paymentmethods.AttachPaymentMethod: %w", err)
	}
	return nil
}

// DetachPaymentMethod removes a payment method from the customer. The
// PaymentMethod itself remains in Stripe (detached). After detach, it
// cannot be used for new charges on this customer.
func (o *Operations) DetachPaymentMethod(ctx context.Context, paymentMethodID string) error {
	if paymentMethodID == "" {
		return errors.New("paymentmethods.DetachPaymentMethod: paymentMethodID is required")
	}
	params := &stripe.PaymentMethodDetachParams{}
	stripeapi.ApplyIdempotencyKey(params, ctx, "paymentmethods.detach", paymentMethodID)
	if _, err := o.cfg.StripeClient.V1PaymentMethods.Detach(ctx, paymentMethodID, params); err != nil {
		return fmt.Errorf("paymentmethods.DetachPaymentMethod: %w", err)
	}
	return nil
}

// SetDefaultPaymentMethod sets the customer's default payment method
// for invoices (Stripe uses this for subscription renewals). The PM
// must already be attached to the customer.
func (o *Operations) SetDefaultPaymentMethod(ctx context.Context, s SubjectID, paymentMethodID string) error {
	if paymentMethodID == "" {
		return errors.New("paymentmethods.SetDefaultPaymentMethod: paymentMethodID is required")
	}
	customerID, err := o.resolveCustomer(ctx, s)
	if err != nil {
		return err
	}
	params := &stripe.CustomerUpdateParams{
		InvoiceSettings: &stripe.CustomerUpdateInvoiceSettingsParams{
			DefaultPaymentMethod: stripe.String(paymentMethodID),
		},
	}
	stripeapi.ApplyIdempotencyKey(params, ctx, "paymentmethods.set_default",
		string(customerID), paymentMethodID)
	if _, err := o.cfg.StripeClient.V1Customers.Update(ctx, string(customerID), params); err != nil {
		return fmt.Errorf("paymentmethods.SetDefaultPaymentMethod: %w", err)
	}
	return nil
}

func (o *Operations) resolveCustomer(ctx context.Context, s SubjectID) (StripeCustomerID, error) {
	id, ok, err := o.cfg.CustomerRepo.Get(ctx, s)
	if err != nil {
		return "", fmt.Errorf("paymentmethods: resolve %s: %w", s, err)
	}
	if !ok {
		return "", fmt.Errorf("paymentmethods: no Stripe customer for subject %s", s)
	}
	return id, nil
}

func projectSetupIntent(si *stripe.SetupIntent) *SetupIntent {
	out := &SetupIntent{
		StripeID:     si.ID,
		ClientSecret: si.ClientSecret,
		Status:       string(si.Status),
		CreatedAt:    time.Unix(si.Created, 0).UTC(),
	}
	if si.NextAction != nil {
		na := &SetupIntentNextAction{
			Type: string(si.NextAction.Type),
		}
		if si.NextAction.RedirectToURL != nil {
			na.RedirectToURL = si.NextAction.RedirectToURL.URL
			na.ReturnURL = si.NextAction.RedirectToURL.ReturnURL
		}
		out.NextAction = na
	}
	return out
}

func projectPaymentMethod(pm *stripe.PaymentMethod, customerID string) *PaymentMethod {
	out := &PaymentMethod{
		StripeID:         pm.ID,
		StripeCustomerID: customerID,
		Type:             string(pm.Type),
		CreatedAt:        time.Unix(pm.Created, 0).UTC(),
		Metadata:         pm.Metadata,
	}
	if pm.Card != nil {
		out.Card = &CardSnapshot{
			Brand:    string(pm.Card.Brand),
			Last4:    pm.Card.Last4,
			ExpMonth: pm.Card.ExpMonth,
			ExpYear:  pm.Card.ExpYear,
		}
	}
	return out
}
