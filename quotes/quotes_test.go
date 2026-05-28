package quotes

import (
	"context"
	"testing"

	stripe "github.com/stripe/stripe-go/v82"
)

func TestNew_PanicsOnNilClient(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil client")
		}
	}()
	_ = New(Config{Namespace: "ns"})
}

func TestCreate_ValidatesCustomer(t *testing.T) {
	ops := New(Config{StripeClient: &stripe.Client{}, Namespace: "ns"})
	_, err := ops.Create(context.Background(), CreateInput{
		LineItems: []QuoteLine{{StripePriceID: "price_X", Quantity: 1}},
	})
	if err == nil || err.Error() != "quotes.Create: StripeCustomerID is required" {
		t.Fatalf("want customer validation; got %v", err)
	}
}

func TestCreate_RequiresLineItems(t *testing.T) {
	ops := New(Config{StripeClient: &stripe.Client{}, Namespace: "ns"})
	_, err := ops.Create(context.Background(), CreateInput{
		StripeCustomerID: "cus_X",
	})
	if err == nil || err.Error() != "quotes.Create: at least one LineItem is required" {
		t.Fatalf("want line-item validation; got %v", err)
	}
}

func TestFinalize_Cancel_Accept_Get_ValidateID(t *testing.T) {
	ops := New(Config{StripeClient: &stripe.Client{}, Namespace: "ns"})
	if _, err := ops.Finalize(context.Background(), ""); err == nil {
		t.Error("Finalize: want validation")
	}
	if _, err := ops.Cancel(context.Background(), ""); err == nil {
		t.Error("Cancel: want validation")
	}
	if _, err := ops.Accept(context.Background(), ""); err == nil {
		t.Error("Accept: want validation")
	}
	if _, err := ops.Retrieve(context.Background(), ""); err == nil {
		t.Error("Get: want validation")
	}
}

func TestCreateInputHash_Stable(t *testing.T) {
	in := CreateInput{
		StripeCustomerID: "cus_X",
		LineItems:        []QuoteLine{{StripePriceID: "p1", Quantity: 2}},
		TrialPeriodDays:  14,
		Metadata:         map[string]string{"a": "1", "b": "2"},
	}
	a := createInputHash(in)
	b := createInputHash(in)
	if a != b {
		t.Errorf("hash not stable: %s vs %s", a, b)
	}
}

func TestCreateInputHash_DistinguishesTrialDays(t *testing.T) {
	base := CreateInput{
		StripeCustomerID: "cus_X",
		LineItems:        []QuoteLine{{StripePriceID: "p1", Quantity: 2}},
		TrialPeriodDays:  14,
	}
	other := base
	other.TrialPeriodDays = 7
	if createInputHash(base) == createInputHash(other) {
		t.Errorf("hash collided across TrialPeriodDays — slice-51 bug class regressed")
	}
}

func TestCreateInputHash_DeterministicAcrossMetadataIteration(t *testing.T) {
	in1 := CreateInput{
		StripeCustomerID: "cus_X",
		LineItems:        []QuoteLine{{StripePriceID: "p1", Quantity: 1}},
		Metadata:         map[string]string{"a": "1", "b": "2", "c": "3"},
	}
	want := createInputHash(in1)
	for i := 0; i < 25; i++ {
		if got := createInputHash(in1); got != want {
			t.Fatalf("non-deterministic on iter %d: %s vs %s", i, got, want)
		}
	}
}
