package metering_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bds421/rho-stripe/metering"
	"github.com/bds421/rho-stripe/subject"
)

type fakeBackend struct {
	pushed []metering.PushEvent
	err    error
}

func (b *fakeBackend) PushBatch(_ context.Context, events []metering.PushEvent) error {
	if b.err != nil {
		return b.err
	}
	b.pushed = append(b.pushed, events...)
	return nil
}

// staticCustomerRepo is a test-only subject.CustomerRepo backed by a
// map. Replaced the old metering.CustomerResolver typed-closure with
// the standard subject.CustomerRepo interface (slice-57 audit Cross#10).
type staticCustomerRepo map[subject.ID]subject.StripeCustomerID

func (r staticCustomerRepo) Get(_ context.Context, s subject.ID) (subject.StripeCustomerID, bool, error) {
	id, ok := r[s]
	return id, ok, nil
}

func (r staticCustomerRepo) Upsert(_ context.Context, s subject.ID, id subject.StripeCustomerID) error {
	r[s] = id
	return nil
}

func staticResolver(m map[metering.SubjectID]string) subject.CustomerRepo {
	repo := staticCustomerRepo{}
	for k, v := range m {
		repo[k] = subject.StripeCustomerID(v)
	}
	return repo
}

func TestReconcileToStripe_AggregatesAndPushes(t *testing.T) {
	repo := metering.NewMemoryRepo()
	now := time.Now()
	for i := 0; i < 3; i++ {
		_ = repo.RecordUsage(t.Context(), metering.MeterEvent{
			SubjectID: "org_a", Metric: "api_calls", Quantity: 10, OccurredAt: now, RequestID: "req_a_" + string(rune('a'+i)),
		})
	}
	_ = repo.RecordUsage(t.Context(), metering.MeterEvent{
		SubjectID: "org_b", Metric: "api_calls", Quantity: 5, OccurredAt: now, RequestID: "req_b",
	})

	be := &fakeBackend{}
	resolver := staticResolver(map[metering.SubjectID]string{"org_a": "cus_a", "org_b": "cus_b"})

	stats, err := metering.ReconcileToStripe(t.Context(), repo, be, resolver, "demo", "api_calls",
		metering.Period{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}, nil)
	if err != nil {
		t.Fatalf("ReconcileToStripe: %v", err)
	}
	if stats.Aggregates != 2 || stats.Pushed != 2 {
		t.Errorf("stats wrong: %+v", stats)
	}
	if len(be.pushed) != 2 {
		t.Fatalf("expected 2 pushes, got %d", len(be.pushed))
	}
	totals := map[string]int64{}
	for _, e := range be.pushed {
		totals[e.StripeCustomer] = e.Value
		if e.EventName != "demo.api_calls" {
			t.Errorf("EventName wrong: %q", e.EventName)
		}
		if e.Identifier == "" {
			t.Error("Identifier should be set for Stripe dedup")
		}
	}
	if totals["cus_a"] != 30 || totals["cus_b"] != 5 {
		t.Errorf("totals wrong: %v", totals)
	}
}

func TestReconcileToStripe_IdempotentIdentifier(t *testing.T) {
	repo := metering.NewMemoryRepo()
	now := time.Now()
	_ = repo.RecordUsage(t.Context(), metering.MeterEvent{SubjectID: "x", Metric: "m", Quantity: 1, OccurredAt: now, RequestID: "r"})

	be := &fakeBackend{}
	resolver := staticResolver(map[metering.SubjectID]string{"x": "cus_x"})
	period := metering.Period{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}

	_, _ = metering.ReconcileToStripe(t.Context(), repo, be, resolver, "demo", "m", period, nil)
	id1 := be.pushed[0].Identifier
	be.pushed = nil
	_, _ = metering.ReconcileToStripe(t.Context(), repo, be, resolver, "demo", "m", period, nil)
	id2 := be.pushed[0].Identifier
	if id1 != id2 {
		t.Errorf("Identifier not stable across runs: %q vs %q", id1, id2)
	}
}

func TestReconcileToStripe_SkipsSubjectWithoutCustomer(t *testing.T) {
	repo := metering.NewMemoryRepo()
	now := time.Now()
	_ = repo.RecordUsage(t.Context(), metering.MeterEvent{SubjectID: "no_cust", Metric: "m", Quantity: 1, OccurredAt: now, RequestID: "r"})
	be := &fakeBackend{}
	stats, _ := metering.ReconcileToStripe(t.Context(), repo, be, staticResolver(nil), "demo", "m",
		metering.Period{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}, nil)
	if stats.Aggregates != 1 || stats.Pushed != 0 {
		t.Errorf("expected 1 aggregate / 0 pushed; got %+v", stats)
	}
}

func TestReconcileToStripe_PropagatesBackendError(t *testing.T) {
	repo := metering.NewMemoryRepo()
	now := time.Now()
	_ = repo.RecordUsage(t.Context(), metering.MeterEvent{SubjectID: "x", Metric: "m", Quantity: 1, OccurredAt: now, RequestID: "r"})
	be := &fakeBackend{err: errors.New("stripe down")}
	_, err := metering.ReconcileToStripe(t.Context(), repo, be, staticResolver(map[metering.SubjectID]string{"x": "cus_x"}), "demo", "m",
		metering.Period{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}, nil)
	if err == nil {
		t.Error("expected error when backend fails")
	}
}

func TestRecordDirect(t *testing.T) {
	be := &fakeBackend{}
	resolver := staticResolver(map[metering.SubjectID]string{"x": "cus_x"})
	if err := metering.RecordDirect(t.Context(), be, resolver, "demo", "m", "x", 42, "req_direct"); err != nil {
		t.Fatalf("RecordDirect: %v", err)
	}
	if len(be.pushed) != 1 || be.pushed[0].Value != 42 || be.pushed[0].Identifier != "req_direct" {
		t.Errorf("RecordDirect shape wrong: %+v", be.pushed)
	}
}

func TestRecordDirect_RequiresCustomer(t *testing.T) {
	be := &fakeBackend{}
	err := metering.RecordDirect(t.Context(), be, staticResolver(nil), "demo", "m", "unknown", 1, "r")
	if err == nil {
		t.Error("expected error when subject has no Stripe customer")
	}
}
