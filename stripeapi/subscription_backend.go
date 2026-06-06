package stripeapi

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/bds421/rho-stripe/subscriptions"
	stripe "github.com/stripe/stripe-go/v82"
)

// SubscriptionBackend implements subscriptions.Backend against the
// live Stripe API via stripe-go.
type SubscriptionBackend struct {
	sc *stripe.Client
}

// NewSubscriptionBackend wraps a stripe-go client.
func NewSubscriptionBackend(sc *stripe.Client) *SubscriptionBackend {
	if sc == nil {
		panic("stripeapi.NewSubscriptionBackend: StripeClient is required")
	}
	return &SubscriptionBackend{sc: sc}
}

var _ subscriptions.Backend = (*SubscriptionBackend)(nil)

// Implements the optional subject-scoped-reconcile capability (see
// subscriptions.ReconcileSubject).
var _ subscriptions.CustomerSubscriptionLister = (*SubscriptionBackend)(nil)

// CancelAtPeriodEnd toggles the cancel_at_period_end flag on the
// subscription. The subscription remains active until the current
// period ends, at which point Stripe cancels it and fires the
// customer.subscription.deleted event.
func (b *SubscriptionBackend) CancelAtPeriodEnd(ctx context.Context, stripeSubID string, cancelAtPeriodEnd bool) error {
	params := &stripe.SubscriptionUpdateParams{
		CancelAtPeriodEnd: stripe.Bool(cancelAtPeriodEnd),
	}
	applyIdem(params, ctx, "subscription.cancel_at_period_end", stripeSubID, strconv.FormatBool(cancelAtPeriodEnd))
	if _, err := b.sc.V1Subscriptions.Update(ctx, stripeSubID, params); err != nil {
		return fmt.Errorf("stripe.Subscriptions.Update(%s, cancel_at_period_end=%v): %w", stripeSubID, cancelAtPeriodEnd, err)
	}
	return nil
}

// CancelNow terminates the subscription immediately.
func (b *SubscriptionBackend) CancelNow(ctx context.Context, stripeSubID string, opts subscriptions.CancelNowOptions) error {
	params := &stripe.SubscriptionCancelParams{}
	if opts.Prorate {
		// Stripe's cancel API uses `invoice_now` + `prorate` for the
		// pro-ration controls. With Prorate=true Stripe credits unused
		// time as a customer-balance entry on the next invoice.
		params.InvoiceNow = stripe.Bool(true)
		params.Prorate = stripe.Bool(true)
	}
	if opts.Reason != "" {
		params.CancellationDetails = &stripe.SubscriptionCancelCancellationDetailsParams{
			Comment: stripe.String(opts.Reason),
		}
	}
	applyIdem(params, ctx, "subscription.cancel_now", stripeSubID, strconv.FormatBool(opts.Prorate), opts.Reason)
	if _, err := b.sc.V1Subscriptions.Cancel(ctx, stripeSubID, params); err != nil {
		return fmt.Errorf("stripe.Subscriptions.Cancel(%s): %w", stripeSubID, err)
	}
	return nil
}

// UpdateItems applies the item changes to the subscription. Used by
// Migrate (delete old + add new in one call).
func (b *SubscriptionBackend) UpdateItems(ctx context.Context, stripeSubID string, items []subscriptions.ItemChange, opts subscriptions.UpdateOptions) error {
	params := &stripe.SubscriptionUpdateParams{}
	if opts.ProrationBehavior != "" {
		params.ProrationBehavior = stripe.String(opts.ProrationBehavior)
	}

	for _, it := range items {
		ip := &stripe.SubscriptionUpdateItemParams{}
		if it.StripeItemID != "" {
			ip.ID = stripe.String(it.StripeItemID)
		}
		if it.PriceID != "" {
			ip.Price = stripe.String(it.PriceID)
		}
		if it.Quantity > 0 {
			ip.Quantity = stripe.Int64(it.Quantity)
		}
		if it.Deleted {
			ip.Deleted = stripe.Bool(true)
		}
		params.Items = append(params.Items, ip)
	}

	applyIdem(params, ctx, "subscription.update_items", stripeSubID, opts.ProrationBehavior, canonicalItemChanges(items))
	if _, err := b.sc.V1Subscriptions.Update(ctx, stripeSubID, params); err != nil {
		return fmt.Errorf("stripe.Subscriptions.Update(%s, items): %w", stripeSubID, err)
	}
	return nil
}

func canonicalSchedulePhases(phases []subscriptions.BackendPhase) string {
	parts := make([]string, 0, len(phases))
	for _, p := range phases {
		parts = append(parts, p.StripePriceID+"|"+
			canonicalInt64(p.Quantity)+"|"+
			canonicalInt64(int64(p.Iterations))+"|"+
			p.StripeCouponID)
	}
	return canonicalStrings(parts)
}

// canonicalItemChanges serializes a slice of ItemChange deterministically.
func canonicalItemChanges(items []subscriptions.ItemChange) string {
	parts := make([]string, 0, len(items))
	for _, it := range items {
		parts = append(parts, it.StripeItemID+"|"+it.PriceID+"|"+
			canonicalInt64(it.Quantity)+"|"+strconv.FormatBool(it.Deleted))
	}
	return canonicalStrings(parts)
}

// ApplyCoupon attaches a coupon to the subscription via the Discounts
// param. Replaces any previously-attached coupon (Stripe only allows
// one coupon per subscription).
func (b *SubscriptionBackend) ApplyCoupon(ctx context.Context, stripeSubID, couponID string) error {
	params := &stripe.SubscriptionUpdateParams{
		Discounts: []*stripe.SubscriptionUpdateDiscountParams{
			{Coupon: stripe.String(couponID)},
		},
	}
	applyIdem(params, ctx, "subscription.apply_coupon", stripeSubID, couponID)
	if _, err := b.sc.V1Subscriptions.Update(ctx, stripeSubID, params); err != nil {
		return fmt.Errorf("stripe.Subscriptions.Update(%s, coupon=%s): %w", stripeSubID, couponID, err)
	}
	return nil
}

// RemoveCoupon clears any discount on the subscription. Stripe
// removes coupons by passing an empty Discounts array.
func (b *SubscriptionBackend) RemoveCoupon(ctx context.Context, stripeSubID string) error {
	params := &stripe.SubscriptionUpdateParams{
		Discounts: []*stripe.SubscriptionUpdateDiscountParams{},
	}
	applyIdem(params, ctx, "subscription.remove_coupon", stripeSubID)
	if _, err := b.sc.V1Subscriptions.Update(ctx, stripeSubID, params); err != nil {
		return fmt.Errorf("stripe.Subscriptions.Update(%s, remove coupon): %w", stripeSubID, err)
	}
	return nil
}

// ListSince fetches every subscription created on or after since.
// Stripe's API only supports filtering by `created`, not "modified" —
// for reconciliation purposes this is acceptable for fresh
// subscriptions, and updates to existing ones are caught on the next
// webhook delivery. Apps that need true update-time filtering can
// paginate the full list and filter client-side.
func (b *SubscriptionBackend) ListSince(ctx context.Context, since time.Time) ([]subscriptions.ReconciledSubscription, error) {
	params := &stripe.SubscriptionListParams{
		Status: stripe.String("all"),
	}
	params.Filters.AddFilter("created", "gte", fmt.Sprintf("%d", since.Unix()))
	params.Filters.AddFilter("limit", "", "100")

	var out []subscriptions.ReconciledSubscription
	for s, err := range b.sc.V1Subscriptions.List(ctx, params) {
		if err != nil {
			return nil, fmt.Errorf("stripe.Subscriptions.List: %w", err)
		}
		out = append(out, projectSubscription(s))
	}
	return out, nil
}

// ListByCustomer fetches every subscription (status=all) for a single Stripe
// customer. It satisfies subscriptions.CustomerSubscriptionLister, enabling the
// subject-scoped Operations.ReconcileSubject — an O(one customer) reconcile for
// the synchronous cancel/delete path, versus ListSince's global sweep.
func (b *SubscriptionBackend) ListByCustomer(ctx context.Context, stripeCustomerID string) ([]subscriptions.ReconciledSubscription, error) {
	params := &stripe.SubscriptionListParams{
		Customer: stripe.String(stripeCustomerID),
		Status:   stripe.String("all"),
	}
	params.Filters.AddFilter("limit", "", "100")

	var out []subscriptions.ReconciledSubscription
	for s, err := range b.sc.V1Subscriptions.List(ctx, params) {
		if err != nil {
			return nil, fmt.Errorf("stripe.Subscriptions.List(customer=%s): %w", stripeCustomerID, err)
		}
		out = append(out, projectSubscription(s))
	}
	return out, nil
}

func projectSubscription(s *stripe.Subscription) subscriptions.ReconciledSubscription {
	rs := subscriptions.ReconciledSubscription{
		StripeID:          s.ID,
		Status:            subscriptions.Status(s.Status),
		CancelAtPeriodEnd: s.CancelAtPeriodEnd,
		Metadata:          s.Metadata,
		UpdatedUnix:       s.Created, // best available; Stripe lacks an "updated_at" on Subscription
	}
	if s.Customer != nil {
		rs.StripeCustomerID = s.Customer.ID
	}
	rs.CanceledAt = unixToPtr(s.CanceledAt)
	rs.EndedAt = unixToPtr(s.EndedAt)
	rs.TrialStart = unixToPtr(s.TrialStart)
	rs.TrialEnd = unixToPtr(s.TrialEnd)
	if s.Items != nil {
		for _, item := range s.Items.Data {
			si := subscriptions.SubscriptionItem{
				StripeID: item.ID,
				Quantity: item.Quantity,
			}
			if item.Price != nil {
				if item.Price.LookupKey != "" {
					si.PriceKey = item.Price.LookupKey
				} else {
					si.PriceKey = "stripe:" + item.Price.ID
				}
			}
			rs.Items = append(rs.Items, si)
			if rs.CurrentPeriodStart.IsZero() && item.CurrentPeriodStart > 0 {
				rs.CurrentPeriodStart = time.Unix(item.CurrentPeriodStart, 0)
				rs.CurrentPeriodEnd = time.Unix(item.CurrentPeriodEnd, 0)
			}
		}
	}
	return rs
}


// CreateSubscriptionSchedule wraps Stripe's SubscriptionSchedule API.
func (b *SubscriptionBackend) CreateSubscriptionSchedule(ctx context.Context, in subscriptions.ScheduleBackendInput) (*subscriptions.Schedule, error) {
	phases := make([]*stripe.SubscriptionScheduleCreatePhaseParams, 0, len(in.Phases))
	for _, p := range in.Phases {
		ph := &stripe.SubscriptionScheduleCreatePhaseParams{
			Items: []*stripe.SubscriptionScheduleCreatePhaseItemParams{
				{Price: stripe.String(p.StripePriceID), Quantity: stripe.Int64(p.Quantity)},
			},
		}
		if p.Iterations > 0 {
			ph.Iterations = stripe.Int64(int64(p.Iterations))
		}
		if p.StripeCouponID != "" {
			ph.Discounts = []*stripe.SubscriptionScheduleCreatePhaseDiscountParams{
				{Coupon: stripe.String(p.StripeCouponID)},
			}
		}
		phases = append(phases, ph)
	}
	params := &stripe.SubscriptionScheduleCreateParams{
		Customer:    stripe.String(in.StripeCustomerID),
		Phases:      phases,
		EndBehavior: stripe.String(in.EndBehavior),
	}
	if in.StartAt.IsZero() {
		// Stripe requires a start_date. When the caller didn't specify
		// one, request "now" using stripe-go's StartDateNow sentinel
		// (renders to start_date=now on the wire). Without this Stripe
		// returns a 400 — verified live.
		params.StartDateNow = stripe.Bool(true)
	} else {
		params.StartDate = stripe.Int64(in.StartAt.Unix())
	}
	for k, v := range in.Metadata {
		params.AddMetadata(k, v)
	}
	applyIdem(params, ctx, "subscription_schedule.create",
		in.StripeCustomerID, in.EndBehavior, canonicalSchedulePhases(in.Phases), canonicalMap(in.Metadata))
	created, err := b.sc.V1SubscriptionSchedules.Create(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("stripe.SubscriptionSchedules.Create: %w", err)
	}
	return &subscriptions.Schedule{
		StripeID: created.ID,
		Status:   string(created.Status),
	}, nil
}

// Pause sets pause_collection on the subscription.
func (b *SubscriptionBackend) Pause(ctx context.Context, stripeSubID string, opts subscriptions.PauseOptions) error { //nolint:dupl
	behavior := opts.Behavior
	if behavior == "" {
		behavior = "keep_as_draft"
	}
	pc := &stripe.SubscriptionUpdatePauseCollectionParams{
		Behavior: stripe.String(behavior),
	}
	if !opts.ResumesAt.IsZero() {
		pc.ResumesAt = stripe.Int64(opts.ResumesAt.Unix())
	}
	params := &stripe.SubscriptionUpdateParams{PauseCollection: pc}
	resumesAt := ""
	if !opts.ResumesAt.IsZero() {
		resumesAt = canonicalInt64(opts.ResumesAt.Unix())
	}
	applyIdem(params, ctx, "subscription.pause", stripeSubID, behavior, resumesAt)
	if _, err := b.sc.V1Subscriptions.Update(ctx, stripeSubID, params); err != nil {
		return fmt.Errorf("stripe.Subscriptions.Update(%s, pause): %w", stripeSubID, err)
	}
	return nil
}

// Resume clears the subscription's pause_collection.
//
// IMPORTANT distinction (verified live):
//   - `Subscriptions.Resume` (dedicated endpoint) only works for
//     subscriptions whose STATUS is "paused" (which Stripe sets via
//     subscription_schedule.paused or via trial-end behavior). It
//     does NOT clear a pause_collection set via Pause() above.
//   - To clear pause_collection set via the Update endpoint, Stripe
//     wants Update with pause_collection set to an empty string
//     (form-level delete sentinel). stripe-go encodes that as
//     SubscriptionParams.PauseCollection = nil + the form-empty token.
//
// We use Update with an empty PauseCollection params struct + the
// form-empty marker, which Stripe interprets as "remove pause".
func (b *SubscriptionBackend) Resume(ctx context.Context, stripeSubID string) error {
	params := &stripe.SubscriptionUpdateParams{}
	// Tell stripe-go to send pause_collection="" (the delete sentinel)
	// rather than omitting the field. Encoded via Params.Extra:
	params.AddExtra("pause_collection", "")
	applyIdem(params, ctx, "subscription.resume", stripeSubID)
	if _, err := b.sc.V1Subscriptions.Update(ctx, stripeSubID, params); err != nil {
		return fmt.Errorf("stripe.Subscriptions.Update(%s, clear pause_collection): %w", stripeSubID, err)
	}
	return nil
}

// PreviewItemChange asks Stripe to compute what the upcoming invoice
// would look like if the proposed items change were applied to the
// subscription right now. Used by Operations.PreviewMigrate to show
// the customer the prorated charge BEFORE they confirm an upgrade.
func (b *SubscriptionBackend) PreviewItemChange(ctx context.Context, stripeSubID string, items []subscriptions.ItemChange) (subscriptions.MigratePreview, error) {
	previewItems := make([]*stripe.InvoiceCreatePreviewSubscriptionDetailsItemParams, 0, len(items))
	for _, it := range items {
		p := &stripe.InvoiceCreatePreviewSubscriptionDetailsItemParams{}
		if it.StripeItemID != "" {
			p.ID = stripe.String(it.StripeItemID)
		}
		if it.PriceID != "" {
			p.Price = stripe.String(it.PriceID)
		}
		if it.Quantity > 0 {
			p.Quantity = stripe.Int64(it.Quantity)
		}
		if it.Deleted {
			p.Deleted = stripe.Bool(true)
		}
		previewItems = append(previewItems, p)
	}
	params := &stripe.InvoiceCreatePreviewParams{
		Subscription: stripe.String(stripeSubID),
		SubscriptionDetails: &stripe.InvoiceCreatePreviewSubscriptionDetailsParams{
			Items:             previewItems,
			ProrationBehavior: stripe.String("create_prorations"),
		},
	}
	inv, err := b.sc.V1Invoices.CreatePreview(ctx, params)
	if err != nil {
		return subscriptions.MigratePreview{}, fmt.Errorf("stripe.Invoices.CreatePreview(%s): %w", stripeSubID, err)
	}
	out := subscriptions.MigratePreview{
		Currency:       string(inv.Currency),
		AmountDueNow:   inv.AmountDue,
		AmountSubtotal: inv.Subtotal,
		AmountTax:      sumInvoiceTaxes(inv.TotalTaxes),
	}
	if inv.Lines != nil {
		for _, line := range inv.Lines.Data {
			lp := subscriptions.MigratePreviewLine{
				Description: line.Description,
				Amount:      line.Amount,
			}
			if line.Pricing != nil && line.Pricing.PriceDetails != nil {
				lp.PriceID = line.Pricing.PriceDetails.Price
			}
			out.ProrationLineItems = append(out.ProrationLineItems, lp)
		}
	}
	return out, nil
}

// buildAddInvoiceItemParams converts the lib's AddInvoiceItem into
// Stripe's SubscriptionCreateAddInvoiceItemParams. Inline items (no
// StripePriceID) get a one-shot Product created first.
//
// Returns the resulting params AND the Stripe Product id (empty when
// the item used a pre-existing StripePriceID). Callers track these
// IDs and archive them via b.archiveOrphanProducts(ctx, ids) if the
// outer Subscriptions.Create fails — otherwise inline-Product creation
// leaks orphans on every failed subscription create.
func (b *SubscriptionBackend) buildAddInvoiceItemParams(ctx context.Context, item subscriptions.AddInvoiceItem, sessionMeta map[string]string) (*stripe.SubscriptionCreateAddInvoiceItemParams, string, error) {
	qty := item.Quantity
	if qty <= 0 {
		qty = 1
	}
	out := &stripe.SubscriptionCreateAddInvoiceItemParams{
		Quantity: stripe.Int64(qty),
	}
	var inlineProductID string
	switch {
	case item.StripePriceID != "":
		out.Price = stripe.String(item.StripePriceID)
	case item.InlineAmount > 0 && item.Currency != "" && item.Name != "":
		// Stripe's InvoiceItemPriceDataParams requires a pre-existing
		// Product id (no inline product_data on invoice items). Create
		// the Product first, then reference it.
		prodParams := &stripe.ProductCreateParams{Name: stripe.String(item.Name)}
		for k, v := range sessionMeta {
			prodParams.AddMetadata(k, v)
		}
		applyIdem(prodParams, ctx, "product.create.setup_fee", item.Name, canonicalMap(sessionMeta))
		prod, err := b.sc.V1Products.Create(ctx, prodParams)
		if err != nil {
			return nil, "", fmt.Errorf("stripe.Products.Create(setup-fee inline): %w", err)
		}
		inlineProductID = prod.ID
		out.PriceData = &stripe.InvoiceItemPriceDataParams{
			Currency:   stripe.String(item.Currency),
			UnitAmount: stripe.Int64(item.InlineAmount),
			Product:    stripe.String(prod.ID),
		}
	default:
		return nil, "", fmt.Errorf("stripeapi: AddInvoiceItem requires StripePriceID OR (InlineAmount>0 + Currency + Name)")
	}
	return out, inlineProductID, nil
}

// archiveOrphanProducts best-effort archives Products created during
// a failed Subscriptions.Create call. Stripe Products archive cleanly
// (active=false). Archive failures are WARN-logged via slog.Default()
// so operators have a signal when orphans accumulate; we still don't
// return the error since the caller is mid-cleanup of a higher-priority
// failure they need to report.
func (b *SubscriptionBackend) archiveOrphanProducts(ctx context.Context, ids []string) {
	for _, id := range ids {
		params := &stripe.ProductUpdateParams{Active: stripe.Bool(false)}
		if _, err := b.sc.V1Products.Update(ctx, id, params); err != nil {
			slog.WarnContext(ctx, "stripeapi: orphan Product archive failed (manual cleanup may be needed)",
				slog.String("product_id", id),
				slog.String("err", err.Error()),
			)
		}
	}
}

// GetSubscriptionItems fetches the subscription from Stripe and
// projects its items into the lib's SubscriptionItem shape. Used as
// the mirror-miss fallback by seat helpers.
func (b *SubscriptionBackend) GetSubscriptionItems(ctx context.Context, stripeSubID string) ([]subscriptions.SubscriptionItem, error) {
	sub, err := b.sc.V1Subscriptions.Retrieve(ctx, stripeSubID, nil)
	if err != nil {
		return nil, fmt.Errorf("stripe.Subscriptions.Retrieve(%s): %w", stripeSubID, err)
	}
	if sub.Items == nil {
		return nil, nil
	}
	out := make([]subscriptions.SubscriptionItem, 0, len(sub.Items.Data))
	for _, it := range sub.Items.Data {
		si := subscriptions.SubscriptionItem{StripeID: it.ID, Quantity: it.Quantity}
		if it.Price != nil {
			if it.Price.LookupKey != "" {
				si.PriceKey = it.Price.LookupKey
			} else {
				si.PriceKey = "stripe:" + it.Price.ID
			}
		}
		out = append(out, si)
	}
	return out, nil
}

// CreateInvoiced creates a subscription with collection_method=send_invoice.
func (b *SubscriptionBackend) CreateInvoiced(ctx context.Context, in subscriptions.InvoicedSubscriptionInput) (*subscriptions.Subscription, error) {
	params := &stripe.SubscriptionCreateParams{
		Customer:         stripe.String(in.StripeCustomerID),
		CollectionMethod: stripe.String(string(stripe.SubscriptionCollectionMethodSendInvoice)),
		Items: []*stripe.SubscriptionCreateItemParams{
			{Price: stripe.String(in.StripePriceID), Quantity: stripe.Int64(in.Quantity)},
		},
	}
	if in.DueIn > 0 {
		days := int64(in.DueIn / (24 * time.Hour))
		if days < 1 {
			days = 1
		}
		params.DaysUntilDue = stripe.Int64(days)
	}
	if in.TrialDays > 0 {
		params.TrialPeriodDays = stripe.Int64(int64(in.TrialDays))
	}
	for k, v := range in.Metadata {
		params.AddMetadata(k, v)
	}
	var inlineProductIDs []string
	for _, item := range in.AddInvoiceItems {
		ai, inlineID, err := b.buildAddInvoiceItemParams(ctx, item, in.Metadata)
		if err != nil {
			// Earlier inline-Product creations already happened; archive them.
			b.archiveOrphanProducts(ctx, inlineProductIDs)
			return nil, err
		}
		if inlineID != "" {
			inlineProductIDs = append(inlineProductIDs, inlineID)
		}
		params.AddInvoiceItems = append(params.AddInvoiceItems, ai)
	}
	applyIdem(params, ctx, "subscription.create_invoiced",
		in.StripeCustomerID, in.StripePriceID, canonicalInt64(in.Quantity),
		canonicalInt64(int64(in.DueIn)), canonicalInt64(int64(in.TrialDays)),
		canonicalMap(in.Metadata),
	)
	sub, err := b.sc.V1Subscriptions.Create(ctx, params)
	if err != nil {
		// Subscription create failed AFTER inline-Product provisioning;
		// archive Products so they don't accumulate as orphans.
		b.archiveOrphanProducts(ctx, inlineProductIDs)
		return nil, fmt.Errorf("stripe.Subscriptions.Create(invoiced): %w", err)
	}
	return projectCreatedSubscription(sub), nil
}

// projectCreatedSubscription builds a subscriptions.Subscription from a
// freshly-created Stripe sub. Used by CreateInvoiced (and any future
// create paths) so callers get the same shape they'd see in the mirror.
// The mirror's authoritative row still gets written by the webhook
// handler — this projection is a synchronous-read convenience.
func projectCreatedSubscription(s *stripe.Subscription) *subscriptions.Subscription {
	out := &subscriptions.Subscription{
		StripeID:          s.ID,
		Status:            subscriptions.Status(s.Status),
		CancelAtPeriodEnd: s.CancelAtPeriodEnd,
		Metadata:          s.Metadata,
		UpdatedAt:         unixOrZero(s.Created),
		StripeUpdatedAt:   unixOrZero(s.Created),
	}
	if s.Customer != nil {
		out.StripeCustomerID = s.Customer.ID
	}
	if s.TrialStart > 0 {
		t := unixOrZero(s.TrialStart)
		out.TrialStart = &t
	}
	if s.TrialEnd > 0 {
		t := unixOrZero(s.TrialEnd)
		out.TrialEnd = &t
	}
	if s.Items != nil {
		for _, it := range s.Items.Data {
			si := subscriptions.SubscriptionItem{StripeID: it.ID, Quantity: it.Quantity}
			if it.Price != nil {
				if it.Price.LookupKey != "" {
					si.PriceKey = it.Price.LookupKey
				} else {
					si.PriceKey = "stripe:" + it.Price.ID
				}
			}
			out.Items = append(out.Items, si)
			if out.CurrentPeriodStart.IsZero() && it.CurrentPeriodStart > 0 {
				out.CurrentPeriodStart = unixOrZero(it.CurrentPeriodStart)
				out.CurrentPeriodEnd = unixOrZero(it.CurrentPeriodEnd)
			}
		}
	}
	return out
}
