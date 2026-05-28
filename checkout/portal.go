package checkout

import (
	"context"
	"errors"
	"fmt"
)

// PortalInput is the data required to create a Customer Portal session.
type PortalInput struct {
	// SubjectID identifies the customer; the lib resolves it to a Stripe
	// Customer via CustomerRepo. The customer must already exist (the
	// portal is for managing existing customers; for first-purchase
	// flows, use CreateSession instead).
	SubjectID SubjectID

	// ReturnURL is where Stripe redirects after the customer leaves
	// the portal. Required.
	ReturnURL string

	// Flow optionally deep-links the customer into a specific portal
	// flow (e.g. cancel a subscription, update payment method) instead
	// of dropping them on the portal home.
	Flow *PortalFlow
}

// PortalSession is the projection of a created Stripe Billing Portal
// session.
type PortalSession struct {
	ID  string
	URL string
}

// PortalFlow optionally deep-links the customer into a specific
// portal action. nil means open the portal home.
type PortalFlow struct {
	Type           PortalFlowType
	SubscriptionID string // required for SubscriptionCancel / SubscriptionUpdate
}

// PortalFlowType enumerates the deep-link flows the lib exposes. The
// full set in Stripe is larger; phase 0 covers the common cases.
type PortalFlowType string

const (
	PortalFlowSubscriptionCancel  PortalFlowType = "subscription_cancel"
	PortalFlowSubscriptionUpdate  PortalFlowType = "subscription_update"
	PortalFlowPaymentMethodUpdate PortalFlowType = "payment_method_update"
)

// PortalSessionCreate is the params struct passed to Backend
// implementations. Apps don't construct this directly; CreatePortalSession
// builds it after resolving the SubjectID.
type PortalSessionCreate struct {
	StripeCustomerID StripeCustomerID
	ReturnURL        string
	Flow             *PortalFlow
}

// Errors specific to portal creation.
var (
	ErrPortalSubjectMissing          = errors.New("checkout: PortalInput.SubjectID is required")
	ErrPortalReturnURLMissing        = errors.New("checkout: PortalInput.ReturnURL is required")
	ErrPortalCustomerUnknown         = errors.New("checkout: no Stripe Customer found for subject — create a checkout session first")
	ErrPortalFlowSubscriptionMissing = errors.New("checkout: PortalFlow.SubscriptionID is required for cancel/update flows")
)

// CreatePortalSession resolves the subject to its Stripe Customer and
// creates a Customer Portal session. Returns the hosted URL the app
// redirects the customer to.
func (c *Checkout) CreatePortalSession(ctx context.Context, in PortalInput) (PortalSession, error) {
	if in.SubjectID == "" {
		return PortalSession{}, ErrPortalSubjectMissing
	}
	if in.ReturnURL == "" {
		return PortalSession{}, ErrPortalReturnURLMissing
	}
	if in.Flow != nil {
		if (in.Flow.Type == PortalFlowSubscriptionCancel || in.Flow.Type == PortalFlowSubscriptionUpdate) && in.Flow.SubscriptionID == "" {
			return PortalSession{}, ErrPortalFlowSubscriptionMissing
		}
	}

	id, ok, err := c.cfg.Customers.Get(ctx, in.SubjectID)
	if err != nil {
		return PortalSession{}, fmt.Errorf("customers.Get: %w", err)
	}
	if !ok {
		return PortalSession{}, ErrPortalCustomerUnknown
	}

	return c.cfg.Backend.CreatePortalSession(ctx, PortalSessionCreate{
		StripeCustomerID: id,
		ReturnURL:        in.ReturnURL,
		Flow:             in.Flow,
	})
}
