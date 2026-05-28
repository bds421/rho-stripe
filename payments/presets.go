// Package payments exposes payment-method presets per checkout intent.
//
// By default, Stripe's hosted Checkout uses the account-level dynamic
// payment method configuration: every method enabled in the Stripe
// dashboard that's available for the customer's region/currency. That
// is usually the right call — Stripe knows current regional support
// better than a hard-coded list.
//
// This package gives apps an explicit-override path for cases where
// they want to constrain methods per intent (e.g. "only card for
// credit-pack top-ups; SEPA + card for subscriptions"). Pass a
// preset slice via checkout.Input.PaymentMethods.
//
// See docs/adr/0009-currency-and-tax.md for the full payment-method
// matrix the design targets.
package payments

// Intent identifies a checkout intent for picking a preset.
type Intent string

const (
	IntentSubscription   Intent = "subscription"
	IntentOneTime        Intent = "one_time"
	IntentCreditTopUp    Intent = "credit_top_up"
	IntentInvoice        Intent = "invoice"
	IntentSEPAOnly       Intent = "sepa_only"
	IntentBankTransferEU Intent = "bank_transfer_eu"
	IntentBankTransferUS Intent = "bank_transfer_us"
	IntentBankTransferUK Intent = "bank_transfer_uk"
	IntentCardOnly       Intent = "card_only"
)

// Standard preset slices, exported for direct use.
var (
	// PresetSubscription targets recurring B2B subscriptions.
	// SEPA Direct Debit is the cheapest recurring method for EU
	// customers; cards are universal. Apple/Google Pay are free
	// wins on mobile.
	PresetSubscription = []string{"card", "sepa_debit", "apple_pay", "google_pay"}

	// PresetOneTime targets ad-hoc one-time purchases. Same set as
	// Subscription but documented separately so future divergence is
	// painless.
	PresetOneTime = []string{"card", "sepa_debit", "apple_pay", "google_pay"}

	// PresetCreditTopUp targets credit-pack purchases. Cards +
	// wallet methods only — SEPA's confirmation delay (~1-3 days)
	// makes it a bad fit for "I want credits now."
	PresetCreditTopUp = []string{"card", "apple_pay", "google_pay"}

	// PresetInvoice targets net-30 invoices (phase-6 feature).
	// Customer-balance covers bank transfer; card is the fallback.
	PresetInvoice = []string{"customer_balance", "card"}

	// PresetSEPADirectDebit is a SEPA-DD-only preset for EU recurring
	// flows where the customer's local bank is the only acceptable
	// payment route (compliance / cost-control). Stripe handles the
	// SEPA mandate collection on the hosted Checkout page.
	PresetSEPADirectDebit = []string{"sepa_debit"}

	// PresetBankTransferEU enables Stripe's customer_balance payment
	// method backed by SEPA Credit Transfer for EU customers. Apps
	// receive a wire-transfer instruction sheet; customer pushes the
	// money; Stripe credits the customer balance when received.
	//
	// Common for B2B enterprise where the customer's AP department
	// pays by bank transfer rather than card. Pair with
	// invoiced subscriptions or with manual conn.Invoices.CreateDraft.
	PresetBankTransferEU = []string{"customer_balance"}

	// PresetBankTransferUS targets US bank-transfer flows (ACH credit
	// + Plaid-backed ACH debit). Stripe handles confirmation; apps
	// see the funds land via the customer_balance.funded event.
	PresetBankTransferUS = []string{"customer_balance", "us_bank_account"}

	// PresetBankTransferUK targets UK BACS / Faster Payments through
	// the customer_balance.
	PresetBankTransferUK = []string{"customer_balance", "bacs_debit"}

	// PresetCardOnly is the strictest preset — useful when an app
	// rejects all non-card flows (e.g. when delivering digital goods
	// instantly and can't wait for bank-transfer settlement).
	PresetCardOnly = []string{"card"}
)

// MethodsFor returns the preset for intent, or nil for an unknown
// intent (which the caller can interpret as "use Stripe's dynamic
// default").
func MethodsFor(intent Intent) []string {
	switch intent {
	case IntentSubscription:
		return append([]string(nil), PresetSubscription...)
	case IntentOneTime:
		return append([]string(nil), PresetOneTime...)
	case IntentCreditTopUp:
		return append([]string(nil), PresetCreditTopUp...)
	case IntentInvoice:
		return append([]string(nil), PresetInvoice...)
	case IntentSEPAOnly:
		return append([]string(nil), PresetSEPADirectDebit...)
	case IntentBankTransferEU:
		return append([]string(nil), PresetBankTransferEU...)
	case IntentBankTransferUS:
		return append([]string(nil), PresetBankTransferUS...)
	case IntentBankTransferUK:
		return append([]string(nil), PresetBankTransferUK...)
	case IntentCardOnly:
		return append([]string(nil), PresetCardOnly...)
	default:
		return nil
	}
}
