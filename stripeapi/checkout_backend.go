package stripeapi

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bds421/rho-stripe/checkout"
	stripe "github.com/stripe/stripe-go/v82"
)

// Compile-time interface check for checkout.Backend.
var _ checkout.Backend = (*CheckoutBackend)(nil)
var _ checkout.CustomerBalanceBackend = (*CheckoutBackend)(nil)

// CheckoutBackend implements checkout.Backend against the live Stripe
// API via stripe-go. It applies B2B defaults (Stripe Tax, tax-ID
// collection, billing address required, customer update) on every
// session it creates, per adr-0009.
type CheckoutBackend struct {
	sc *stripe.Client
}

// NewCheckoutBackend wraps a stripe-go client as a checkout.Backend.
func NewCheckoutBackend(sc *stripe.Client) *CheckoutBackend {
	if sc == nil {
		panic("stripeapi.NewCheckoutBackend: StripeClient is required")
	}
	return &CheckoutBackend{sc: sc}
}

// CreateCustomer creates a Stripe Customer with metadata stamps.
// Idempotency-Key derived from metadata so a retry produces the same
// Customer rather than duplicates.
func (b *CheckoutBackend) CreateCustomer(ctx context.Context, p checkout.CustomerCreate) (checkout.StripeCustomerID, error) {
	params := &stripe.CustomerCreateParams{}
	for k, v := range p.Metadata {
		params.AddMetadata(k, v)
	}
	applyIdem(params, ctx, "customer.create", canonicalMap(p.Metadata))
	cust, err := b.sc.V1Customers.Create(ctx, params)
	if err != nil {
		return "", fmt.Errorf("stripe.Customers.Create: %w", err)
	}
	return checkout.StripeCustomerID(cust.ID), nil
}

// CreateCheckoutSession creates a Stripe Checkout Session with the
// configured B2B defaults: automatic tax, tax-ID collection, required
// billing address, and customer auto-update for name + address.
func (b *CheckoutBackend) CreateCheckoutSession(ctx context.Context, p checkout.SessionCreate) (checkout.Session, error) {
	d := p.Defaults.Resolve()
	params := &stripe.CheckoutSessionCreateParams{
		Mode:                     stripe.String(stripeMode(p.Mode)),
		Customer:                 stripe.String(string(p.StripeCustomerID)),
		AutomaticTax:             &stripe.CheckoutSessionCreateAutomaticTaxParams{Enabled: stripe.Bool(d.AutomaticTax)},
		TaxIDCollection:          &stripe.CheckoutSessionCreateTaxIDCollectionParams{Enabled: stripe.Bool(d.TaxIDCollection)},
		BillingAddressCollection: stripe.String(d.BillingAddressCollection),
		CustomerUpdate: &stripe.CheckoutSessionCreateCustomerUpdateParams{
			Address: stripe.String(d.CustomerUpdateAddress),
			Name:    stripe.String(d.CustomerUpdateName),
		},
		AllowPromotionCodes: stripe.Bool(d.AllowPromotionCodes),
	}

	if p.Locale != "" {
		params.Locale = stripe.String(p.Locale)
	}

	if p.UIMode == checkout.UIModeEmbedded {
		params.UIMode = stripe.String("embedded")
		params.ReturnURL = stripe.String(p.ReturnURL)
	} else {
		params.SuccessURL = stripe.String(p.SuccessURL)
		params.CancelURL = stripe.String(p.CancelURL)
	}

	if p.ClientReference != "" {
		params.ClientReferenceID = stripe.String(p.ClientReference)
	}

	for _, m := range p.PaymentMethods {
		params.PaymentMethodTypes = append(params.PaymentMethodTypes, stripe.String(m))
	}

	if p.PromoCodeID != "" {
		params.Discounts = append(params.Discounts, &stripe.CheckoutSessionCreateDiscountParams{
			PromotionCode: stripe.String(p.PromoCodeID),
		})
		// Pre-applied discount; let customer override if they have a
		// different code by leaving allow_promotion_codes on… but
		// Stripe forbids both at once. Turn it off when pre-applying.
		params.AllowPromotionCodes = stripe.Bool(false)
	}

	for k, v := range p.Metadata {
		params.AddMetadata(k, v)
	}

	// Propagate namespace metadata onto the downstream Subscription /
	// PaymentIntent so subsequent webhook events (invoice.*,
	// customer.subscription.*, payment_intent.*) carry the stamp the
	// dispatcher uses for namespace-based filtering. Without this, only
	// the checkout.session.* events would be filterable.
	switch p.Mode {
	case checkout.ModeSubscription:
		subData := &stripe.CheckoutSessionCreateSubscriptionDataParams{}
		for k, v := range p.Metadata {
			subData.AddMetadata(k, v)
		}
		if p.TrialDays > 0 {
			subData.TrialPeriodDays = stripe.Int64(int64(p.TrialDays))
		}
		params.SubscriptionData = subData
		// When TrialDays is set + caller opted out of upfront card
		// capture, switch Stripe to its "if_required" mode so the
		// customer can start the trial without entering a card.
		if p.TrialDays > 0 && !d.RequirePaymentMethodForTrial {
			params.PaymentMethodCollection = stripe.String("if_required")
		}
	case checkout.ModePayment:
		piData := &stripe.CheckoutSessionCreatePaymentIntentDataParams{}
		for k, v := range p.Metadata {
			piData.AddMetadata(k, v)
		}
		params.PaymentIntentData = piData
	}

	hasCustomAmount := false
	// adHocPriceIDs tracks ad-hoc Prices we create; if the session
	// creation below fails, we archive them so they don't accumulate
	// as orphan Stripe resources. Stripe doesn't support deleting
	// Prices (they're financial primitives), but archiving hides them
	// from the dashboard and prevents reuse.
	var adHocPriceIDs []string
	for _, li := range p.LineItems {
		lineItem := &stripe.CheckoutSessionCreateLineItemParams{
			Quantity: stripe.Int64(int64(li.Quantity)),
		}
		if li.CustomAmount != nil {
			hasCustomAmount = true
			// Pay-what-you-want: create an ad-hoc Price (with inline
			// Product) and reference it. Stripe Checkout doesn't
			// accept custom_unit_amount inline on line items —
			// custom_unit_amount lives on the Price resource itself.
			priceID, err := b.createAdHocCustomPrice(ctx, p.Metadata, li.CustomAmount)
			if err != nil {
				b.archiveOrphanAdHocPrices(ctx, adHocPriceIDs)
				return checkout.Session{}, err
			}
			adHocPriceIDs = append(adHocPriceIDs, priceID)
			lineItem.Price = stripe.String(priceID)
		} else {
			lineItem.Price = stripe.String(li.StripePriceID)
		}
		params.LineItems = append(params.LineItems, lineItem)
	}
	// Stripe rejects allow_promotion_codes=true on sessions whose
	// line items include a custom_unit_amount price (verified live).
	// Override the default — apps that opt into custom-amount accept
	// "no promo codes" as the trade-off.
	if hasCustomAmount {
		params.AllowPromotionCodes = stripe.Bool(false)
	}

	applyIdem(params, ctx, "checkout.session.create",
		string(p.StripeCustomerID),
		string(p.Mode),
		p.SuccessURL,
		p.CancelURL,
		p.ReturnURL,
		p.ClientReference,
		canonicalMap(p.Metadata),
		canonicalLineItems(p.LineItems),
	)
	created, err := b.sc.V1CheckoutSessions.Create(ctx, params)
	if err != nil {
		// Session creation failed AFTER we provisioned ad-hoc Prices —
		// archive them to avoid orphans in Stripe.
		b.archiveOrphanAdHocPrices(ctx, adHocPriceIDs)
		return checkout.Session{}, fmt.Errorf("stripe.CheckoutSessions.Create: %w", err)
	}
	return checkout.Session{
		ID:           created.ID,
		URL:          created.URL,
		ClientSecret: created.ClientSecret,
	}, nil
}

// archiveOrphanAdHocPrices best-effort archives Prices the lib created
// during a failed CheckoutSessions.Create call. Stripe Prices can't be
// deleted (financial primitive); archive == active=false, which hides
// them from dashboard searches and prevents reuse.
//
// Archive failures are WARN-logged via slog.Default() so operators can
// detect orphan accumulation; the error is not returned because the
// caller is mid-cleanup of the higher-priority session-creation failure.
func (b *CheckoutBackend) archiveOrphanAdHocPrices(ctx context.Context, ids []string) {
	for _, id := range ids {
		params := &stripe.PriceUpdateParams{Active: stripe.Bool(false)}
		if _, err := b.sc.V1Prices.Update(ctx, id, params); err != nil {
			slog.WarnContext(ctx, "stripeapi: orphan Price archive failed (manual cleanup may be needed)",
				slog.String("price_id", id),
				slog.String("err", err.Error()),
			)
		}
	}
}

// canonicalLineItems renders Checkout LineItems into a deterministic
// string for idempotency-key derivation. CustomAmount line items
// include the bounds so two pay-what-you-want sessions with different
// presets get different keys.
func canonicalLineItems(items []checkout.SessionLineItem) string {
	parts := make([]string, 0, len(items))
	for _, li := range items {
		s := li.StripePriceID + ":" + canonicalInt64(int64(li.Quantity))
		if li.CustomAmount != nil {
			s += ":custom=" + li.CustomAmount.Name + "/" +
				li.CustomAmount.Currency + "/" +
				canonicalInt64(li.CustomAmount.Min) + "-" +
				canonicalInt64(li.CustomAmount.Max) + "@" +
				canonicalInt64(li.CustomAmount.Preset)
		}
		parts = append(parts, s)
	}
	return canonicalStrings(parts)
}

// createAdHocCustomPrice creates a one-shot Stripe Price with
// custom_unit_amount enabled (pay-what-you-want flows). The Price
// belongs to an ad-hoc Product created inline via product_data.
//
// The returned Price ID is referenced by the Checkout line item.
// sessionMeta is propagated to both the Product's metadata and the
// Price's metadata so webhook routing (app_namespace) works.
func (b *CheckoutBackend) createAdHocCustomPrice(ctx context.Context, sessionMeta map[string]string, ca *checkout.CustomAmount) (string, error) {
	taxCode := ca.TaxCategory
	if taxCode == "" {
		taxCode = "txcd_99999999"
	}
	productData := &stripe.PriceCreateProductDataParams{
		Name:    stripe.String(ca.Name),
		TaxCode: stripe.String(taxCode),
	}
	for k, v := range sessionMeta {
		productData.AddMetadata(k, v)
	}
	customUnit := &stripe.PriceCreateCustomUnitAmountParams{
		Enabled: stripe.Bool(true),
		Minimum: stripe.Int64(ca.Min),
		Maximum: stripe.Int64(ca.Max),
	}
	if ca.Preset > 0 {
		customUnit.Preset = stripe.Int64(ca.Preset)
	}
	params := &stripe.PriceCreateParams{
		Currency:         stripe.String(ca.Currency),
		ProductData:      productData,
		CustomUnitAmount: customUnit,
	}
	for k, v := range sessionMeta {
		params.AddMetadata(k, v)
	}
	applyIdem(params, ctx, "price.create.adhoc",
		ca.Name, ca.Currency,
		canonicalInt64(ca.Min), canonicalInt64(ca.Max), canonicalInt64(ca.Preset),
		canonicalMap(sessionMeta),
	)
	pr, err := b.sc.V1Prices.Create(ctx, params)
	if err != nil {
		return "", fmt.Errorf("stripe.Prices.Create(custom_unit_amount): %w", err)
	}
	return pr.ID, nil
}

// AdjustCustomerBalance creates a CustomerBalanceTransaction adjusting
// the customer's balance by the given amount. Implements
// checkout.CustomerBalanceBackend.
func (b *CheckoutBackend) AdjustCustomerBalance(ctx context.Context, customerID checkout.StripeCustomerID, amount int64, currency, description string) (checkout.CustomerBalanceTransaction, error) {
	params := &stripe.CustomerBalanceTransactionCreateParams{
		Customer:    stripe.String(string(customerID)),
		Amount:      stripe.Int64(amount),
		Currency:    stripe.String(currency),
		Description: stripe.String(description),
	}
	applyIdem(params, ctx, "customer_balance_tx.create",
		string(customerID), canonicalInt64(amount), currency, description)
	tx, err := b.sc.V1CustomerBalanceTransactions.Create(ctx, params)
	if err != nil {
		return checkout.CustomerBalanceTransaction{}, fmt.Errorf("stripe.CustomerBalanceTransactions.Create: %w", err)
	}
	return projectBalanceTx(tx), nil
}

// GetCustomerBalance fetches the Customer's current balance.
func (b *CheckoutBackend) GetCustomerBalance(ctx context.Context, customerID checkout.StripeCustomerID) (checkout.CustomerBalance, error) {
	cust, err := b.sc.V1Customers.Retrieve(ctx, string(customerID), nil)
	if err != nil {
		return checkout.CustomerBalance{}, fmt.Errorf("stripe.Customers.Retrieve(%s): %w", customerID, err)
	}
	return checkout.CustomerBalance{
		StripeCustomerID: customerID,
		Currency:         string(cust.Currency),
		Balance:          cust.Balance,
	}, nil
}

// ListCustomerBalanceTransactions returns the customer's balance-
// transaction history, newest first.
func (b *CheckoutBackend) ListCustomerBalanceTransactions(ctx context.Context, customerID checkout.StripeCustomerID, limit int) ([]checkout.CustomerBalanceTransaction, error) {
	if limit <= 0 {
		limit = 50
	}
	params := &stripe.CustomerBalanceTransactionListParams{
		Customer: stripe.String(string(customerID)),
	}
	params.Filters.AddFilter("limit", "", fmt.Sprintf("%d", limit))

	var out []checkout.CustomerBalanceTransaction
	for tx, err := range b.sc.V1CustomerBalanceTransactions.List(ctx, params) {
		if err != nil {
			return nil, fmt.Errorf("stripe.CustomerBalanceTransactions.List: %w", err)
		}
		out = append(out, projectBalanceTx(tx))
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func projectBalanceTx(tx *stripe.CustomerBalanceTransaction) checkout.CustomerBalanceTransaction {
	out := checkout.CustomerBalanceTransaction{
		StripeID:    tx.ID,
		Type:        string(tx.Type),
		Amount:      tx.Amount,
		Currency:    string(tx.Currency),
		Description: tx.Description,
		CreatedAt:   time.Unix(tx.Created, 0).UTC(),
	}
	if tx.Invoice != nil {
		out.InvoiceID = tx.Invoice.ID
	}
	return out
}

// CreatePortalSession creates a Stripe Customer Portal session.
func (b *CheckoutBackend) CreatePortalSession(ctx context.Context, p checkout.PortalSessionCreate) (checkout.PortalSession, error) {
	params := &stripe.BillingPortalSessionCreateParams{
		Customer:  stripe.String(string(p.StripeCustomerID)),
		ReturnURL: stripe.String(p.ReturnURL),
	}

	if p.Flow != nil {
		flowParams, err := buildFlowDataParams(p.Flow)
		if err != nil {
			return checkout.PortalSession{}, err
		}
		params.FlowData = flowParams
	}

	// Portal sessions get a single-shot URL with a short TTL; dedup
	// across retries within a request, but not across separate "Manage
	// billing" clicks. Caller can override via WithIdempotencyKey.
	flowKey := ""
	if p.Flow != nil {
		flowKey = string(p.Flow.Type) + ":" + p.Flow.SubscriptionID
	}
	applyIdem(params, ctx, "portal.session.create",
		string(p.StripeCustomerID), p.ReturnURL, flowKey)
	created, err := b.sc.V1BillingPortalSessions.Create(ctx, params)
	if err != nil {
		return checkout.PortalSession{}, fmt.Errorf("stripe.BillingPortalSessions.Create: %w", err)
	}
	return checkout.PortalSession{ID: created.ID, URL: created.URL}, nil
}

func buildFlowDataParams(f *checkout.PortalFlow) (*stripe.BillingPortalSessionCreateFlowDataParams, error) {
	params := &stripe.BillingPortalSessionCreateFlowDataParams{
		Type: stripe.String(string(f.Type)),
	}
	switch f.Type {
	case checkout.PortalFlowSubscriptionCancel:
		params.SubscriptionCancel = &stripe.BillingPortalSessionCreateFlowDataSubscriptionCancelParams{
			Subscription: stripe.String(f.SubscriptionID),
		}
	case checkout.PortalFlowSubscriptionUpdate:
		params.SubscriptionUpdate = &stripe.BillingPortalSessionCreateFlowDataSubscriptionUpdateParams{
			Subscription: stripe.String(f.SubscriptionID),
		}
	case checkout.PortalFlowPaymentMethodUpdate:
		// no extra params required
	default:
		return nil, fmt.Errorf("stripeapi: unsupported PortalFlowType %q", f.Type)
	}
	return params, nil
}

func stripeMode(m checkout.Mode) string {
	switch m {
	case checkout.ModeSubscription:
		return string(stripe.CheckoutSessionModeSubscription)
	case checkout.ModePayment:
		return string(stripe.CheckoutSessionModePayment)
	default:
		return string(stripe.CheckoutSessionModePayment)
	}
}
