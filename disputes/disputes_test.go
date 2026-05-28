package disputes

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
	_ = New(nil)
}

func TestSubmitEvidence_ValidatesDisputeID(t *testing.T) {
	ops := New(&stripe.Client{})
	_, err := ops.SubmitEvidence(context.Background(), "", Evidence{})
	if err == nil || err.Error() != "disputes.SubmitEvidence: disputeID is required" {
		t.Fatalf("want validation; got %v", err)
	}
}

func TestClose_ValidatesDisputeID(t *testing.T) {
	ops := New(&stripe.Client{})
	_, err := ops.Close(context.Background(), "")
	if err == nil || err.Error() != "disputes.Close: disputeID is required" {
		t.Fatalf("want validation; got %v", err)
	}
}

func TestRetrieve_ValidatesDisputeID(t *testing.T) {
	ops := New(&stripe.Client{})
	_, err := ops.Retrieve(context.Background(), "")
	if err == nil || err.Error() != "disputes.Retrieve: disputeID is required" {
		t.Fatalf("want validation; got %v", err)
	}
}

func TestEvidenceContentHash_DistinguishesContent(t *testing.T) {
	a := evidenceContentHash(Evidence{CustomerName: "Alice"})
	b := evidenceContentHash(Evidence{CustomerName: "Bob"})
	if a == b {
		t.Errorf("hash collided across CustomerName — disputes idempotency bug")
	}
}

func TestEvidenceContentHash_StableForIdenticalInput(t *testing.T) {
	e := Evidence{CustomerName: "Alice", ProductDescription: "SaaS", ShippingDate: "2026-05-01"}
	a := evidenceContentHash(e)
	b := evidenceContentHash(e)
	if a != b {
		t.Errorf("hash not stable: %s vs %s", a, b)
	}
}

func TestEvidenceContentHash_FieldNamesNamespaced(t *testing.T) {
	a := evidenceContentHash(Evidence{CustomerName: "X", CustomerEmail: "Y"})
	b := evidenceContentHash(Evidence{CustomerName: "Y", CustomerEmail: "X"})
	if a == b {
		t.Errorf("field-name namespacing broken — values swapped between fields collide")
	}
}

func TestEvidenceContentHash_EnhancedEvidenceParticipates(t *testing.T) {
	base := Evidence{CustomerName: "X"}
	enhanced := base
	enhanced.EnhancedEvidence = &EnhancedEvidence{
		VisaCompellingEvidence3: &VisaCE3{DisputedTransactionID: "ch_X"},
	}
	if evidenceContentHash(base) == evidenceContentHash(enhanced) {
		t.Errorf("EnhancedEvidence didn't contribute to hash")
	}
}

func TestBuildEvidenceParams_OnlySetsNonEmpty(t *testing.T) {
	e := Evidence{
		CustomerName:       "Acme Corp",
		ProductDescription: "SaaS subscription",
	}
	p := buildEvidenceParams(e)
	if p.CustomerName == nil || *p.CustomerName != "Acme Corp" {
		t.Errorf("CustomerName not set")
	}
	if p.ProductDescription == nil || *p.ProductDescription != "SaaS subscription" {
		t.Errorf("ProductDescription not set")
	}
	if p.ShippingAddress != nil {
		t.Errorf("ShippingAddress should be nil for empty input")
	}
	if p.CustomerEmailAddress != nil {
		t.Errorf("CustomerEmailAddress should be nil for empty input")
	}
}
