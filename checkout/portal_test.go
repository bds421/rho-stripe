package checkout_test

import (
	"errors"
	"testing"

	"github.com/bds421/rho-stripe/checkout"
)

func TestCreatePortalSession_HappyPath(t *testing.T) {
	be := &fakeBackend{}
	c, repo := newCheckout(t, map[string]string{}, be)
	_ = repo.Upsert(t.Context(), "org_acme", "cus_known")

	sess, err := c.CreatePortalSession(t.Context(), checkout.PortalInput{
		SubjectID:   "org_acme",
		ReturnURL: "https://app.example.com/billing",
	})
	if err != nil {
		t.Fatalf("CreatePortalSession: %v", err)
	}
	if sess.URL == "" {
		t.Error("portal URL is empty")
	}
	if len(be.portalCreated) != 1 {
		t.Fatalf("expected 1 portal session, got %d", len(be.portalCreated))
	}
	if be.portalCreated[0].StripeCustomerID != "cus_known" {
		t.Errorf("StripeCustomerID = %q", be.portalCreated[0].StripeCustomerID)
	}
	if be.portalCreated[0].ReturnURL != "https://app.example.com/billing" {
		t.Errorf("ReturnURL = %q", be.portalCreated[0].ReturnURL)
	}
}

func TestCreatePortalSession_UnknownCustomerErrors(t *testing.T) {
	be := &fakeBackend{}
	c, _ := newCheckout(t, map[string]string{}, be) // empty repo

	_, err := c.CreatePortalSession(t.Context(), checkout.PortalInput{
		SubjectID:   "org_unknown",
		ReturnURL: "https://x",
	})
	if !errors.Is(err, checkout.ErrPortalCustomerUnknown) {
		t.Errorf("expected ErrPortalCustomerUnknown, got %v", err)
	}
}

func TestCreatePortalSession_MissingFields(t *testing.T) {
	be := &fakeBackend{}
	c, repo := newCheckout(t, map[string]string{}, be)
	_ = repo.Upsert(t.Context(), "org_acme", "cus_known")

	for name, in := range map[string]checkout.PortalInput{
		"subject":    {ReturnURL: "https://x"},
		"return_url": {SubjectID: "org_acme"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := c.CreatePortalSession(t.Context(), in)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestCreatePortalSession_FlowDeepLinkPassedThrough(t *testing.T) {
	be := &fakeBackend{}
	c, repo := newCheckout(t, map[string]string{}, be)
	_ = repo.Upsert(t.Context(), "org_acme", "cus_known")

	flow := &checkout.PortalFlow{
		Type:           checkout.PortalFlowSubscriptionCancel,
		SubscriptionID: "sub_xyz",
	}
	_, err := c.CreatePortalSession(t.Context(), checkout.PortalInput{
		SubjectID:   "org_acme",
		ReturnURL: "https://x",
		Flow:      flow,
	})
	if err != nil {
		t.Fatalf("CreatePortalSession: %v", err)
	}
	got := be.portalCreated[0].Flow
	if got == nil || got.Type != checkout.PortalFlowSubscriptionCancel || got.SubscriptionID != "sub_xyz" {
		t.Errorf("Flow not passed through correctly: %+v", got)
	}
}

func TestCreatePortalSession_SubscriptionFlowRequiresSubID(t *testing.T) {
	be := &fakeBackend{}
	c, repo := newCheckout(t, map[string]string{}, be)
	_ = repo.Upsert(t.Context(), "org_acme", "cus_known")

	flow := &checkout.PortalFlow{
		Type: checkout.PortalFlowSubscriptionCancel,
		// SubscriptionID intentionally empty
	}
	_, err := c.CreatePortalSession(t.Context(), checkout.PortalInput{
		SubjectID:   "org_acme",
		ReturnURL: "https://x",
		Flow:      flow,
	})
	if !errors.Is(err, checkout.ErrPortalFlowSubscriptionMissing) {
		t.Errorf("expected ErrPortalFlowSubscriptionMissing, got %v", err)
	}
}

func TestCreatePortalSession_PaymentMethodFlowDoesNotRequireSubID(t *testing.T) {
	be := &fakeBackend{}
	c, repo := newCheckout(t, map[string]string{}, be)
	_ = repo.Upsert(t.Context(), "org_acme", "cus_known")

	flow := &checkout.PortalFlow{Type: checkout.PortalFlowPaymentMethodUpdate}
	_, err := c.CreatePortalSession(t.Context(), checkout.PortalInput{
		SubjectID:   "org_acme",
		ReturnURL: "https://x",
		Flow:      flow,
	})
	if err != nil {
		t.Errorf("PaymentMethodUpdate flow should not require SubscriptionID; got %v", err)
	}
}
