package payments_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/bds421/rho-stripe/payments"
)

type fakeBackend struct {
	mu    sync.Mutex
	calls []payments.RefundInput
	err   error
}

func (f *fakeBackend) Refund(_ context.Context, in payments.RefundInput) (payments.Refund, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, in)
	if f.err != nil {
		return payments.Refund{}, f.err
	}
	return payments.Refund{
		StripeID: "re_test",
		ChargeID: in.ChargeID,
		Amount:   in.Amount,
		Status:   "succeeded",
	}, nil
}

func TestRefund_RequiresEitherChargeOrPaymentIntent(t *testing.T) {
	ops := payments.New(&fakeBackend{})
	_, err := ops.Refund(t.Context(), payments.RefundInput{})
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Errorf("expected required error, got %v", err)
	}
}

func TestRefund_RejectsBoth(t *testing.T) {
	ops := payments.New(&fakeBackend{})
	_, err := ops.Refund(t.Context(), payments.RefundInput{
		ChargeID: "ch_x", PaymentIntentID: "pi_y",
	})
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Errorf("expected 'not both' error, got %v", err)
	}
}

func TestRefund_DefaultsReason(t *testing.T) {
	be := &fakeBackend{}
	ops := payments.New(be)
	if _, err := ops.Refund(t.Context(), payments.RefundInput{ChargeID: "ch_x"}); err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if be.calls[0].Reason != payments.RefundReasonRequestedByCustomer {
		t.Errorf("default reason = %q", be.calls[0].Reason)
	}
}

func TestRefund_RejectsInvalidReason(t *testing.T) {
	ops := payments.New(&fakeBackend{})
	_, err := ops.Refund(t.Context(), payments.RefundInput{
		ChargeID: "ch_x", Reason: payments.RefundReason("bogus"),
	})
	if err == nil || !strings.Contains(err.Error(), "invalid Reason") {
		t.Errorf("expected invalid-reason error, got %v", err)
	}
}

func TestRefund_StampsNamespaceWhenSet(t *testing.T) {
	be := &fakeBackend{}
	ops := payments.New(be, payments.WithNamespace("myapp"))
	if _, err := ops.Refund(t.Context(), payments.RefundInput{ChargeID: "ch_x"}); err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if be.calls[0].Metadata["app_namespace"] != "myapp" {
		t.Errorf("namespace not stamped: %v", be.calls[0].Metadata)
	}
}

func TestRefund_NegativeAmountRejected(t *testing.T) {
	ops := payments.New(&fakeBackend{})
	_, err := ops.Refund(t.Context(), payments.RefundInput{
		ChargeID: "ch_x", Amount: -100,
	})
	if err == nil {
		t.Error("expected negative-amount error")
	}
}

func TestRefund_BackendErrorBubbles(t *testing.T) {
	be := &fakeBackend{err: errors.New("stripe down")}
	ops := payments.New(be)
	_, err := ops.Refund(t.Context(), payments.RefundInput{ChargeID: "ch_x"})
	if err == nil || !strings.Contains(err.Error(), "stripe down") {
		t.Errorf("expected backend error, got %v", err)
	}
}
