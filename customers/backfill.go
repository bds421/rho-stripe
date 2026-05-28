package customers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bds421/rho-stripe/checkout"
	"github.com/bds421/rho-stripe/meta"
	"github.com/bds421/rho-stripe/stripeapi"
	"github.com/bds421/rho-stripe/subject"
	"github.com/bds421/rho-stripe/subscriptions"
	stripe "github.com/stripe/stripe-go/v82"
)

// ImportCustomerInput configures a backfill of an existing Stripe
// customer into the lib's namespace.
//
// Use case: an app already has customers in Stripe (e.g. migrated
// from a previous billing setup) and is adopting rho-stripe.
// The lib normally tags newly-created Stripe customers with
// `metadata.app_namespace = <ns>` so webhook dispatch can filter by
// namespace. Existing customers lack this tag; without backfill, the
// lib will reject their events at the namespace filter.
//
// ImportCustomer:
//
//  1. Adds `metadata.app_namespace = cfg.Namespace` to the Stripe
//     customer (if not already present, or AdoptNamespace=true to
//     overwrite a different namespace).
//  2. Inserts a CustomerRepo mapping subject ↔ stripe customer id.
//  3. Optionally backfills the subscription mirror by fetching every
//     subscription currently on the Stripe customer.
type ImportCustomerInput struct {
	// SubjectID is the app's stable identity for the customer.
	SubjectID SubjectID

	// StripeCustomerID is the existing Stripe customer id (cus_…).
	StripeCustomerID string

	// Namespace is the app's namespace string. Required.
	Namespace string

	// AdoptNamespace controls behavior when the customer already has
	// metadata.app_namespace set to a *different* value:
	//   - false (default): return ErrNamespaceConflict — caller must
	//     decide.
	//   - true: overwrite. Use only when the customer is genuinely
	//     migrating from one app namespace to another.
	AdoptNamespace bool

	// BackfillSubscriptions toggles fetching every Stripe subscription
	// for the customer and upserting it into the lib's mirror.
	// Defaults to true. Requires Config.SubscriptionRepo to be set.
	BackfillSubscriptions bool
}

// ImportCustomerResult summarises what the backfill did.
type ImportCustomerResult struct {
	SubjectID                SubjectID
	StripeCustomerID         string
	NamespaceStamped         bool // true if we wrote the metadata stamp
	PreviouslyHadDifferentNS bool // true if we overwrote a different ns
	PreviousNamespace        string
	SubscriptionsBackfilled  int
}

// ErrNamespaceConflict is returned when the Stripe customer already
// has metadata.app_namespace set to a different value and the caller
// didn't opt in to AdoptNamespace=true.
var ErrNamespaceConflict = errors.New("customers: stripe customer already in a different namespace; set AdoptNamespace=true to overwrite")

// ImportCustomer is the entry point. See ImportCustomerInput.
func (o *Operations) ImportCustomer(ctx context.Context, in ImportCustomerInput) (*ImportCustomerResult, error) {
	if in.SubjectID == "" {
		return nil, errors.New("customers.ImportCustomer: SubjectID is required")
	}
	if in.StripeCustomerID == "" {
		return nil, errors.New("customers.ImportCustomer: StripeCustomerID is required")
	}
	if in.Namespace == "" {
		return nil, errors.New("customers.ImportCustomer: Namespace is required")
	}

	result := &ImportCustomerResult{
		SubjectID:        in.SubjectID,
		StripeCustomerID: in.StripeCustomerID,
	}

	// Read current Stripe customer state.
	cust, err := o.cfg.StripeClient.V1Customers.Retrieve(ctx, in.StripeCustomerID, nil)
	if err != nil {
		return nil, fmt.Errorf("customers.ImportCustomer: Customers.Retrieve(%s): %w", in.StripeCustomerID, err)
	}

	existingNS, hasNS := cust.Metadata[meta.MetadataKeyNamespace]
	switch {
	case hasNS && existingNS == in.Namespace:
		// already correctly stamped — no Stripe call needed
	case hasNS && existingNS != in.Namespace && !in.AdoptNamespace:
		result.PreviouslyHadDifferentNS = true
		result.PreviousNamespace = existingNS
		return result, ErrNamespaceConflict
	default:
		// Stamp it (either it was unstamped, or we're adopting over an
		// existing different namespace).
		updParams := &stripe.CustomerUpdateParams{}
		updParams.AddMetadata(meta.MetadataKeyNamespace, in.Namespace)
		stripeapi.ApplyIdempotencyKey(updParams, ctx, "customers.import.stamp",
			in.StripeCustomerID, in.Namespace)
		if _, err := o.cfg.StripeClient.V1Customers.Update(ctx, in.StripeCustomerID, updParams); err != nil {
			return result, fmt.Errorf("customers.ImportCustomer: Customers.Update(stamp ns): %w", err)
		}
		result.NamespaceStamped = true
		if hasNS && existingNS != in.Namespace {
			result.PreviouslyHadDifferentNS = true
			result.PreviousNamespace = existingNS
		}
	}

	// Upsert the local mapping.
	if err := o.cfg.CustomerRepo.Upsert(ctx, subject.ID(in.SubjectID), checkout.StripeCustomerID(in.StripeCustomerID)); err != nil {
		return result, fmt.Errorf("customers.ImportCustomer: CustomerRepo.Upsert: %w", err)
	}

	// Optionally backfill subscriptions.
	backfillSubs := in.BackfillSubscriptions || !in.explicitOptOut()
	if backfillSubs && o.cfg.SubscriptionRepo != nil {
		n, err := o.backfillSubscriptions(ctx, in.SubjectID, in.StripeCustomerID, in.Namespace)
		if err != nil {
			return result, fmt.Errorf("customers.ImportCustomer: backfill subs: %w", err)
		}
		result.SubscriptionsBackfilled = n
	}

	return result, nil
}

// explicitOptOut returns true if the caller explicitly set
// BackfillSubscriptions=false. Helper for clarity at the call site.
func (in ImportCustomerInput) explicitOptOut() bool {
	return !in.BackfillSubscriptions
}

// backfillSubscriptions fetches every subscription on the Stripe
// customer and upserts the lib's mirror with each.
//
// We also stamp `app_namespace` on each subscription that lacks one,
// so its future webhook events route correctly. This is a side effect
// that's safe (Stripe accepts redundant metadata writes) and removes
// a footgun from operators (forgetting to stamp subscriptions causes
// silent webhook-filter drops).
func (o *Operations) backfillSubscriptions(ctx context.Context, s SubjectID, stripeCustomerID, namespace string) (int, error) {
	listParams := &stripe.SubscriptionListParams{Customer: stripe.String(stripeCustomerID)}
	listParams.Filters.AddFilter("status", "", "all")
	listParams.Filters.AddFilter("limit", "", "100")
	count := 0
	for ss, err := range o.cfg.StripeClient.V1Subscriptions.List(ctx, listParams) {
		if err != nil {
			return count, fmt.Errorf("Subscriptions.List(%s): %w", stripeCustomerID, err)
		}
		if existing, ok := ss.Metadata[meta.MetadataKeyNamespace]; !ok || existing != namespace {
			updParams := &stripe.SubscriptionUpdateParams{}
			updParams.AddMetadata(meta.MetadataKeyNamespace, namespace)
			stripeapi.ApplyIdempotencyKey(updParams, ctx, "customers.import.stamp_sub",
				ss.ID, namespace)
			updated, err := o.cfg.StripeClient.V1Subscriptions.Update(ctx, ss.ID, updParams)
			if err != nil {
				return count, fmt.Errorf("Subscriptions.Update(%s, stamp ns): %w", ss.ID, err)
			}
			ss = updated
		}
		sub := projectImportedSubscription(s, ss)
		if err := o.cfg.SubscriptionRepo.Upsert(ctx, sub); err != nil {
			return count, fmt.Errorf("SubscriptionRepo.Upsert(%s): %w", ss.ID, err)
		}
		count++
	}
	return count, nil
}

func projectImportedSubscription(s SubjectID, ss *stripe.Subscription) *subscriptions.Subscription {
	sub := &subscriptions.Subscription{
		StripeID:          ss.ID,
		SubjectID:         subscriptions.SubjectID(s),
		Status:            subscriptions.Status(ss.Status),
		CancelAtPeriodEnd: ss.CancelAtPeriodEnd,
		Metadata:          ss.Metadata,
		UpdatedAt:         time.Unix(ss.Created, 0).UTC(),
		StripeUpdatedAt:   time.Unix(ss.Created, 0).UTC(),
	}
	if ss.Customer != nil {
		sub.StripeCustomerID = ss.Customer.ID
	}
	if ss.Items != nil {
		for _, it := range ss.Items.Data {
			si := subscriptions.SubscriptionItem{StripeID: it.ID, Quantity: it.Quantity}
			if it.Price != nil {
				if it.Price.LookupKey != "" {
					si.PriceKey = it.Price.LookupKey
				} else {
					si.PriceKey = "stripe:" + it.Price.ID
				}
			}
			sub.Items = append(sub.Items, si)
			if sub.CurrentPeriodStart.IsZero() && it.CurrentPeriodStart > 0 {
				sub.CurrentPeriodStart = time.Unix(it.CurrentPeriodStart, 0).UTC()
				sub.CurrentPeriodEnd = time.Unix(it.CurrentPeriodEnd, 0).UTC()
			}
		}
	}
	if ss.CanceledAt > 0 {
		t := time.Unix(ss.CanceledAt, 0).UTC()
		sub.CanceledAt = &t
	}
	if ss.EndedAt > 0 {
		t := time.Unix(ss.EndedAt, 0).UTC()
		sub.EndedAt = &t
	}
	if ss.TrialEnd > 0 {
		t := time.Unix(ss.TrialEnd, 0)
		sub.TrialEnd = &t
	}
	return sub
}
