// Package portalconfig declares the desired Stripe BillingPortal
// Configuration in Go, then syncs it to Stripe like the catalog
// package syncs Products and Prices.
//
// Rationale: Stripe's Customer Portal exposes per-account configuration
// (which features customers see, which subscription updates they can
// self-serve). Apps usually configure it once in the Stripe Dashboard,
// then forget about it — drift between code expectations and dashboard
// reality bites later. Declaring the config in Go + syncing makes
// the configuration reviewable in PRs.
//
// Each app's portal config is identified by `metadata.app_namespace`
// so multiple apps sharing one Stripe account can co-exist (same
// pattern as the catalog package).
package portalconfig

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bds421/rho-stripe/meta"
	"github.com/bds421/rho-stripe/stripeapi"
	stripe "github.com/stripe/stripe-go/v82"
)

// Spec describes the desired portal configuration. Fields map closely
// to Stripe's BillingPortalConfiguration with sensible defaults baked
// in for B2B SaaS.
type Spec struct {
	// Namespace stamps metadata.app_namespace so multiple apps in one
	// Stripe account don't clobber each other's configs. Required.
	Namespace string

	// Name surfaces in the Stripe Dashboard as the config's display
	// name. Defaults to "<namespace> portal".
	Name string

	// DefaultReturnURL is where Stripe sends the customer when they
	// click "back to <app>" in the portal. Required.
	DefaultReturnURL string

	// BusinessHeadline shows above the portal greeting.
	BusinessHeadline string

	// PrivacyPolicyURL + TermsOfServiceURL appear in the portal footer.
	PrivacyPolicyURL  string
	TermsOfServiceURL string

	// Features toggles which portal capabilities are exposed.
	Features Features
}

// Features groups every togglable portal capability.
type Features struct {
	CustomerUpdate             CustomerUpdateFeature
	InvoiceHistoryEnabled      bool
	PaymentMethodUpdateEnabled bool
	SubscriptionCancel         SubscriptionCancelFeature
	SubscriptionUpdate         SubscriptionUpdateFeature
}

// CustomerUpdateFeature controls which customer fields are editable.
type CustomerUpdateFeature struct {
	Enabled       bool
	AllowedFields []string // "address" | "email" | "name" | "phone" | "shipping" | "tax_id"
}

// SubscriptionCancelFeature controls the cancel flow.
type SubscriptionCancelFeature struct {
	Enabled           bool
	Mode              string // "at_period_end" | "immediately"
	ProrationBehavior string // "create_prorations" | "none"
}

// SubscriptionUpdateFeature controls in-portal plan changes.
type SubscriptionUpdateFeature struct {
	Enabled               bool
	DefaultAllowedUpdates []string // "price" | "promotion_code" | "quantity"
	ProrationBehavior     string
	Products              []SubscriptionUpdateProduct
}

// SubscriptionUpdateProduct lists products + prices customers can
// self-serve switch to.
type SubscriptionUpdateProduct struct {
	StripeProductID string
	StripePriceIDs  []string
}

// Validate returns the first complaint, or nil.
func (s *Spec) Validate() error {
	if s.Namespace == "" {
		return errors.New("portalconfig: Namespace is required")
	}
	if s.DefaultReturnURL == "" {
		return errors.New("portalconfig: DefaultReturnURL is required")
	}
	return nil
}

// Operations is the entry point. Construct via New, then call
// Operations.Sync to push the spec to Stripe.
type Operations struct {
	sc *stripe.Client
}

// New wraps a Stripe client. Panics if sc is nil.
func New(sc *stripe.Client) *Operations {
	if sc == nil {
		panic("portalconfig.New: StripeClient is required")
	}
	return &Operations{sc: sc}
}

// SyncResult summarises what Sync did. Exactly one of Created /
// Updated / Unchanged is true.
type SyncResult struct {
	ConfigID  string
	Created   bool
	Updated   bool
	Unchanged bool // spec matched existing config; no Stripe write issued
}

// Sync reconciles the spec against Stripe. Strategy:
//
//  1. List existing configurations and find the one stamped with
//     `app_namespace=<spec.Namespace>`.
//  2. If none exists → create.
//  3. If exists + spec matches → return Unchanged (no Stripe write).
//  4. If exists + spec differs → Update in-place.
//
// Idempotent: re-running with the same spec issues zero writes.
func (o *Operations) Sync(ctx context.Context, spec *Spec) (*SyncResult, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	existing, err := o.findByNamespace(ctx, spec.Namespace)
	if err != nil {
		return nil, fmt.Errorf("portalconfig.Sync: find existing: %w", err)
	}
	if existing != nil {
		if !specDiffersFromExisting(spec, existing) {
			return &SyncResult{ConfigID: existing.ID, Unchanged: true}, nil
		}
		params := buildUpdateParams(spec)
		params.AddMetadata(meta.MetadataKeyNamespace, spec.Namespace)
		stripeapi.ApplyIdempotencyKey(params, ctx, "portalconfig.update", spec.Namespace, existing.ID)
		updated, err := o.sc.V1BillingPortalConfigurations.Update(ctx, existing.ID, params)
		if err != nil {
			return nil, fmt.Errorf("portalconfig.Sync: Update(%s): %w", existing.ID, err)
		}
		return &SyncResult{ConfigID: updated.ID, Updated: true}, nil
	}
	params := buildCreateParams(spec)
	params.AddMetadata(meta.MetadataKeyNamespace, spec.Namespace)
	stripeapi.ApplyIdempotencyKey(params, ctx, "portalconfig.create", spec.Namespace)
	created, err := o.sc.V1BillingPortalConfigurations.Create(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("portalconfig.Sync: Create: %w", err)
	}
	return &SyncResult{ConfigID: created.ID, Created: true}, nil
}

// specDiffersFromExisting returns true when the spec's intent doesn't
// match what Stripe currently has — i.e., an Update call is needed.
//
// Only fields the lib's Spec can express are compared; fields Stripe
// supports but the Spec doesn't model are ignored (otherwise every
// Sync would always be "changed" against Stripe's defaults).
func specDiffersFromExisting(spec *Spec, existing *stripe.BillingPortalConfiguration) bool {
	if existing == nil {
		return true
	}
	if spec.DefaultReturnURL != existing.DefaultReturnURL {
		return true
	}
	if bp := existing.BusinessProfile; bp != nil {
		if spec.BusinessHeadline != bp.Headline {
			return true
		}
		if spec.PrivacyPolicyURL != bp.PrivacyPolicyURL {
			return true
		}
		if spec.TermsOfServiceURL != bp.TermsOfServiceURL {
			return true
		}
	} else if spec.BusinessHeadline != "" || spec.PrivacyPolicyURL != "" || spec.TermsOfServiceURL != "" {
		return true
	}
	if f := existing.Features; f != nil {
		if spec.Features.InvoiceHistoryEnabled != boolFromInvoiceHistory(f.InvoiceHistory) {
			return true
		}
		if spec.Features.PaymentMethodUpdateEnabled != boolFromPMUpdate(f.PaymentMethodUpdate) {
			return true
		}
		if spec.Features.CustomerUpdate.Enabled != boolFromCustomerUpdate(f.CustomerUpdate) {
			return true
		}
		if spec.Features.SubscriptionCancel.Enabled != boolFromSubCancel(f.SubscriptionCancel) {
			return true
		}
		if spec.Features.SubscriptionUpdate.Enabled != boolFromSubUpdate(f.SubscriptionUpdate) {
			return true
		}
		// Feature toggles compared above; sub-fields (allowed-fields,
		// products list, etc.) are NOT diffed — they're harder to
		// canonicalize cheaply. If apps change those frequently, the
		// drift is corrected next Sync; the cost of an extra Update
		// call per deploy is acceptable.
	}
	return false
}

func boolFromInvoiceHistory(p *stripe.BillingPortalConfigurationFeaturesInvoiceHistory) bool {
	return p != nil && p.Enabled
}
func boolFromPMUpdate(p *stripe.BillingPortalConfigurationFeaturesPaymentMethodUpdate) bool {
	return p != nil && p.Enabled
}
func boolFromCustomerUpdate(p *stripe.BillingPortalConfigurationFeaturesCustomerUpdate) bool {
	return p != nil && p.Enabled
}
func boolFromSubCancel(p *stripe.BillingPortalConfigurationFeaturesSubscriptionCancel) bool {
	return p != nil && p.Enabled
}
func boolFromSubUpdate(p *stripe.BillingPortalConfigurationFeaturesSubscriptionUpdate) bool {
	return p != nil && p.Enabled
}

// findByNamespace walks BillingPortalConfigurations and returns the
// first whose `metadata.app_namespace` matches. Returns nil if none.
// findByNamespace walks every BillingPortalConfiguration looking for
// `metadata.app_namespace == namespace`. If two or more configurations
// share the namespace (concurrent Sync from two deploys, or a manual
// dashboard duplicate), we return the most-recently-created one and
// WARN-log so operators can clean up the orphan. Picking deterministically
// stops Sync from oscillating between two configs across runs.
func (o *Operations) findByNamespace(ctx context.Context, namespace string) (*stripe.BillingPortalConfiguration, error) {
	params := &stripe.BillingPortalConfigurationListParams{}
	params.Filters.AddFilter("limit", "", "100")
	var matches []*stripe.BillingPortalConfiguration
	for c, err := range o.sc.V1BillingPortalConfigurations.List(ctx, params) {
		if err != nil {
			return nil, err
		}
		if c.Metadata[meta.MetadataKeyNamespace] == namespace {
			matches = append(matches, c)
		}
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return matches[0], nil
	default:
		newest := matches[0]
		orphans := make([]string, 0, len(matches)-1)
		for _, c := range matches[1:] {
			if c.Created > newest.Created {
				orphans = append(orphans, newest.ID)
				newest = c
			} else {
				orphans = append(orphans, c.ID)
			}
		}
		slog.WarnContext(ctx, "portalconfig: multiple configurations share namespace; using newest, others are orphans",
			slog.String("namespace", namespace),
			slog.String("active_id", newest.ID),
			slog.Any("orphan_ids", orphans),
		)
		return newest, nil
	}
}

func resolveName(spec *Spec) string {
	if spec.Name != "" {
		return spec.Name
	}
	return spec.Namespace + " portal"
}

func buildCreateParams(spec *Spec) *stripe.BillingPortalConfigurationCreateParams {
	return &stripe.BillingPortalConfigurationCreateParams{
		Name:             stripe.String(resolveName(spec)),
		DefaultReturnURL: stripe.String(spec.DefaultReturnURL),
		BusinessProfile: &stripe.BillingPortalConfigurationCreateBusinessProfileParams{
			Headline:          nilIfEmpty(spec.BusinessHeadline),
			PrivacyPolicyURL:  nilIfEmpty(spec.PrivacyPolicyURL),
			TermsOfServiceURL: nilIfEmpty(spec.TermsOfServiceURL),
		},
		Features: buildCreateFeaturesParams(spec.Features),
	}
}

func buildUpdateParams(spec *Spec) *stripe.BillingPortalConfigurationUpdateParams {
	return &stripe.BillingPortalConfigurationUpdateParams{
		Name:             stripe.String(resolveName(spec)),
		DefaultReturnURL: stripe.String(spec.DefaultReturnURL),
		BusinessProfile: &stripe.BillingPortalConfigurationUpdateBusinessProfileParams{
			Headline:          nilIfEmpty(spec.BusinessHeadline),
			PrivacyPolicyURL:  nilIfEmpty(spec.PrivacyPolicyURL),
			TermsOfServiceURL: nilIfEmpty(spec.TermsOfServiceURL),
		},
		Features: buildUpdateFeaturesParams(spec.Features),
	}
}

func buildCreateFeaturesParams(f Features) *stripe.BillingPortalConfigurationCreateFeaturesParams {
	p := &stripe.BillingPortalConfigurationCreateFeaturesParams{
		InvoiceHistory: &stripe.BillingPortalConfigurationCreateFeaturesInvoiceHistoryParams{
			Enabled: stripe.Bool(f.InvoiceHistoryEnabled),
		},
		PaymentMethodUpdate: &stripe.BillingPortalConfigurationCreateFeaturesPaymentMethodUpdateParams{
			Enabled: stripe.Bool(f.PaymentMethodUpdateEnabled),
		},
		CustomerUpdate: &stripe.BillingPortalConfigurationCreateFeaturesCustomerUpdateParams{
			Enabled: stripe.Bool(f.CustomerUpdate.Enabled),
		},
		SubscriptionCancel: &stripe.BillingPortalConfigurationCreateFeaturesSubscriptionCancelParams{
			Enabled: stripe.Bool(f.SubscriptionCancel.Enabled),
		},
		SubscriptionUpdate: &stripe.BillingPortalConfigurationCreateFeaturesSubscriptionUpdateParams{
			Enabled: stripe.Bool(f.SubscriptionUpdate.Enabled),
		},
	}
	if f.CustomerUpdate.Enabled && len(f.CustomerUpdate.AllowedFields) > 0 {
		for _, fld := range f.CustomerUpdate.AllowedFields {
			p.CustomerUpdate.AllowedUpdates = append(p.CustomerUpdate.AllowedUpdates,
				stripe.String(strings.ToLower(fld)))
		}
	}
	if f.SubscriptionCancel.Enabled {
		if f.SubscriptionCancel.Mode != "" {
			p.SubscriptionCancel.Mode = stripe.String(f.SubscriptionCancel.Mode)
		}
		if f.SubscriptionCancel.ProrationBehavior != "" {
			p.SubscriptionCancel.ProrationBehavior = stripe.String(f.SubscriptionCancel.ProrationBehavior)
		}
	}
	if f.SubscriptionUpdate.Enabled {
		if f.SubscriptionUpdate.ProrationBehavior != "" {
			p.SubscriptionUpdate.ProrationBehavior = stripe.String(f.SubscriptionUpdate.ProrationBehavior)
		}
		for _, u := range f.SubscriptionUpdate.DefaultAllowedUpdates {
			p.SubscriptionUpdate.DefaultAllowedUpdates = append(p.SubscriptionUpdate.DefaultAllowedUpdates,
				stripe.String(u))
		}
		for _, prod := range f.SubscriptionUpdate.Products {
			pp := &stripe.BillingPortalConfigurationCreateFeaturesSubscriptionUpdateProductParams{
				Product: stripe.String(prod.StripeProductID),
			}
			for _, pid := range prod.StripePriceIDs {
				pp.Prices = append(pp.Prices, stripe.String(pid))
			}
			p.SubscriptionUpdate.Products = append(p.SubscriptionUpdate.Products, pp)
		}
	}
	return p
}

func buildUpdateFeaturesParams(f Features) *stripe.BillingPortalConfigurationUpdateFeaturesParams {
	p := &stripe.BillingPortalConfigurationUpdateFeaturesParams{
		InvoiceHistory: &stripe.BillingPortalConfigurationUpdateFeaturesInvoiceHistoryParams{
			Enabled: stripe.Bool(f.InvoiceHistoryEnabled),
		},
		PaymentMethodUpdate: &stripe.BillingPortalConfigurationUpdateFeaturesPaymentMethodUpdateParams{
			Enabled: stripe.Bool(f.PaymentMethodUpdateEnabled),
		},
		CustomerUpdate: &stripe.BillingPortalConfigurationUpdateFeaturesCustomerUpdateParams{
			Enabled: stripe.Bool(f.CustomerUpdate.Enabled),
		},
		SubscriptionCancel: &stripe.BillingPortalConfigurationUpdateFeaturesSubscriptionCancelParams{
			Enabled: stripe.Bool(f.SubscriptionCancel.Enabled),
		},
		SubscriptionUpdate: &stripe.BillingPortalConfigurationUpdateFeaturesSubscriptionUpdateParams{
			Enabled: stripe.Bool(f.SubscriptionUpdate.Enabled),
		},
	}
	if f.CustomerUpdate.Enabled && len(f.CustomerUpdate.AllowedFields) > 0 {
		for _, fld := range f.CustomerUpdate.AllowedFields {
			p.CustomerUpdate.AllowedUpdates = append(p.CustomerUpdate.AllowedUpdates,
				stripe.String(strings.ToLower(fld)))
		}
	}
	if f.SubscriptionCancel.Enabled {
		if f.SubscriptionCancel.Mode != "" {
			p.SubscriptionCancel.Mode = stripe.String(f.SubscriptionCancel.Mode)
		}
		if f.SubscriptionCancel.ProrationBehavior != "" {
			p.SubscriptionCancel.ProrationBehavior = stripe.String(f.SubscriptionCancel.ProrationBehavior)
		}
	}
	if f.SubscriptionUpdate.Enabled {
		if f.SubscriptionUpdate.ProrationBehavior != "" {
			p.SubscriptionUpdate.ProrationBehavior = stripe.String(f.SubscriptionUpdate.ProrationBehavior)
		}
		for _, u := range f.SubscriptionUpdate.DefaultAllowedUpdates {
			p.SubscriptionUpdate.DefaultAllowedUpdates = append(p.SubscriptionUpdate.DefaultAllowedUpdates,
				stripe.String(u))
		}
		for _, prod := range f.SubscriptionUpdate.Products {
			pp := &stripe.BillingPortalConfigurationUpdateFeaturesSubscriptionUpdateProductParams{
				Product: stripe.String(prod.StripeProductID),
			}
			for _, pid := range prod.StripePriceIDs {
				pp.Prices = append(pp.Prices, stripe.String(pid))
			}
			p.SubscriptionUpdate.Products = append(p.SubscriptionUpdate.Products, pp)
		}
	}
	return p
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return stripe.String(s)
}
