package credits_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/bds421/rho-stripe/credits"
	"github.com/bds421/rho-stripe/webhooks"
	stripe "github.com/stripe/stripe-go/v82"
)

func TestEncodeDecodeSessionMetadata_Roundtrip(t *testing.T) {
	grants := []credits.PendingGrant{
		{Bucket: "ai", Amount: 1000, ValidDays: 90, ProductKey: "credit_pack_1000_ai", Quantity: 1},
		{Bucket: "api", Amount: 5000, ValidDays: 30, ProductKey: "api_pack", Quantity: 2},
	}
	enc, err := credits.EncodeSessionMetadata(grants)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if enc == "" {
		t.Fatal("Encode returned empty")
	}
	out, err := credits.DecodeSessionMetadata(enc)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != len(grants) {
		t.Fatalf("len = %d, want %d", len(out), len(grants))
	}
	for i, g := range out {
		if g != grants[i] {
			t.Errorf("[%d] = %+v, want %+v", i, g, grants[i])
		}
	}
}

func TestEncodeSessionMetadata_EmptyReturnsEmpty(t *testing.T) {
	v, err := credits.EncodeSessionMetadata(nil)
	if err != nil || v != "" {
		t.Errorf("Encode(nil) = (%q, %v), want ('', nil)", v, err)
	}
}

func TestDecodeSessionMetadata_EmptyReturnsNil(t *testing.T) {
	out, err := credits.DecodeSessionMetadata("")
	if err != nil || out != nil {
		t.Errorf("Decode('') = (%v, %v), want (nil, nil)", out, err)
	}
}

func TestEncodeSessionMetadata_RejectsOversized(t *testing.T) {
	var grants []credits.PendingGrant
	for i := 0; i < 50; i++ {
		grants = append(grants, credits.PendingGrant{
			Bucket: "very_long_bucket_name_for_padding", Amount: 1000,
			ProductKey: "product_with_padded_name_for_size", Quantity: 1,
		})
	}
	_, err := credits.EncodeSessionMetadata(grants)
	if err == nil {
		t.Error("expected oversize error, got nil")
	}
}

func TestMemoryRepo_Grant(t *testing.T) {
	repo := credits.NewMemoryRepo()
	g, err := repo.Grant(t.Context(), credits.GrantInput{
		SubjectID: "org_acme", Bucket: "ai", Amount: 1000, ValidDays: 90,
		Source: credits.SourceStripePayment, SourceRef: "pi_abc",
	})
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if g.AmountInitial != 1000 || g.AmountRemaining != 1000 {
		t.Errorf("amount fields wrong: %+v", g)
	}
	if g.ExpiresAt == nil {
		t.Error("ExpiresAt should be set when ValidDays > 0")
	}

	list, _ := repo.ListBySubject(t.Context(), "org_acme")
	if len(list) != 1 {
		t.Errorf("ListBySubject len = %d, want 1", len(list))
	}
}

func TestMemoryRepo_GrantIdempotentOnSourceRef(t *testing.T) {
	repo := credits.NewMemoryRepo()
	in := credits.GrantInput{
		SubjectID: "org_acme", Bucket: "ai", Amount: 1000,
		Source: credits.SourceStripePayment, SourceRef: "pi_xyz",
	}
	g1, err := repo.Grant(t.Context(), in)
	if err != nil {
		t.Fatalf("first Grant: %v", err)
	}
	g2, err := repo.Grant(t.Context(), in)
	if err != nil {
		t.Fatalf("second Grant: %v", err)
	}
	if g1.ID != g2.ID {
		t.Errorf("re-grant created a new id %q (want same as %q) — dedup failed", g2.ID, g1.ID)
	}
	list, _ := repo.ListBySubject(t.Context(), "org_acme")
	if len(list) != 1 {
		t.Errorf("expected 1 grant after dedup, got %d", len(list))
	}
}

func TestMemoryRepo_GrantNoExpiry(t *testing.T) {
	repo := credits.NewMemoryRepo()
	g, _ := repo.Grant(t.Context(), credits.GrantInput{
		SubjectID: "x", Bucket: "ai", Amount: 100, ValidDays: 0,
	})
	if g.ExpiresAt != nil {
		t.Errorf("ExpiresAt = %v, want nil when ValidDays=0", g.ExpiresAt)
	}
}

// --- Auto-handler tests ---

func sessionEvent(sessionID, paymentIntent, subjectID, grantsMetadata string) webhooks.Event {
	body := fmt.Sprintf(
		`{"id":%q,"object":"checkout.session","payment_intent":%q,"metadata":{"subject_id":%q,"app_namespace":"demo","credit_grants":%s}}`,
		sessionID, paymentIntent, subjectID, jsonStringLiteral(grantsMetadata),
	)
	return webhooks.Event{
		ID:   "evt_x",
		Type: "checkout.session.completed",
		Raw: &stripe.Event{
			Data: &stripe.EventData{Raw: []byte(body)},
		},
	}
}

func jsonStringLiteral(s string) string {
	// Embedding a JSON value inside another JSON: quote and escape.
	b := []byte{'"'}
	for _, c := range []byte(s) {
		switch c {
		case '"', '\\':
			b = append(b, '\\', c)
		default:
			b = append(b, c)
		}
	}
	b = append(b, '"')
	return string(b)
}

func TestApplyGrantsFromSession_HappyPath(t *testing.T) {
	repo := credits.NewMemoryRepo()
	grants := []credits.PendingGrant{
		{Bucket: "ai", Amount: 1000, ValidDays: 90, ProductKey: "credit_pack_1000_ai", Quantity: 1},
	}
	enc, _ := credits.EncodeSessionMetadata(grants)
	evt := sessionEvent("cs_1", "pi_1", "org_acme", enc)

	if err := credits.ApplyGrantsFromSession(t.Context(), repo, evt, nil); err != nil {
		t.Fatalf("ApplyGrantsFromSession: %v", err)
	}

	list, _ := repo.ListBySubject(t.Context(), "org_acme")
	if len(list) != 1 {
		t.Fatalf("ListBySubject len = %d, want 1", len(list))
	}
	if list[0].Bucket != "ai" || list[0].AmountInitial != 1000 {
		t.Errorf("granted incorrectly: %+v", list[0])
	}
}

func TestApplyGrantsFromSession_QuantityRespected(t *testing.T) {
	repo := credits.NewMemoryRepo()
	grants := []credits.PendingGrant{
		{Bucket: "ai", Amount: 500, ValidDays: 30, ProductKey: "pack", Quantity: 3},
	}
	enc, _ := credits.EncodeSessionMetadata(grants)
	evt := sessionEvent("cs_q", "pi_q", "org_q", enc)

	if err := credits.ApplyGrantsFromSession(t.Context(), repo, evt, nil); err != nil {
		t.Fatalf("ApplyGrantsFromSession: %v", err)
	}
	list, _ := repo.ListBySubject(t.Context(), "org_q")
	if len(list) != 3 {
		t.Errorf("expected 3 grants for Quantity=3, got %d", len(list))
	}
}

func TestApplyGrantsFromSession_IdempotentOnReplay(t *testing.T) {
	repo := credits.NewMemoryRepo()
	grants := []credits.PendingGrant{
		{Bucket: "ai", Amount: 1000, ValidDays: 90, ProductKey: "pack", Quantity: 1},
	}
	enc, _ := credits.EncodeSessionMetadata(grants)
	evt := sessionEvent("cs_replay", "pi_replay", "org_r", enc)

	for i := 0; i < 3; i++ {
		if err := credits.ApplyGrantsFromSession(t.Context(), repo, evt, nil); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	list, _ := repo.ListBySubject(t.Context(), "org_r")
	if len(list) != 1 {
		t.Errorf("expected 1 grant after 3 replays, got %d", len(list))
	}
}

func TestApplyGrantsFromSession_NoCreditMetadataIsNoop(t *testing.T) {
	repo := credits.NewMemoryRepo()
	body := `{"id":"cs_x","object":"checkout.session","payment_intent":"pi_x","metadata":{"subject_id":"org_y","app_namespace":"demo"}}`
	evt := webhooks.Event{
		ID: "evt_n", Type: "checkout.session.completed",
		Raw: &stripe.Event{Data: &stripe.EventData{Raw: []byte(body)}},
	}
	if err := credits.ApplyGrantsFromSession(t.Context(), repo, evt, nil); err != nil {
		t.Errorf("expected no-op, got error: %v", err)
	}
	list, _ := repo.ListBySubject(t.Context(), "org_y")
	if len(list) != 0 {
		t.Errorf("expected no grants when metadata absent, got %d", len(list))
	}
}

func TestApplyGrantsFromSession_NilRepoIsNoop(t *testing.T) {
	enc, _ := credits.EncodeSessionMetadata([]credits.PendingGrant{
		{Bucket: "ai", Amount: 100, Quantity: 1},
	})
	evt := sessionEvent("cs_n", "pi_n", "x", enc)
	if err := credits.ApplyGrantsFromSession(t.Context(), nil, evt, nil); err != nil {
		t.Errorf("expected nil-repo to be no-op, got: %v", err)
	}
}

func TestApplyGrantsFromSession_WrongEventTypeIsNoop(t *testing.T) {
	repo := credits.NewMemoryRepo()
	evt := webhooks.Event{
		ID: "evt", Type: "invoice.paid",
		Raw: &stripe.Event{Data: &stripe.EventData{Raw: []byte(`{}`)}},
	}
	if err := credits.ApplyGrantsFromSession(t.Context(), repo, evt, nil); err != nil {
		t.Errorf("expected non-session event to be no-op, got: %v", err)
	}
	list, _ := repo.ListBySubject(_ctx(t), "anything")
	if len(list) != 0 {
		t.Error("expected no grants when event type isn't session.completed")
	}
}

func TestApplyGrantsFromSession_MissingSubjectErrors(t *testing.T) {
	repo := credits.NewMemoryRepo()
	enc, _ := credits.EncodeSessionMetadata([]credits.PendingGrant{
		{Bucket: "ai", Amount: 1000, Quantity: 1},
	})
	body := fmt.Sprintf(
		`{"id":"cs_z","object":"checkout.session","payment_intent":"pi_z","metadata":{"credit_grants":%s}}`,
		jsonStringLiteral(enc),
	)
	evt := webhooks.Event{
		ID: "evt_z", Type: "checkout.session.completed",
		Raw: &stripe.Event{Data: &stripe.EventData{Raw: []byte(body)}},
	}
	err := credits.ApplyGrantsFromSession(t.Context(), repo, evt, nil)
	if err == nil {
		t.Error("expected error when subject_id missing, got nil")
	}
}

func _ctx(t *testing.T) context.Context { return t.Context() }
