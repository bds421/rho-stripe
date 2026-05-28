package metering_test

import (
	"testing"
	"time"

	"github.com/bds421/rho-stripe/metering"
)

func TestRecord_HappyPath(t *testing.T) {
	ops := metering.New(metering.NewMemoryRepo())
	err := ops.Record(t.Context(), metering.MeterEvent{
		SubjectID: "org", Metric: "api_calls", Quantity: 1, RequestID: "r1",
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
}

func TestRecord_RequiresFields(t *testing.T) {
	ops := metering.New(metering.NewMemoryRepo())
	for name, mut := range map[string]func(*metering.MeterEvent){
		"subject":    func(e *metering.MeterEvent) { e.SubjectID = "" },
		"metric":     func(e *metering.MeterEvent) { e.Metric = "" },
		"quantity":   func(e *metering.MeterEvent) { e.Quantity = 0 },
		"request_id": func(e *metering.MeterEvent) { e.RequestID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			evt := metering.MeterEvent{SubjectID: "x", Metric: "y", Quantity: 1, RequestID: "z"}
			mut(&evt)
			if err := ops.Record(t.Context(), evt); err == nil {
				t.Errorf("expected error when %s is missing/invalid", name)
			}
		})
	}
}

func TestRecord_Idempotent(t *testing.T) {
	ops := metering.New(metering.NewMemoryRepo())
	for i := 0; i < 3; i++ {
		_ = ops.Record(t.Context(), metering.MeterEvent{
			SubjectID: "org", Metric: "api_calls", Quantity: 5, RequestID: "same",
			OccurredAt: time.Now(),
		})
	}
	total, _ := ops.QueryByPeriod(t.Context(), "org", "api_calls", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if total != 5 {
		t.Errorf("idempotent record total = %d, want 5", total)
	}
}

func TestQueryByPeriod_FiltersOutsideWindow(t *testing.T) {
	ops := metering.New(metering.NewMemoryRepo())
	now := time.Now()
	_ = ops.Record(t.Context(), metering.MeterEvent{SubjectID: "org", Metric: "m", Quantity: 10, OccurredAt: now, RequestID: "in"})
	_ = ops.Record(t.Context(), metering.MeterEvent{SubjectID: "org", Metric: "m", Quantity: 100, OccurredAt: now.Add(-48 * time.Hour), RequestID: "out"})
	total, _ := ops.QueryByPeriod(t.Context(), "org", "m", now.Add(-time.Hour), now.Add(time.Hour))
	if total != 10 {
		t.Errorf("window total = %d, want 10 (older event excluded)", total)
	}
}

func TestAggregateByPeriod_GroupsPerSubject(t *testing.T) {
	repo := metering.NewMemoryRepo()
	ops := metering.New(repo)
	now := time.Now()
	_ = ops.Record(t.Context(), metering.MeterEvent{SubjectID: "a", Metric: "m", Quantity: 5, OccurredAt: now, RequestID: "1"})
	_ = ops.Record(t.Context(), metering.MeterEvent{SubjectID: "a", Metric: "m", Quantity: 7, OccurredAt: now, RequestID: "2"})
	_ = ops.Record(t.Context(), metering.MeterEvent{SubjectID: "b", Metric: "m", Quantity: 3, OccurredAt: now, RequestID: "3"})

	aggs, _ := repo.AggregateByPeriod(t.Context(), "m", metering.Period{Start: now.Add(-time.Hour), End: now.Add(time.Hour)})
	if len(aggs) != 2 {
		t.Fatalf("expected 2 subjects, got %d", len(aggs))
	}
	totals := map[metering.SubjectID]int64{}
	for _, a := range aggs {
		totals[a.SubjectID] = a.Total
	}
	if totals["a"] != 12 || totals["b"] != 3 {
		t.Errorf("aggregates wrong: %v", totals)
	}
}

func TestPruneOldEvents_DeletesOlder(t *testing.T) {
	ops := metering.New(metering.NewMemoryRepo())
	now := time.Now()
	_ = ops.Record(t.Context(), metering.MeterEvent{SubjectID: "x", Metric: "m", Quantity: 1, OccurredAt: now.Add(-72 * time.Hour), RequestID: "old"})
	_ = ops.Record(t.Context(), metering.MeterEvent{SubjectID: "x", Metric: "m", Quantity: 1, OccurredAt: now, RequestID: "new"})
	n, err := ops.PruneOldEvents(t.Context(), now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("pruned %d, want 1", n)
	}
	total, _ := ops.QueryByPeriod(t.Context(), "x", "m", now.Add(-100*time.Hour), now.Add(time.Hour))
	if total != 1 {
		t.Errorf("remaining total = %d, want 1", total)
	}
}
