// Package catalog defines the declarative spec for the products, prices,
// and coupons an app sells through Stripe. The spec is the source of
// truth; the sync CLI reconciles Stripe to match it. Apps refer to
// catalog entries by logical key everywhere; the lib resolves to Stripe
// IDs at runtime via lookup_key.
//
// See docs/adr/0001-catalog-as-source-of-truth.md and
// docs/adr/0003-catalog-binding.md for the design rationale.
package catalog

import "regexp"

// Spec is the top-level declarative catalog for one app.
type Spec struct {
	Namespace string
	Products  map[string]Product
	Coupons   map[string]Coupon
	Meters    map[string]Meter
}

// Meter declares a Stripe billing meter used for metered pricing.
// Sync ensures a corresponding Stripe Meter exists. Apps reference a
// Meter via Price.MeterRef on metered recurring prices.
type Meter struct {
	DisplayName string
	EventName   string // namespaced at sync time: "<namespace>.<event_name>"
	AggregateBy string // "sum" (default) | "count" | "last_value"
}

// Product is one purchasable thing. It can have many Prices (per
// currency, interval, etc.).
type Product struct {
	Name        string
	Description string
	TaxCategory TaxCategory
	Prices      map[string]Price
	CreditGrant *CreditGrant

	// RecurringGrant, when set on a Product with recurring Prices
	// (subscription), grants credits each time an invoice for the
	// subscription is paid. Useful for "Pro plan includes 1000 AI
	// credits/month" patterns.
	RecurringGrant *RecurringGrant

	Metadata map[string]string
}

// Price is one way to buy a Product. Stripe Prices are immutable once
// used; changes here produce a new Stripe Price on sync with the
// lookup_key transferred (see adr-0003).
type Price struct {
	Amount        int64
	Currency      string
	Type          PriceType
	Interval      Interval
	IntervalCount int

	// MeterRef, when non-empty, references a Spec.Meters key. Sync
	// creates the corresponding Stripe Price with `recurring.meter`
	// set, making it a usage-based price. Mutually exclusive with
	// the parent Product's CreditGrant / RecurringGrant (the lib's
	// validation enforces this).
	MeterRef string

	// TaxBehavior declares whether Amount is "inclusive" or
	// "exclusive" of tax. Empty defaults to Stripe's account-level
	// setting (typically "exclusive"). Set to "inclusive" for
	// EU-style "VAT included" pricing displays.
	TaxBehavior string

	// TaxRateOverrides, when non-empty, lists Stripe tax-rate ids
	// ("txr_…") that override Stripe Tax's automatic calculation
	// for sessions / subscriptions containing this Price. Useful for
	// legacy contracts, jurisdictions Stripe Tax doesn't cover, or
	// non-standard pricing arrangements.
	//
	// These ids reference Stripe's TaxRate resource — apps create
	// the TaxRate via dashboard or stripe-go and pass the id here.
	TaxRateOverrides []string
}

// CreditGrant is a one-shot credit-issuance descriptor attached to a
// one-time Price. The lib grants the named credits to the buyer's
// subject when the payment completes (driven by the
// checkout.session.completed webhook).
type CreditGrant struct {
	Bucket    string
	Amount    int64
	ValidDays int
}

// RecurringGrant is a per-billing-cycle credit-issuance descriptor
// attached to a Product with recurring Prices. The lib grants the
// named credits each billing cycle (one grant per invoice.paid).
// ValidDaysFromGrant controls expiry per grant (typically equal to
// the billing interval so customers can't accumulate unused credits
// indefinitely; pass 0 for "never expires").
type RecurringGrant struct {
	Bucket             string
	Amount             int64
	ValidDaysFromGrant int
}

// Coupon is a backend discount definition. Customer-facing promo codes
// (which reference a Coupon) are managed at runtime via the coupons
// package, not declared here.
type Coupon struct {
	PercentOff        float64
	AmountOff         int64
	Currency          string
	Duration          CouponDuration
	DurationMonths    int
	MaxRedemptions    int
	AppliesToProducts []string
	Preserve          bool
}

type PriceType string

const (
	PriceTypeRecurring PriceType = "recurring"
	PriceTypeOneTime   PriceType = "one_time"
)

type Interval string

const (
	IntervalMonth Interval = "month"
	IntervalYear  Interval = "year"
	IntervalWeek  Interval = "week"
	IntervalDay   Interval = "day"
)

type CouponDuration string

const (
	CouponDurationOnce      CouponDuration = "once"
	CouponDurationRepeating CouponDuration = "repeating"
	CouponDurationForever   CouponDuration = "forever"
)

// TaxCategory maps to Stripe's tax_code. The constants below are
// convenience aliases for the most commonly-used Stripe tax codes,
// fetched verbatim from https://docs.stripe.com/tax/tax-codes. Any
// value of the form "txcd_…" is accepted and passed through to Stripe.
//
// Pick the code that fits what you're SELLING, not what your business
// does. A SaaS company selling a one-time e-book uses
// TaxCategoryDigitalBookPermanent, not TaxCategorySaaSBusiness.
//
// Naming convention: the suffix narrows the variant Stripe defines
// (Personal vs Business; Streamed vs Downloaded; Subscription vs
// non-Subscription; Permanent vs Limited rights). Codes with a
// Personal/Business split charge VAT differently in many EU
// jurisdictions; pick the variant that matches your customer base.
//
// Stripe categorizes a "general" pass-through for each top-level
// bucket: TaxCategoryGeneralDigital, TaxCategoryGeneralService,
// TaxCategoryGeneralTangible. Use those when no specific code fits.
//
// Nontaxable products use TaxCategoryNontaxable (NOT TaxCategoryGeneralTangible
// — the latter still charges tax in jurisdictions with general sales tax).
type TaxCategory string

const (
	// --- Generic fallbacks ---
	TaxCategoryGeneralDigital  TaxCategory = "txcd_10000000" // General - Electronically Supplied Services
	TaxCategoryGeneralService  TaxCategory = "txcd_20030000" // General - Services
	TaxCategoryGeneralTangible TaxCategory = "txcd_99999999" // General - Tangible Goods
	TaxCategoryNontaxable      TaxCategory = "txcd_00000000" // Nontaxable

	// --- SaaS (the most-used family for backend libraries) ---
	TaxCategorySaaSPersonal             TaxCategory = "txcd_10103000" // SaaS - personal use
	TaxCategorySaaSBusiness             TaxCategory = "txcd_10103001" // SaaS - business use
	TaxCategorySaaSDownloadablePersonal TaxCategory = "txcd_10103100" // SaaS - electronic download - personal use
	TaxCategorySaaSDownloadableBusiness TaxCategory = "txcd_10103101" // SaaS - electronic download - business use
	TaxCategoryBusinessProcessAsService TaxCategory = "txcd_10104001" // Cloud-based business process as a service

	// --- IaaS / PaaS ---
	TaxCategoryIaaSPersonal TaxCategory = "txcd_10010001" // Infrastructure as a Service - personal use
	TaxCategoryIaaSBusiness TaxCategory = "txcd_10101000" // Infrastructure as a Service - business use
	TaxCategoryPaaSBusiness TaxCategory = "txcd_10102000" // Platform as a Service - business use
	TaxCategoryPaaSPersonal TaxCategory = "txcd_10102001" // Platform as a Service - personal use

	// --- AI as a Service ---
	TaxCategoryAIaaSCloudPersonal         TaxCategory = "txcd_10105001" // AI as a Service - Cloud Based - personal use
	TaxCategoryAIaaSCloudBusiness         TaxCategory = "txcd_10105002" // AI as a Service - Cloud Based - business use
	TaxCategoryAIaaSCloudDownloadPersonal TaxCategory = "txcd_10105003" // AIaaS - Cloud Based & Downloaded - personal use
	TaxCategoryAIaaSCloudDownloadBusiness TaxCategory = "txcd_10105004" // AIaaS - Cloud Based & Downloaded - business use

	// --- Downloadable software ---
	TaxCategoryDownloadableSoftwarePersonal                TaxCategory = "txcd_10202000" // Downloadable Software - personal use
	TaxCategoryDownloadableSoftwareNonRecreationalPersonal TaxCategory = "txcd_10202001" // Downloadable Software - non-recreational - personal use
	TaxCategoryDownloadableSoftwareBusiness                TaxCategory = "txcd_10202003" // Downloadable Software - business use
	TaxCategoryDownloadableSoftwareCustomPersonal          TaxCategory = "txcd_10203000" // Downloadable Software - custom - personal use
	TaxCategoryDownloadableSoftwareCustomBusiness          TaxCategory = "txcd_10203001" // Downloadable Software - custom - business use

	// --- Video games ---
	TaxCategoryVideoGameDownloadPermanent       TaxCategory = "txcd_10201000" // Video Games - downloaded - non subscription - with permanent rights
	TaxCategoryVideoGameDownloadLimited         TaxCategory = "txcd_10201001" // Video Games - downloaded - non subscription - with limited rights
	TaxCategoryVideoGameDownloadSubscription    TaxCategory = "txcd_10201002" // Video Games - downloaded - subscription - with conditional rights
	TaxCategoryVideoGameStreamedNonSubscription TaxCategory = "txcd_10201003" // Video Games - streamed - non subscription - with limited rights
	TaxCategoryVideoGameStreamedSubscription    TaxCategory = "txcd_10201004" // Video Games - streamed - subscription - with conditional rights

	// --- Books & audiobooks ---
	TaxCategoryAudiobook                       TaxCategory = "txcd_10301000" // Audiobook
	TaxCategoryDigitalBookDownloadPermanent    TaxCategory = "txcd_10302000" // Digital Books - downloaded - non subscription - with permanent rights
	TaxCategoryDigitalBookDownloadLimited      TaxCategory = "txcd_10302001" // Digital Books - downloaded - non subscription - with limited rights
	TaxCategoryDigitalBookDownloadSubscription TaxCategory = "txcd_10302002" // Digital Books - downloaded - subscription - with conditional rights
	TaxCategoryDigitalBookViewableSubscription TaxCategory = "txcd_10302003" // Digital Books - viewable only - subscription - with conditional rights
	TaxCategoryDigitalTextbookLimited          TaxCategory = "txcd_10305000" // Digital School Textbooks - downloaded - non subscription - with limited rights
	TaxCategoryDigitalTextbookPermanent        TaxCategory = "txcd_10305001" // Digital School Textbooks - downloaded - non subscription - with permanent rights

	// --- Magazines / periodicals ---
	TaxCategoryDigitalMagazineDownloadSubscription TaxCategory = "txcd_10303000" // Digital Magazines - downloadable - subscription - with conditional rights
	TaxCategoryDigitalMagazineTangibleAndDigital   TaxCategory = "txcd_10303001" // Digital Magazines - subscription tangible and digital
	TaxCategoryDigitalMagazineViewableSubscription TaxCategory = "txcd_10303002" // Digital Magazines - viewable only - subscription - with conditional rights
	TaxCategoryDigitalMagazineDownloadPermanent    TaxCategory = "txcd_10303100" // Digital Magazines - downloadable - non subscription - with permanent rights
	TaxCategoryDigitalMagazineViewableLimited      TaxCategory = "txcd_10303101" // Digital Magazines - viewable only - non subscription - with limited rights
	TaxCategoryDigitalMagazineViewablePermanent    TaxCategory = "txcd_10303102" // Digital Magazines - viewable only - non subscription - with permanent rights
	TaxCategoryDigitalMagazineDownloadLimited      TaxCategory = "txcd_10303104" // Digital Magazines - downloadable - non subscription - with limited rights

	// --- Newspapers ---
	TaxCategoryDigitalNewspaperDownloadPermanent    TaxCategory = "txcd_10304000" // Digital Newspapers - downloadable - non subscription - with permanent rights
	TaxCategoryDigitalNewspaperViewableLimited      TaxCategory = "txcd_10304001" // Digital Newspapers - viewable only - non subscription - with limited rights
	TaxCategoryDigitalNewspaperViewablePermanent    TaxCategory = "txcd_10304002" // Digital Newspapers - viewable only - non subscription - with permanent rights
	TaxCategoryDigitalNewspaperDownloadLimited      TaxCategory = "txcd_10304003" // Digital Newspapers - downloadable - non subscription - with limited rights
	TaxCategoryDigitalNewspaperDownloadSubscription TaxCategory = "txcd_10304100" // Digital Newspapers - downloadable - subscription - with conditional rights
	TaxCategoryDigitalNewspaperTangibleAndDigital   TaxCategory = "txcd_10304101" // Digital Newspapers - subscription tangible and digital
	TaxCategoryDigitalNewspaperViewableSubscription TaxCategory = "txcd_10304102" // Digital Newspapers - viewable only - subscription - with conditional rights

	// --- Streaming / downloadable audio (music apps) ---
	TaxCategoryDigitalAudioStreamedLimited      TaxCategory = "txcd_10401000" // Digital Audio Works - streamed - non subscription - with limited rights
	TaxCategoryDigitalAudioDownloadLimited      TaxCategory = "txcd_10401001" // Digital Audio Works - downloaded - non subscription - with limited rights
	TaxCategoryDigitalAudioDownloadPermanent    TaxCategory = "txcd_10401100" // Digital Audio Works - downloaded - non subscription - with permanent rights
	TaxCategoryDigitalAudioStreamedSubscription TaxCategory = "txcd_10401200" // Digital Audio Works - streamed - subscription - with conditional rights (Spotify-like)

	// --- Streaming / downloadable video (video apps) ---
	TaxCategoryDigitalVideoStreamedLimited      TaxCategory = "txcd_10402000" // Digital Audio Visual Works - streamed - non subscription - with limited rights
	TaxCategoryDigitalVideoDownloadPermanent    TaxCategory = "txcd_10402100" // Digital Audio Visual Works - downloaded - non subscription - with permanent rights
	TaxCategoryDigitalVideoDownloadLimited      TaxCategory = "txcd_10402110" // Digital Audio Visual Works - downloaded - non subscription - with limited rights
	TaxCategoryDigitalVideoStreamedSubscription TaxCategory = "txcd_10402200" // Digital Audio Visual Works - streamed - subscription - with conditional rights (Netflix-like)
	TaxCategoryDigitalVideoLiveEvent            TaxCategory = "txcd_10402300" // Digital Video Streaming - live events - limited use

	// --- Images & photos ---
	TaxCategoryDigitalImagePermanent TaxCategory = "txcd_10501000" // Digital Photographs/Images - downloaded - non subscription - with permanent rights

	// --- Digital gifts ---
	TaxCategoryGiftCard                   TaxCategory = "txcd_10502000" // Gift Card
	TaxCategoryDigitalGreetingAudio       TaxCategory = "txcd_10506000" // Digital Greeting Cards - Audio Only
	TaxCategoryDigitalGreetingAudioVisual TaxCategory = "txcd_10506001" // Digital Greeting Cards - Audio Visual
	TaxCategoryDigitalGreetingStatic      TaxCategory = "txcd_10506002" // Digital Greeting Cards - Static text and/or images

	// --- Web & digital services ---
	TaxCategoryWebsiteAdvertising    TaxCategory = "txcd_10701000" // Website Advertising
	TaxCategoryWebsiteHosting        TaxCategory = "txcd_10701100" // Website Hosting
	TaxCategoryWebsiteDesign         TaxCategory = "txcd_10701200" // Website Design
	TaxCategoryWebsiteDataProcessing TaxCategory = "txcd_10701300" // Website Data Processing
	TaxCategoryOnlineDating          TaxCategory = "txcd_10702000" // Online Dating Services

	// --- Documentation ---
	TaxCategorySoftwareDocCustom     TaxCategory = "txcd_10504000" // Electronic software documentation - Custom, electronic delivery
	TaxCategorySoftwareDocPrewritten TaxCategory = "txcd_10504003" // Electronic software documentation - Prewritten, electronic delivery

	// --- Aliases for the most common SaaS choice ---
	// Prefer the explicit *Personal / *Business constants in new code;
	// this alias preserves the slice-1 name and defaults to "personal
	// use" (the most permissive interpretation).
	TaxCategorySaaS TaxCategory = TaxCategorySaaSPersonal
)

var (
	// namespaceRegex enforces:
	//   - Lowercase ASCII letters, digits, underscore only.
	//   - First character must be a letter.
	//   - 1-30 characters total.
	//   - No '.' (reserved as the catalog/Stripe key separator).
	//   - No '-' (Stripe metadata supports it but our lookup-key
	//     convention uses underscore for consistency).
	//
	// Why 30 chars: Stripe lookup_keys are limited to ~200 chars;
	// after `<namespace>_<product>_<price>` plus padding we keep
	// each segment well under that.
	namespaceRegex = regexp.MustCompile(`^[a-z][a-z0-9_]{0,29}$`)

	// keyRegex applies to product and price keys. Same character
	// set as namespace but:
	//   - 1-63 characters total (Stripe lookup_keys limit minus
	//     the namespace+separator overhead).
	//   - First character may be a digit (rare but legal for
	//     pricing keys like "2gb_plan").
	keyRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,62}$`)

	// couponKeyRegex permits uppercase because promo-code-style coupon IDs
	// (SAVE20, FOUNDER, ENTERPRISE30D) are the industry convention.
	couponKeyRegex = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_]{0,62}$`)

	// currencyRegex is a permissive ISO 4217 check; Stripe rejects bad
	// codes too but a local check gives better errors.
	currencyRegex = regexp.MustCompile(`^[a-z]{3}$`)
)
