package credits_test

import (
	"encoding/json"
	"testing"

	"github.com/bds421/rho-stripe/credits"
	"github.com/bds421/rho-stripe/webhooks"
	stripe "github.com/stripe/stripe-go/v82"
)

// refundEvent builds a webhook.Event with raw data shaped as Stripe
// would send for the given event type. eventType controls which
// payload shape: "charge.refunded" puts ID at the top + refunds.data;
// "refund.created" puts ID + payment_intent/charge directly on object.
func refundEvent(t *testing.T, evtType string, payload map[string]any) webhooks.Event {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return webhooks.Event{
		ID:   "evt_x",
		Type: evtType,
		Raw: &stripe.Event{
			ID:   "evt_x",
			Type: stripe.EventType(evtType),
			Data: &stripe.EventData{Raw: raw},
		},
	}
}

func TestApplyRefundReversal_RevokesGrantsTiedToPaymentIntent(t *testing.T) {
	repo := credits.NewMemoryRepo()
	ctx := t.Context()

	// Two grants share the PI source ref; one unrelated grant should NOT be touched.
	g1, err := repo.Grant(ctx, credits.GrantInput{
		SubjectID: "user_a", Bucket: "credits", Amount: 1000,
		Source: credits.SourceStripePayment, SourceRef: "pi_target",
	})
	if err != nil {
		t.Fatalf("Grant 1: %v", err)
	}
	g2, err := repo.Grant(ctx, credits.GrantInput{
		SubjectID: "user_b", Bucket: "credits", Amount: 500,
		Source: credits.SourceStripePayment, SourceRef: "pi_target",
	})
	if err != nil {
		t.Fatalf("Grant 2: %v", err)
	}
	// Unrelated grant on a different PI.
	g3, err := repo.Grant(ctx, credits.GrantInput{
		SubjectID: "user_c", Bucket: "credits", Amount: 9999,
		Source: credits.SourceStripePayment, SourceRef: "pi_unrelated",
	})
	if err != nil {
		t.Fatalf("Grant 3: %v", err)
	}

	evt := refundEvent(t, "refund.created", map[string]any{
		"id":             "re_xxx",
		"object":         "refund",
		"payment_intent": "pi_target",
		"charge":         "ch_xxx",
	})

	if err := credits.ApplyRefundReversal(ctx, repo, evt, nil); err != nil {
		t.Fatalf("ApplyRefundReversal: %v", err)
	}

	// g1 + g2 revoked → balance is 0; g3 untouched.
	verifyRevoked(t, repo, "user_a", "credits", 0)
	verifyRevoked(t, repo, "user_b", "credits", 0)
	verifyRevoked(t, repo, "user_c", "credits", 9999)

	// Re-deliver — should be idempotent (no error, no extra revocation).
	if err := credits.ApplyRefundReversal(ctx, repo, evt, nil); err != nil {
		t.Errorf("re-delivery: %v", err)
	}
	_ = g1
	_ = g2
	_ = g3
}

func TestApplyRefundReversal_HandlesChargeRefundedShape(t *testing.T) {
	repo := credits.NewMemoryRepo()
	ctx := t.Context()
	_, _ = repo.Grant(ctx, credits.GrantInput{
		SubjectID: "user", Bucket: "credits", Amount: 100,
		Source: credits.SourceStripePayment, SourceRef: "ch_main",
	})

	evt := refundEvent(t, "charge.refunded", map[string]any{
		"id":     "ch_main",
		"object": "charge",
		"refunds": map[string]any{
			"data": []any{
				map[string]any{"id": "re_first"},
				map[string]any{"id": "re_latest"},
			},
		},
	})

	if err := credits.ApplyRefundReversal(ctx, repo, evt, nil); err != nil {
		t.Fatalf("ApplyRefundReversal: %v", err)
	}
	verifyRevoked(t, repo, "user", "credits", 0)
}

func TestApplyRefundReversal_NoMatchIsNoOp(t *testing.T) {
	repo := credits.NewMemoryRepo()
	ctx := t.Context()
	_, _ = repo.Grant(ctx, credits.GrantInput{
		SubjectID: "user", Bucket: "credits", Amount: 100,
		Source: credits.SourceStripePayment, SourceRef: "pi_kept",
	})
	evt := refundEvent(t, "refund.created", map[string]any{
		"id":             "re_x",
		"object":         "refund",
		"payment_intent": "pi_nobody_has_this",
	})
	if err := credits.ApplyRefundReversal(ctx, repo, evt, nil); err != nil {
		t.Fatalf("ApplyRefundReversal: %v", err)
	}
	verifyRevoked(t, repo, "user", "credits", 100) // untouched
}

func verifyRevoked(t *testing.T, repo credits.CreditRepo, subj credits.SubjectID, bucket string, wantTotal int64) {
	t.Helper()
	bal, err := repo.Balance(t.Context(), subj, bucket)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Total != wantTotal {
		t.Errorf("subj=%s bucket=%s: balance=%d, want %d", subj, bucket, bal.Total, wantTotal)
	}
}
