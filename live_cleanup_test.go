//go:build live_stripe

package live_stripe_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bds421/rho-stripe/stripeapi"
	stripe "github.com/stripe/stripe-go/v82"
)

// CleanupLiveTestArtifacts purges every Customer / SetupIntent /
// TaxID / Subscription that the live tests have created. Stripe test
// accounts otherwise accumulate thousands of artifacts across CI runs.
//
// Strategy: tests stamp every resource with metadata "live_test_run":
// "true" (and ideally "live_test_namespace": "<test ns>") at
// creation time. This cleanup walks each resource type, filtering by
// that metadata, and deletes / cancels.
//
// Run BEFORE the test suite:
//
//	go test -tags=live_stripe -run TestLive_CleanupAll -timeout=300s .
//
// or invoke from CI as a pre-step.
//
// Safe to run repeatedly. Idempotent.
func TestLive_CleanupAll(t *testing.T) {
	if !cleanupRequested(t) {
		t.Skip("skipping live cleanup (set LIVE_CLEANUP=true to run)")
	}
	key := liveKey(t)
	sc := stripeapi.NewClient(stripeapi.Config{SecretKey: key})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	r := &cleanupReport{}

	// Customers stamped with live_test_run=true. Deleting a customer
	// cascades subscription cancellation + payment-method detach.
	cleanCustomers(ctx, t, sc, r)

	// SetupIntents linger as standalone objects when no PaymentMethod
	// confirmed them. Cancel them.
	cleanSetupIntents(ctx, t, sc, r)

	t.Logf("cleanup summary: customers_deleted=%d setup_intents_canceled=%d errors=%d",
		r.customersDeleted, r.setupIntentsCanceled, r.errors)
}

type cleanupReport struct {
	customersDeleted     int
	setupIntentsCanceled int
	errors               int
}

func cleanupRequested(t *testing.T) bool {
	t.Helper()
	// Check env explicitly; we never want a CI accident to mass-delete
	// without operator intent.
	for _, e := range []string{"LIVE_CLEANUP"} {
		if v := getenv(e); v == "true" || v == "1" {
			return true
		}
	}
	return false
}

func getenv(key string) string { return os.Getenv(key) }

func cleanCustomers(ctx context.Context, t *testing.T, sc *stripe.Client, r *cleanupReport) {
	params := &stripe.CustomerSearchParams{
		SearchParams: stripe.SearchParams{
			Query: `metadata["live_test_run"]:"true"`,
		},
	}
	for c, err := range sc.V1Customers.Search(ctx, params) {
		if err != nil {
			r.errors++
			t.Logf("cleanup: Customers.Search: %v", err)
			return
		}
		if !shouldClean(c.Metadata) {
			continue
		}
		if _, err := sc.V1Customers.Delete(ctx, c.ID, nil); err != nil {
			r.errors++
			t.Logf("cleanup: Customers.Delete(%s): %v", c.ID, err)
			continue
		}
		r.customersDeleted++
	}
}

func cleanSetupIntents(ctx context.Context, t *testing.T, sc *stripe.Client, r *cleanupReport) {
	// SetupIntents can't be searched by metadata directly; we list
	// recent ones and filter client-side. Bounded to last 1000 to keep
	// the run fast.
	params := &stripe.SetupIntentListParams{}
	params.Filters.AddFilter("limit", "", "100")
	scanned := 0
	for si, err := range sc.V1SetupIntents.List(ctx, params) {
		if err != nil {
			r.errors++
			t.Logf("cleanup: SetupIntents.List: %v", err)
			return
		}
		scanned++
		if scanned > 1000 {
			break
		}
		if !shouldClean(si.Metadata) {
			continue
		}
		if si.Status == "succeeded" || si.Status == "canceled" {
			continue
		}
		if _, err := sc.V1SetupIntents.Cancel(ctx, si.ID, nil); err != nil {
			r.errors++
			t.Logf("cleanup: SetupIntents.Cancel(%s): %v", si.ID, err)
			continue
		}
		r.setupIntentsCanceled++
	}
}

// shouldClean returns true when the resource carries our live-test
// stamp. Conservative: any of these markers is enough.
func shouldClean(meta map[string]string) bool {
	if meta == nil {
		return false
	}
	if meta["live_test_run"] == "true" {
		return true
	}
	// Legacy patterns from earlier live tests.
	ns := meta["app_namespace"]
	return strings.HasPrefix(ns, "live") || strings.HasPrefix(ns, "livet_") || strings.HasPrefix(ns, "livegdpr") || strings.HasPrefix(ns, "livesi") || strings.HasPrefix(ns, "livetax")
}
