package customers

import (
	"context"
	"errors"
	"testing"

	"github.com/bds421/rho-stripe/checkout"
	"github.com/bds421/rho-stripe/subject"
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

func TestResolveStripeID_NoMapping(t *testing.T) {
	repo := checkout.NewMemoryCustomerRepo()
	ops := New(Config{StripeClient: &stripe.Client{}, CustomerRepo: repo})
	_, err := ops.resolveStripeID(context.Background(), "unknown-subj")
	if err == nil || !contains(err.Error(), "no Stripe customer for subject") {
		t.Fatalf("expected no-mapping error; got %v", err)
	}
}

func TestResolveStripeID_FoundMapping(t *testing.T) {
	repo := checkout.NewMemoryCustomerRepo()
	if err := repo.Upsert(context.Background(), subject.ID("subj-1"), "cus_TEST"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	ops := New(Config{StripeClient: &stripe.Client{}, CustomerRepo: repo})
	got, err := ops.resolveStripeID(context.Background(), "subj-1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "cus_TEST" {
		t.Fatalf("got %q want cus_TEST", got)
	}
}

func TestErrNoMapping_Identity(t *testing.T) {
	if !errors.Is(ErrNoMapping, ErrNoMapping) {
		t.Fatalf("errors.Is broken")
	}
}

func TestImportCustomerInput_Validation(t *testing.T) {
	repo := checkout.NewMemoryCustomerRepo()
	ops := New(Config{StripeClient: &stripe.Client{}, CustomerRepo: repo})
	cases := []struct {
		name string
		in   ImportCustomerInput
		want string
	}{
		{"missing subject", ImportCustomerInput{StripeCustomerID: "cus_X", Namespace: "ns"}, "SubjectID is required"},
		{"missing stripe id", ImportCustomerInput{SubjectID: "s", Namespace: "ns"}, "StripeCustomerID is required"},
		{"missing namespace", ImportCustomerInput{SubjectID: "s", StripeCustomerID: "cus_X"}, "Namespace is required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ops.ImportCustomer(context.Background(), c.in)
			if err == nil || !contains(err.Error(), c.want) {
				t.Fatalf("want %q got %v", c.want, err)
			}
		})
	}
}

func TestAddTaxIDInput_Validation(t *testing.T) {
	repo := checkout.NewMemoryCustomerRepo()
	ops := New(Config{StripeClient: &stripe.Client{}, CustomerRepo: repo})
	cases := []struct {
		in   AddTaxIDInput
		want string
	}{
		{AddTaxIDInput{}, "SubjectID is required"},
		{AddTaxIDInput{SubjectID: "s"}, "Type and Value are required"},
		{AddTaxIDInput{SubjectID: "s", Type: "eu_vat"}, "Type and Value are required"},
	}
	for _, c := range cases {
		_, err := ops.AddTaxID(context.Background(), c.in)
		if err == nil || !contains(err.Error(), c.want) {
			t.Fatalf("want %q got %v", c.want, err)
		}
	}
}

func TestRemoveTaxID_RequiresStripeID(t *testing.T) {
	repo := checkout.NewMemoryCustomerRepo()
	ops := New(Config{StripeClient: &stripe.Client{}, CustomerRepo: repo})
	if err := ops.RemoveTaxID(context.Background(), "s", ""); err == nil || !contains(err.Error(), "taxIDStripeID is required") {
		t.Fatalf("want validation; got %v", err)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
