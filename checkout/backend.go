package checkout

import "context"

// Backend is the Stripe-side surface the Checkout package depends on.
// The default implementation backed by stripe-go lives in the
// stripeapi package; tests inject fakes.
type Backend interface {
	// CreateCustomer creates a new Stripe Customer with the provided
	// metadata stamps (notably app_namespace and subject_id).
	CreateCustomer(ctx context.Context, params CustomerCreate) (StripeCustomerID, error)

	// CreateCheckoutSession creates a Checkout Session and returns
	// its id + hosted URL.
	CreateCheckoutSession(ctx context.Context, params SessionCreate) (Session, error)

	// CreatePortalSession creates a Customer Portal Session for the
	// given Stripe Customer and returns its id + hosted URL.
	CreatePortalSession(ctx context.Context, params PortalSessionCreate) (PortalSession, error)
}

// CustomerCreate carries the fields needed to create a Stripe Customer.
type CustomerCreate struct {
	SubjectID  SubjectID
	Metadata map[string]string
}

// SessionCreate carries the parameters for stripe.CheckoutSession.New.
// The lib's higher-level CreateSession assembles this from Input +
// resolved price ids + B2B defaults.
type SessionCreate struct {
	StripeCustomerID StripeCustomerID
	Mode             Mode
	LineItems        []SessionLineItem
	SuccessURL       string
	CancelURL        string
	ClientReference  string // ActorID, if any
	PromoCode        string // optional; customer-facing string only
	PromoCodeID      string // optional; pre-applied Stripe promotion_code id
	Metadata         map[string]string

	// PaymentMethods, when non-empty, sets Stripe's
	// payment_method_types explicitly (overriding the account-level
	// dynamic configuration). Empty leaves Stripe in dynamic mode.
	PaymentMethods []string

	// UIMode + ReturnURL pass through embedded-mode parameters.
	// SuccessURL / CancelURL are ignored when UIMode == embedded.
	UIMode    UIMode
	ReturnURL string

	// Defaults bundles the B2B-tax-friendly defaults the wrapper
	// applies. Apps that need to opt out (B2C, no-tax-collection,
	// optional billing address) populate this explicitly. The zero
	// value yields the library's B2B defaults (automatic tax,
	// tax-ID collection, required billing address, name+address
	// auto-update, promo-code entry allowed).
	Defaults SessionDefaults

	// TrialDays, when > 0, starts the resulting subscription with a
	// free-trial period of N days. Has no effect for payment-mode
	// sessions. See [Input.TrialDays] for the higher-level docs.
	TrialDays int

	// Locale forces the hosted-Checkout language. Empty leaves it to
	// Stripe's auto-detection. See [Input.Locale] for accepted values.
	Locale string
}

// SessionLineItem is one line on the session, after PriceKey resolution.
//
// Exactly one of (StripePriceID, CustomAmount) is populated:
//   - StripePriceID — pre-registered catalog price.
//   - CustomAmount — pay-what-you-want; stripeapi creates the inline
//     ad-hoc Price/Product when sending the request.
type SessionLineItem struct {
	StripePriceID string
	Quantity      int
	CustomAmount  *CustomAmount // when set, StripePriceID is empty
}

// SessionDefaults controls every B2B-friendly knob the wrapper applies
// to a Checkout Session. The zero value yields sensible B2B defaults
// (automatic tax + tax-ID collection + required billing address +
// auto-update of customer name/address + promo-code entry allowed).
// Apps that need a non-B2B flow override individual fields.
//
// Each *bool / pointer field follows the convention: nil means "use
// the library default (B2B)", non-nil means "use this explicit value".
type SessionDefaults struct {
	// AutomaticTax, when set, overrides the library default of true.
	// Set to a pointer-to-false to skip Stripe Tax (B2C in jurisdictions
	// where you handle tax yourself, or for free-tier upgrades).
	AutomaticTax *bool

	// TaxIDCollection, when set, overrides the library default of true.
	// Apps that don't need to capture VAT-IDs (pure B2C) set this to
	// pointer-to-false to remove the field from the hosted form.
	TaxIDCollection *bool

	// BillingAddressCollection overrides "required". Valid values are
	// "required" and "auto" — see Stripe docs. Empty string keeps the
	// library default ("required" — useful for VAT).
	BillingAddressCollection string

	// CustomerUpdateAddress controls the customer_update.address mode
	// ("auto" | "never" | ""). Empty string keeps the library default
	// of "auto" (Stripe writes the entered address back to the Customer).
	CustomerUpdateAddress string

	// CustomerUpdateName controls customer_update.name ("auto" | "never").
	// Empty keeps the default "auto".
	CustomerUpdateName string

	// AllowPromotionCodes overrides the library default of true. Apps
	// that don't accept promo codes set this to pointer-to-false (also
	// hides the field from the hosted page).
	AllowPromotionCodes *bool

	// RequirePaymentMethodForTrial controls whether Stripe captures a
	// payment method upfront during a trial. nil/default = true (today's
	// behaviour: card captured at sign-up, auto-charged at trial end).
	// Set to pointer-to-false for "no credit card required" trial
	// landing pages — Stripe lets the user skip the card form and
	// asks for it before the first charge instead. Maps to Stripe's
	// payment_method_collection: "if_required".
	//
	// Only meaningful when [Input.TrialDays] > 0.
	RequirePaymentMethodForTrial *bool
}

// resolveBool returns *v when set, else fallback.
func resolveBool(v *bool, fallback bool) bool {
	if v == nil {
		return fallback
	}
	return *v
}

// resolveString returns v when non-empty, else fallback.
func resolveString(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// Resolve returns each field with the library's B2B fallback applied
// where the caller didn't override. Exported because stripeapi reads
// it; apps don't normally call this.
func (d SessionDefaults) Resolve() ResolvedSessionDefaults {
	return ResolvedSessionDefaults{
		AutomaticTax:                 resolveBool(d.AutomaticTax, true),
		TaxIDCollection:              resolveBool(d.TaxIDCollection, true),
		BillingAddressCollection:     resolveString(d.BillingAddressCollection, "required"),
		CustomerUpdateAddress:        resolveString(d.CustomerUpdateAddress, "auto"),
		CustomerUpdateName:           resolveString(d.CustomerUpdateName, "auto"),
		AllowPromotionCodes:          resolveBool(d.AllowPromotionCodes, true),
		RequirePaymentMethodForTrial: resolveBool(d.RequirePaymentMethodForTrial, true),
	}
}

// ResolvedSessionDefaults is the post-fallback view stripeapi uses.
type ResolvedSessionDefaults struct {
	AutomaticTax                 bool
	TaxIDCollection              bool
	BillingAddressCollection     string
	CustomerUpdateAddress        string
	CustomerUpdateName           string
	AllowPromotionCodes          bool
	RequirePaymentMethodForTrial bool
}
