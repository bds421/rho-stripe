package stripeapi

import (
	"context"
	"testing"

	stripe "github.com/stripe/stripe-go/v82"
)

// BenchmarkApplyIdem measures the hot path of every outbound write.
// At ~1µs we're an order of magnitude below the Stripe round-trip
// (~50ms), so idempotency-key derivation isn't worth optimizing
// further — but if this regresses we want to catch it.
func BenchmarkApplyIdem(b *testing.B) {
	ctx := context.Background()
	meta := map[string]string{"app_namespace": "bench", "subject": "subj-1"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := &stripe.CustomerParams{}
		applyIdem(p, ctx, "customer.create", canonicalMap(meta), "extra-field")
	}
}

// BenchmarkDeriveKey measures pure hash construction (no Stripe Params
// allocation). Establishes the floor.
func BenchmarkDeriveKey(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = deriveKey("op.checkout.session.create",
			"cus_X", "subscription", "https://example.com/s", "https://example.com/c",
			"app_namespace=ns\x00subject=subj-1", "price_X:1")
	}
}

// BenchmarkCanonicalMap measures the metadata canonicalization that
// every multi-key derive call performs.
func BenchmarkCanonicalMap(b *testing.B) {
	m := map[string]string{
		"app_namespace": "myapp",
		"subject":       "subj-12345",
		"created_by":    "checkout_handler",
		"run_id":        "run_1234567890",
		"trace_id":      "abcdef0123456789",
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = canonicalMap(m)
	}
}
