// Package checkout creates Stripe Checkout Sessions for one-time and
// subscription purchases. It hides the dozens of Stripe-API knobs
// behind a small B2B-tuned API: apps pass logical catalog keys + a
// subject identifier and get back a hosted payment URL.
//
// See docs/adr/0009-currency-and-tax.md (Stripe Tax, payment methods)
// and docs/design/checkout-and-portal.md for the design.
package checkout

import "github.com/bds421/rho-stripe/subject"

// SubjectID is the opaque per-app identifier for the billing subject
// (org / tenant for B2B, user for B2C). Per adr-0002 the lib stays
// neutral on what a subject is; apps decide and stick to one meaning.
//
// As of slice 40 SubjectID is a type alias of [subject.ID] so values
// flow freely between the connector packages without conversion.
type SubjectID = subject.ID

// ActorID is the optional id of the user initiating the purchase. In
// B2B it differs from SubjectID (the org pays, a user clicks). Stored
// in Stripe's client_reference_id for audit trails.
type ActorID string

// LineItem refers to a Price in the catalog by its logical key
// (e.g. "pro_plan.monthly_eur"). The lib resolves the key to a Stripe
// price id via the catalog cache; Quantity defaults to 1.
//
// For pay-what-you-want / donation flows, leave PriceKey empty and
// populate CustomAmount — the lib creates an ad-hoc Stripe Price
// inline (custom_unit_amount) without registering it in the catalog.
type LineItem struct {
	PriceKey string
	Quantity int

	// CustomAmount, when non-nil, switches this line item to
	// pay-what-you-want mode. PriceKey must be empty.
	//
	// Use cases: donations, name-your-price tipping, custom-priced
	// services where the price isn't predetermined.
	CustomAmount *CustomAmount
}

// CustomAmount configures an ad-hoc pay-what-you-want line item.
// All amounts are in smallest currency unit.
type CustomAmount struct {
	// Currency is required (ISO 4217, lowercase: "eur", "usd", …).
	Currency string

	// Min / Max bound the amount the customer can choose.
	// Stripe requires Min >= 1 and Max >= Min.
	Min int64
	Max int64

	// Preset is the default amount shown in the form. Optional;
	// when 0, Stripe uses Min as the default.
	Preset int64

	// Name is the product name shown on the receipt. Required —
	// Stripe forces every ad-hoc price to be attached to a Product,
	// so we materialize one with this display name.
	Name string

	// TaxCategory determines the tax_code on the ad-hoc Product.
	// Defaults to "txcd_99999999" (general taxable) when empty.
	TaxCategory string
}

// Input is the data required to create a Checkout Session.
type Input struct {
	SubjectID     SubjectID
	Actor       ActorID // optional
	LineItems   []LineItem
	SuccessURL  string
	CancelURL   string
	PromoCode   string            // optional; customer-facing promo code (e.g. "SAVE20"). Currently only used to allow Stripe-hosted page entry (AllowPromotionCodes); for pre-application use PromoCodeID with a resolved promo_ id.
	PromoCodeID string            // optional; Stripe promotion_code id (promo_…). Pre-applied to the session so the customer sees the discount without entering the code. Apps resolve via conn.Coupons.ListPromoCodes.
	Metadata    map[string]string // optional; merged into session metadata

	// PaymentMethods optionally constrains the payment methods Stripe
	// offers on the hosted page (e.g. ["card", "sepa_debit"]). When
	// nil, Stripe's account-level dynamic configuration is used. Use
	// the payments package's presets for the recommended sets per
	// intent.
	PaymentMethods []string

	// UIMode selects the rendering surface. The zero value (UIModeHosted)
	// keeps the lib's historical behavior: Stripe-hosted page, returned
	// via Session.URL. UIModeEmbedded returns a client_secret for the
	// Payment Element to render in-app (Session.ClientSecret).
	UIMode UIMode

	// ReturnURL is required for UIModeEmbedded. Stripe redirects here
	// when the embedded session completes. May contain the literal
	// "{CHECKOUT_SESSION_ID}" placeholder which Stripe substitutes.
	ReturnURL string

	// Defaults overrides the library's B2B Checkout defaults. Zero value
	// keeps the defaults (automatic tax + tax-ID + required billing
	// address + auto-update + promo codes). See [SessionDefaults] for
	// the per-field meaning.
	Defaults SessionDefaults

	// TrialDays starts the subscription with a free trial of N days
	// (1–730). 0 disables (default). After the trial Stripe
	// auto-charges and transitions the subscription from "trialing"
	// to "active". Apps usually combine this with
	// Defaults.RequirePaymentMethodForTrial=false for "no credit card
	// required" trial landing pages.
	//
	// Has no effect for one-time (payment-mode) sessions.
	TrialDays int

	// Locale forces the Stripe-hosted Checkout page's language.
	// Accepted values: "auto" (default; Stripe detects from browser),
	// "de", "en", "fr", "es", "it", "nl", "pt", "pt-BR", "ja", "zh",
	// "zh-HK", and several others — see Stripe docs.
	//
	// Apps with multi-language UIs set this from the user's session
	// language so the Checkout page matches.
	Locale string
}

// UIMode selects between hosted (Stripe-served page) and embedded
// (Payment Element rendered inside the app via Stripe.js).
type UIMode string

const (
	UIModeHosted   UIMode = "" // default; legacy hosted page
	UIModeEmbedded UIMode = "embedded"
)

// Session is the projection of a created Stripe Checkout Session.
//
// URL is populated for hosted (redirect) mode.
// ClientSecret is populated for embedded mode (UIMode=embedded); the
// app passes it to Stripe.js to render the Payment Element inline.
type Session struct {
	ID           string
	URL          string
	ClientSecret string
}

// Mode reflects whether the line items in a session are recurring (a
// subscription) or one-time (a payment). The lib infers this from the
// resolved price types; apps don't pass it.
type Mode string

const (
	ModeSubscription Mode = "subscription"
	ModePayment      Mode = "payment"
)
