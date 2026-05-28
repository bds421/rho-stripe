package paymentmethods

import (
	"context"
	"testing"

	"github.com/bds421/rho-stripe/checkout"
	stripe "github.com/stripe/stripe-go/v82"
)

func TestNew_PanicsOnMissingStripeClient(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on missing StripeClient")
		}
	}()
	_ = New(Config{CustomerRepo: checkout.NewMemoryCustomerRepo()})
}

func TestNew_PanicsOnMissingCustomerRepo(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on missing CustomerRepo")
		}
	}()
	_ = New(Config{StripeClient: &stripe.Client{}})
}

func TestCreateSetupIntent_ValidatesSubject(t *testing.T) {
	ops := New(Config{StripeClient: &stripe.Client{}, CustomerRepo: checkout.NewMemoryCustomerRepo()})
	_, err := ops.CreateSetupIntent(context.Background(), CreateSetupIntentInput{})
	if err == nil || err.Error() != "paymentmethods.CreateSetupIntent: SubjectID is required" {
		t.Fatalf("want SubjectID validation; got %v", err)
	}
}

func TestAttachPaymentMethod_ValidatesID(t *testing.T) {
	ops := New(Config{StripeClient: &stripe.Client{}, CustomerRepo: checkout.NewMemoryCustomerRepo()})
	err := ops.AttachPaymentMethod(context.Background(), "subj", "")
	if err == nil || err.Error() != "paymentmethods.AttachPaymentMethod: paymentMethodID is required" {
		t.Fatalf("want pm-required; got %v", err)
	}
}

func TestDetachPaymentMethod_ValidatesID(t *testing.T) {
	ops := New(Config{StripeClient: &stripe.Client{}, CustomerRepo: checkout.NewMemoryCustomerRepo()})
	err := ops.DetachPaymentMethod(context.Background(), "")
	if err == nil || err.Error() != "paymentmethods.DetachPaymentMethod: paymentMethodID is required" {
		t.Fatalf("want pm-required; got %v", err)
	}
}

func TestSetDefaultPaymentMethod_ValidatesID(t *testing.T) {
	ops := New(Config{StripeClient: &stripe.Client{}, CustomerRepo: checkout.NewMemoryCustomerRepo()})
	err := ops.SetDefaultPaymentMethod(context.Background(), "subj", "")
	if err == nil || err.Error() != "paymentmethods.SetDefaultPaymentMethod: paymentMethodID is required" {
		t.Fatalf("want pm-required; got %v", err)
	}
}

func TestResolveCustomer_NoMapping(t *testing.T) {
	ops := New(Config{StripeClient: &stripe.Client{}, CustomerRepo: checkout.NewMemoryCustomerRepo()})
	_, err := ops.resolveCustomer(context.Background(), "unknown")
	if err == nil || !contains(err.Error(), "no Stripe customer for subject unknown") {
		t.Fatalf("want no-mapping; got %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
