package catalog

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// DriftReport summarizes the drift state for one check run.
type DriftReport struct {
	Namespace string
	CheckedAt time.Time
	Items     []PlanItem // items whose Op == OpCreate / OpUpdate / OpReplace / OpArchive / OpDrift / OpUnarchive
	HasDrift  bool
}

// Apply runs the drift remediation against Stripe by feeding the
// report's items through Apply. The report is a frozen snapshot —
// applying it later doesn't re-diff against current Stripe state, so
// concurrent dashboard edits could be silently overwritten. Apps
// usually want to re-check first and only call Apply against a
// freshly-generated report:
//
//	rep, err := catalog.CheckDrift(ctx, backend, spec)
//	if err != nil { ... }
//	if rep.HasDrift {
//	    if err := rep.Apply(ctx, backend); err != nil { ... }
//	}
//
// Returns nil + no-op when HasDrift is false.
func (r DriftReport) Apply(ctx context.Context, backend Backend) error {
	if !r.HasDrift {
		return nil
	}
	return Apply(ctx, backend, Plan{Items: r.Items})
}

// FormatHuman returns a multi-line, human-readable summary of the
// report suitable for piping to alerting (Slack, PagerDuty, email).
// Empty string when no drift.
func (r DriftReport) FormatHuman() string {
	if !r.HasDrift {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Catalog drift detected in namespace %q at %s\n", r.Namespace, r.CheckedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "%d item(s) need attention:\n", len(r.Items))
	for _, it := range r.Items {
		fmt.Fprintf(&b, "  - [%s] %s %s\n", it.Op, it.Kind, planItemLabel(it))
	}
	return b.String()
}

// planItemLabel renders a short identifier for the item suitable for
// the FormatHuman summary.
func planItemLabel(it PlanItem) string {
	if it.Key != "" {
		return it.Key
	}
	switch it.Kind {
	case KindProduct:
		if it.NewProduct != nil && it.NewProduct.Name != "" {
			return it.NewProduct.Name
		}
		if it.ExistingProduct != nil {
			if it.ExistingProduct.Name != "" {
				return it.ExistingProduct.Name
			}
			return it.ExistingProduct.ID
		}
	case KindPrice:
		if it.NewPrice != nil && it.NewPrice.LookupKey != "" {
			return it.NewPrice.LookupKey
		}
		if it.ExistingPrice != nil {
			return it.ExistingPrice.LookupKey
		}
	case KindMeter:
		if it.NewMeter != nil && it.NewMeter.DisplayName != "" {
			return it.NewMeter.DisplayName
		}
		if it.ExistingMeter != nil {
			return it.ExistingMeter.ID
		}
	}
	return string(it.Kind)
}

// CheckDrift runs the sync diff in read-only mode and returns a
// report. The check fetches the current state from Stripe via the
// backend, diffs against spec, and reports any items that would be
// changed if sync ran now.
//
// Apps schedule this (e.g. every 15 minutes) to alert on dashboard
// edits or out-of-band changes between deploys. The sync-time diff
// (slice 5) catches the dominant case (drift at deploy time); this
// catches drift between deploys.
func CheckDrift(ctx context.Context, backend Backend, spec *Spec) (DriftReport, error) {
	products, err := backend.ListProductsByNamespace(ctx, spec.Namespace)
	if err != nil {
		return DriftReport{}, fmt.Errorf("catalog.CheckDrift: list products: %w", err)
	}
	meters, err := backend.ListMetersByNamespace(ctx, spec.Namespace)
	if err != nil {
		return DriftReport{}, fmt.Errorf("catalog.CheckDrift: list meters: %w", err)
	}
	plan := DiffWithMeters(spec, products, meters)
	report := DriftReport{
		Namespace: spec.Namespace,
		CheckedAt: nowFunc(),
		Items:     plan.Items,
		HasDrift:  !plan.Empty(),
	}
	return report, nil
}

// nowFunc is the time-of-check source. Replaced in tests via
// SetNowFuncForTest; production code uses the real wall clock.
//
// We use a package-level var rather than threading a Clock through
// CheckDrift's signature because:
//   - CheckDrift is a stateless top-level function; adding a
//     parameter is a breaking change to every caller.
//   - The drift report's CheckedAt is purely informational (no
//     business logic branches on it), so deterministic-time tests
//     only matter for the drift detector's RunDriftDetector loop,
//     not the per-call CheckDrift.
var nowFunc = func() time.Time { return time.Now().UTC() }

// SetNowFuncForTest replaces the time source. Returns a restore
// function suitable for t.Cleanup. Test-only seam — production code
// must not call this.
func SetNowFuncForTest(fn func() time.Time) (restore func()) {
	prev := nowFunc
	nowFunc = fn
	return func() { nowFunc = prev }
}

// RunDriftDetector starts a background goroutine that runs CheckDrift
// at the given interval and invokes onReport for each result.
// Returns a stop function that cancels the goroutine when called.
//
// Apps wire this once at startup:
//
//	stop := catalog.RunDriftDetector(ctx, backend, spec, 15*time.Minute, func(r catalog.DriftReport) {
//	    if r.HasDrift {
//	        slack.Notify("rho-stripe drift", r)
//	    }
//	})
//	defer stop()
func RunDriftDetector(
	ctx context.Context,
	backend Backend,
	spec *Spec,
	interval time.Duration,
	onReport func(DriftReport),
	logger *slog.Logger,
) (stop func()) {
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	if logger == nil {
		logger = slog.Default()
	}
	stopCh := make(chan struct{})
	var stopOnce sync.Once
	done := make(chan struct{}) // closed when goroutine exits
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				rep, err := CheckDrift(ctx, backend, spec)
				if err != nil {
					logger.WarnContext(ctx, "catalog drift check failed", "err", err)
					continue
				}
				if onReport != nil {
					onReport(rep)
				}
			}
		}
	}()
	// Returned stop():
	//   - Safe to call multiple times (sync.Once around the channel close).
	//   - Blocks until the goroutine has fully exited (no orphan in-flight
	//     CheckDrift / onReport callbacks after stop() returns).
	return func() {
		stopOnce.Do(func() { close(stopCh) })
		<-done
	}
}
