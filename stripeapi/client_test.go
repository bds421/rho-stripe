//go:build integration

package stripeapi_test

import (
	"os"
	"strings"
	"testing"

	"github.com/bds421/rho-stripe/stripeapi"
)

// TestRealAccountCall exercises the full stack against the live test-mode
// Stripe API: rho-kit's resilient HTTP client → stripe-go → Stripe.
// Run with:  set -a; source .env; set +a; go test -tags=integration ./stripeapi/...
func TestRealAccountCall(t *testing.T) {
	key := os.Getenv("STRIPE_SECRET_KEY")
	if key == "" {
		t.Skip("STRIPE_SECRET_KEY not set; skipping integration test")
	}
	if !strings.HasPrefix(key, "sk_test_") {
		t.Fatalf("refusing to run integration test with non-test key (prefix %q)", key[:8])
	}

	sc := stripeapi.NewClient(stripeapi.Config{SecretKey: key})

	acct, err := sc.Accounts.Get()
	if err != nil {
		t.Fatalf("Accounts.Get: %v", err)
	}
	if acct.ID == "" {
		t.Fatal("expected non-empty account ID")
	}
	t.Logf("account: id=%s country=%s default_currency=%s",
		acct.ID, acct.Country, acct.DefaultCurrency)
}
