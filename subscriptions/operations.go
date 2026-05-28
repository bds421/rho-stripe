package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bds421/rho-kit/core/v2/clock"
	"github.com/bds421/rho-stripe/catalog"
	"github.com/bds421/rho-stripe/meta"
)

// Operations is the public surface apps use to manipulate live
// subscriptions and query the mirror. Construct via New; the
// connector facade builds one automatically when Config.Subscriptions
// is set.
//
// Mutating methods (Cancel*, Migrate, ApplyCoupon, RemoveCoupon) send
// the change to Stripe and let the resulting webhook event update the
// mirror — they do NOT update the repo directly. Apps that need
// immediate read-after-write consistency should re-fetch via the
// repo a short moment later, OR explicitly call repo.GetByStripeID
// after the webhook has been observed.
type Operations struct {
	backend   Backend
	repo      SubscriptionRepo
	spec      *catalog.Spec
	cache     *catalog.Cache
	namespace string // app_namespace stamped onto every created Stripe object
	now       clock.Func
}

// SetClock replaces the now-function used for UpdatedAt stamping
// during reconcile. Defaults to clock.System() (time.Now); tests
// pass clock.NewStub(...).Func() to drive time deterministically.
func (o *Operations) SetClock(fn clock.Func) { o.now = clock.OrSystem(fn) }

// Config wires the package's dependencies. Replaces the prior
// positional New(backend, repo, spec, cache) (rho-stripe v0.1.0
// constructor-shape convergence — see CONTRIBUTING.md).
type Config struct {
	// Backend is the Stripe-side adapter. Required.
	Backend Backend
	// Repo is the local subscription mirror. Required.
	Repo SubscriptionRepo
	// Spec is the catalog used to namespace + resolve PriceKeys. Required
	// for Migrate / CreateInvoiced / CreateSchedule; the simpler mutation
	// methods (Cancel*, Pause, Resume, ApplyCoupon) work without it.
	Spec *catalog.Spec
	// Cache resolves namespaced PriceKeys → Stripe price ids. Required
	// alongside Spec for the catalog-resolution paths.
	Cache *catalog.Cache
}

// New constructs Operations. Cache and Spec are required when calling
// Migrate / CreateInvoiced / CreateSchedule (price-key resolution);
// other methods work without them.
//
// The namespace is sourced from Spec.Namespace when Spec != nil;
// otherwise it's empty (no auto-stamp). The connector facade always
// passes Spec so production callers always get the stamp.
func New(cfg Config) *Operations {
	if cfg.Backend == nil {
		panic("subscriptions.New: Backend is required")
	}
	if cfg.Repo == nil {
		panic("subscriptions.New: Repo is required")
	}
	ns := ""
	if cfg.Spec != nil {
		ns = cfg.Spec.Namespace
	}
	return &Operations{
		backend:   cfg.Backend,
		repo:      cfg.Repo,
		spec:      cfg.Spec,
		cache:     cfg.Cache,
		namespace: ns,
		now:       clock.System(),
	}
}

// stampNamespace returns a copy of in with app_namespace set. Used
// before every backend call that creates a Stripe object on the
// caller's behalf so webhook routing can attribute the event back
// to this app. Thin shim over [meta.StampNamespace] kept for
// receiver-method ergonomics at call sites.
func (o *Operations) stampNamespace(in map[string]string) map[string]string {
	return meta.StampNamespace(in, o.namespace)
}

// Errors specific to operations.
var (
	ErrMigrateNeedsCacheAndSpec = errors.New("subscriptions: Migrate requires Operations to be constructed with both cache and spec (catalog resolution)")
	ErrSubscriptionNotInMirror  = errors.New("subscriptions: subscription not found in local mirror — Stripe may not have delivered its event yet")
	ErrFromPriceNotOnSub        = errors.New("subscriptions: FromPriceKey not present on subscription")
	ErrPriceKeyUnresolved       = errors.New("subscriptions: price key not in catalog cache (run sync?)")
)

// MigrateInput describes a price change on an existing subscription:
// replace the subscription's item that currently uses FromPriceKey
// with an item using ToPriceKey. Both keys are app-relative (e.g.
// "pro_plan.monthly_eur"); the lib namespaces them via the catalog spec.
type MigrateInput struct {
	StripeSubID  string
	FromPriceKey string
	ToPriceKey   string
	Prorate      bool

	// Cycle-anchor control omitted: Stripe defaults to "unchanged"
	// which keeps the customer's billing date predictable. Apps that
	// need to force-restart the cycle use the raw stripe-go client.
}

// CancelAtPeriodEnd schedules cancellation at the end of the current
// billing period. The subscription stays Active until then; the
// mirror's CancelAtPeriodEnd flag flips when the resulting webhook
// arrives.
func (o *Operations) CancelAtPeriodEnd(ctx context.Context, stripeSubID string) error {
	if stripeSubID == "" {
		return errors.New("subscriptions: stripeSubID is required")
	}
	return o.backend.CancelAtPeriodEnd(ctx, stripeSubID, true)
}

// UndoCancelAtPeriodEnd clears a previously-scheduled cancellation.
// No-op if the subscription wasn't scheduled to cancel.
func (o *Operations) UndoCancelAtPeriodEnd(ctx context.Context, stripeSubID string) error {
	if stripeSubID == "" {
		return errors.New("subscriptions: stripeSubID is required")
	}
	return o.backend.CancelAtPeriodEnd(ctx, stripeSubID, false)
}

// CancelNow cancels the subscription immediately. Customer loses
// access as soon as the resulting `customer.subscription.deleted`
// event lands in the mirror.
func (o *Operations) CancelNow(ctx context.Context, stripeSubID string, opts CancelNowOptions) error {
	if stripeSubID == "" {
		return errors.New("subscriptions: stripeSubID is required")
	}
	return o.backend.CancelNow(ctx, stripeSubID, opts)
}

// CancelNowByStripeID is the no-options shorthand satisfying the
// customers.SubscriptionCanceler narrow interface. Used by
// customers.Forget to route cancellation through the lib's
// Operations layer (so namespace stamping + idempotency keys apply).
//
// Design note (why two methods, not one): CancelNow takes
// CancelNowOptions for direct app use (apps choose proration /
// reason). customers.SubscriptionCanceler is a narrow contract
// declared in the customers package — it can't depend on
// subscriptions.CancelNowOptions without an import cycle. This
// shim adapts the wide method to the narrow interface; the
// "gdpr_forget" reason is hard-coded because the only caller IS
// the GDPR forget flow.
func (o *Operations) CancelNowByStripeID(ctx context.Context, stripeSubID string) error {
	return o.CancelNow(ctx, stripeSubID, CancelNowOptions{Reason: "gdpr_forget"})
}

// Migrate replaces FromPriceKey with ToPriceKey on the subscription.
// Requires Operations to be constructed with both spec + cache so
// catalog keys can be namespaced and resolved.
func (o *Operations) Migrate(ctx context.Context, in MigrateInput) error {
	if o.spec == nil || o.cache == nil {
		return ErrMigrateNeedsCacheAndSpec
	}
	if in.StripeSubID == "" {
		return errors.New("subscriptions: MigrateInput.StripeSubID is required")
	}

	sub, ok, err := o.repo.GetByStripeID(ctx, in.StripeSubID)
	if err != nil {
		return fmt.Errorf("subscriptions.Migrate: lookup subscription: %w", err)
	}
	if !ok {
		return ErrSubscriptionNotInMirror
	}

	fromNamespaced := namespacedKeyOrSelf(o.spec, in.FromPriceKey)
	toNamespaced := namespacedKeyOrSelf(o.spec, in.ToPriceKey)

	var fromItemID string
	for _, item := range sub.Items {
		if item.PriceKey == fromNamespaced {
			fromItemID = item.StripeID
			break
		}
	}
	if fromItemID == "" {
		return fmt.Errorf("%w: %q (subscription has: %v)", ErrFromPriceNotOnSub, fromNamespaced, itemPriceKeys(sub.Items))
	}

	toPriceID, ok := o.cache.Lookup(toNamespaced)
	if !ok {
		return fmt.Errorf("%w: %q", ErrPriceKeyUnresolved, toNamespaced)
	}

	updateOpts := UpdateOptions{
		ProrationBehavior: prorationBehavior(in.Prorate),
	}

	// Delete the old item + add the new item in a single update.
	items := []ItemChange{
		{StripeItemID: fromItemID, Deleted: true},
		{PriceID: toPriceID, Quantity: 1},
	}
	return o.backend.UpdateItems(ctx, in.StripeSubID, items, updateOpts)
}

// Pause pauses invoice generation on the subscription without
// canceling. Customer keeps access (mirror status stays as-is until
// the resulting webhook lands). Apps use this for "snooze billing
// for a month" retention flows.
//
// Behavior "" defaults to "keep_as_draft" (invoices created as
// drafts the app reviews after resume). Other values: "mark_uncollectible",
// "void" — see Stripe docs.
func (o *Operations) Pause(ctx context.Context, stripeSubID string, opts PauseOptions) error {
	if stripeSubID == "" {
		return errors.New("subscriptions.Pause: stripeSubID is required")
	}
	return o.backend.Pause(ctx, stripeSubID, opts)
}

// Resume undoes Pause. Stripe resumes billing from now.
func (o *Operations) Resume(ctx context.Context, stripeSubID string) error {
	if stripeSubID == "" {
		return errors.New("subscriptions.Resume: stripeSubID is required")
	}
	return o.backend.Resume(ctx, stripeSubID)
}

// PreviewMigrate returns the proration breakdown for the proposed
// Migrate WITHOUT applying it. Apps use this to show the customer
// "upgrading now will charge €X today, and your next renewal will
// be €Y" before they confirm.
//
// The preview is point-in-time: by the time the actual Migrate runs
// the customer's clock may have moved past a period boundary and the
// prorated amount may differ slightly. The preview is accurate enough
// for UX, NOT for billing reconciliation.
func (o *Operations) PreviewMigrate(ctx context.Context, in MigrateInput) (MigratePreview, error) {
	if o.spec == nil || o.cache == nil {
		return MigratePreview{}, ErrMigrateNeedsCacheAndSpec
	}
	if in.StripeSubID == "" {
		return MigratePreview{}, errors.New("subscriptions: PreviewMigrate: StripeSubID is required")
	}

	sub, ok, err := o.repo.GetByStripeID(ctx, in.StripeSubID)
	if err != nil {
		return MigratePreview{}, fmt.Errorf("subscriptions.PreviewMigrate: lookup: %w", err)
	}
	if !ok {
		return MigratePreview{}, ErrSubscriptionNotInMirror
	}

	fromNamespaced := namespacedKeyOrSelf(o.spec, in.FromPriceKey)
	toNamespaced := namespacedKeyOrSelf(o.spec, in.ToPriceKey)
	var fromItemID string
	for _, item := range sub.Items {
		if item.PriceKey == fromNamespaced {
			fromItemID = item.StripeID
			break
		}
	}
	if fromItemID == "" {
		return MigratePreview{}, fmt.Errorf("%w: %q", ErrFromPriceNotOnSub, fromNamespaced)
	}
	toPriceID, ok := o.cache.Lookup(toNamespaced)
	if !ok {
		return MigratePreview{}, fmt.Errorf("%w: %q", ErrPriceKeyUnresolved, toNamespaced)
	}
	return o.backend.PreviewItemChange(ctx, in.StripeSubID, []ItemChange{
		{StripeItemID: fromItemID, Deleted: true},
		{PriceID: toPriceID, Quantity: 1},
	})
}

// ApplyCoupon attaches couponKey (catalog-relative, e.g. "SAVE20") to
// the subscription. The lib namespaces it via the spec (matching what
// catalog sync created in Stripe).
func (o *Operations) ApplyCoupon(ctx context.Context, stripeSubID, couponKey string) error {
	if stripeSubID == "" {
		return errors.New("subscriptions: stripeSubID is required")
	}
	if couponKey == "" {
		return errors.New("subscriptions: couponKey is required")
	}
	couponID := couponKey
	if o.spec != nil {
		couponID = o.spec.NamespacedCouponID(couponKey)
	}
	return o.backend.ApplyCoupon(ctx, stripeSubID, couponID)
}

// RemoveCoupon clears any coupon attached to the subscription. Future
// invoices are billed at full price.
func (o *Operations) RemoveCoupon(ctx context.Context, stripeSubID string) error {
	if stripeSubID == "" {
		return errors.New("subscriptions: stripeSubID is required")
	}
	return o.backend.RemoveCoupon(ctx, stripeSubID)
}

// --- Hot-path query wrappers (delegate to the standalone helpers) ---

// ListActive returns the subject's access-granting subscriptions.
func (o *Operations) ListActive(ctx context.Context, subject SubjectID) ([]*Subscription, error) {
	return ListActive(ctx, o.repo, subject)
}

// HasActivePrice reports whether subject has an active subscription
// matching priceKeyOrGlob.
func (o *Operations) HasActivePrice(ctx context.Context, subject SubjectID, priceKeyOrGlob string) (bool, error) {
	pattern := priceKeyOrGlob
	if o.spec != nil {
		pattern = namespacedKeyOrSelf(o.spec, priceKeyOrGlob)
	}
	return HasActivePrice(ctx, o.repo, subject, pattern)
}

// ListByPriceKey returns active subscriptions matching priceKeyOrGlob.
func (o *Operations) ListByPriceKey(ctx context.Context, subject SubjectID, priceKeyOrGlob string) ([]*Subscription, error) {
	pattern := priceKeyOrGlob
	if o.spec != nil {
		pattern = namespacedKeyOrSelf(o.spec, priceKeyOrGlob)
	}
	return ListByPriceKey(ctx, o.repo, subject, pattern)
}

// Repo exposes the raw repo for app-side direct queries.
func (o *Operations) Repo() SubscriptionRepo { return o.repo }

// InvoicedInput configures CreateInvoiced (recurring net-30 plans).
type InvoicedInput struct {
	StripeCustomerID string        // resolved by the app from its CustomerRepo
	PriceKey         string        // catalog-relative ("pro_plan.yearly_eur")
	Quantity         int64         // defaults to 1
	DueIn            time.Duration // e.g. 30*24*time.Hour for net-30
	AutoFinalize     bool          // when false, each cycle's invoice waits for app review
	Metadata         map[string]string

	// TrialDays starts the subscription with a free-trial period of
	// N days (1–730). 0 disables. After the trial Stripe transitions
	// the subscription from "trialing" to the configured collection
	// mode and the first invoice is sent.
	TrialDays int

	// SetupFee, when > 0, adds a one-time charge of that amount to
	// the first invoice using the same currency as the main price.
	// Useful for "annual contract with €500 onboarding" patterns.
	SetupFee int64

	// SetupFeeDescription is the line-item description shown on the
	// invoice for the setup fee. Defaults to "Setup fee".
	SetupFeeDescription string

	// AddInvoiceItems is the lower-level escape hatch: multiple
	// arbitrary one-time charges on the first invoice. Use when
	// SetupFee isn't expressive enough (different currencies,
	// per-item quantities, references to catalog prices).
	AddInvoiceItems []AddInvoiceItem
}

// CreateInvoiced creates a subscription whose recurring invoices are
// sent to the customer (collection_method=send_invoice) instead of
// charged automatically. Common for enterprise annual contracts.
// Requires Operations to be constructed with spec + cache.
func (o *Operations) CreateInvoiced(ctx context.Context, in InvoicedInput) (*Subscription, error) {
	if o.spec == nil || o.cache == nil {
		return nil, ErrMigrateNeedsCacheAndSpec
	}
	if in.StripeCustomerID == "" {
		return nil, errors.New("subscriptions.CreateInvoiced: StripeCustomerID is required")
	}
	if in.PriceKey == "" {
		return nil, errors.New("subscriptions.CreateInvoiced: PriceKey is required")
	}
	nskey := namespacedKeyOrSelf(o.spec, in.PriceKey)
	priceID, ok := o.cache.Lookup(nskey)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrPriceKeyUnresolved, nskey)
	}
	qty := in.Quantity
	if qty <= 0 {
		qty = 1
	}
	if in.TrialDays != 0 && (in.TrialDays < 1 || in.TrialDays > 730) {
		return nil, errors.New("subscriptions.CreateInvoiced: TrialDays must be 0 or between 1 and 730")
	}

	// Build the AddInvoiceItems list: explicit items + SetupFee sugar.
	addItems := append([]AddInvoiceItem(nil), in.AddInvoiceItems...)
	if in.SetupFee > 0 {
		desc := in.SetupFeeDescription
		if desc == "" {
			desc = "Setup fee"
		}
		// Pull the currency from the resolved Price's spec entry.
		specPrice := lookupSpecPrice(o.spec, in.PriceKey)
		if specPrice.Currency == "" {
			return nil, errors.New("subscriptions.CreateInvoiced: SetupFee requires the priced item to declare a Currency in the spec")
		}
		addItems = append(addItems, AddInvoiceItem{
			InlineAmount: in.SetupFee,
			Currency:     specPrice.Currency,
			Name:         desc,
			Quantity:     1,
		})
	}

	return o.backend.CreateInvoiced(ctx, InvoicedSubscriptionInput{
		StripeCustomerID: in.StripeCustomerID,
		StripePriceID:    priceID,
		Quantity:         qty,
		DueIn:            in.DueIn,
		AutoFinalize:     in.AutoFinalize,
		Metadata:         o.stampNamespace(in.Metadata),
		TrialDays:        in.TrialDays,
		AddInvoiceItems:  addItems,
	})
}

// lookupSpecPrice returns the catalog Price for the given app-relative
// key, or zero value when not found. Used by CreateInvoiced to
// retrieve the currency for SetupFee.
func lookupSpecPrice(spec *catalog.Spec, key string) catalog.Price {
	pk, prk, ok := splitTwoDot(key)
	if !ok {
		return catalog.Price{}
	}
	p, ok := spec.Products[pk]
	if !ok {
		return catalog.Price{}
	}
	return p.Prices[prk]
}

// splitTwoDot splits "product.price" on the single dot. Mirrors the
// helper in the checkout package; duplicated here to avoid the
// import cycle (subscriptions → checkout would be wrong direction).
func splitTwoDot(s string) (a, b string, ok bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return s[:i], s[i+1:], i > 0 && i < len(s)-1
		}
	}
	return "", "", false
}

// --- helpers ---

// namespacedKeyOrSelf prepends the spec namespace if the key isn't
// already namespaced. Supports trailing-`*` globs.
func namespacedKeyOrSelf(spec *catalog.Spec, key string) string {
	if spec == nil || key == "" {
		return key
	}
	prefix := spec.Namespace + "."
	if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
		return key
	}
	return prefix + key
}

func itemPriceKeys(items []SubscriptionItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.PriceKey)
	}
	return out
}

func prorationBehavior(prorate bool) string {
	if prorate {
		return "create_prorations"
	}
	return "none"
}
