package stripeapi

import (
	"context"
	"strings"
	"testing"

	stripe "github.com/stripe/stripe-go/v82"
)

func TestApplyIdem_UsesCallerKeyWhenProvided(t *testing.T) {
	p := &stripe.CustomerParams{}
	ctx := WithIdempotencyKey(context.Background(), "explicit-key")
	applyIdem(p, ctx, "customer.create", "some-input")
	if got := p.IdempotencyKey; got == nil || *got != "explicit-key" {
		t.Fatalf("expected caller-supplied key to win; got %v", got)
	}
}

func TestApplyIdem_DerivesKeyWhenNoCallerOverride(t *testing.T) {
	p := &stripe.CustomerParams{}
	applyIdem(p, context.Background(), "customer.create", "subj-1")
	if got := p.IdempotencyKey; got == nil || !strings.HasPrefix(*got, "customer.create:") {
		t.Fatalf("expected op-prefixed derived key; got %v", got)
	}
}

func TestDeriveKey_StableForSameInputs(t *testing.T) {
	a := deriveKey("op.foo", "x", "y", "z")
	b := deriveKey("op.foo", "x", "y", "z")
	if a != b {
		t.Fatalf("derived key not stable: %s vs %s", a, b)
	}
}

func TestDeriveKey_DifferentForDifferentInputs(t *testing.T) {
	a := deriveKey("op.foo", "x", "y")
	b := deriveKey("op.foo", "x", "z")
	if a == b {
		t.Fatalf("derived key collided across inputs: %s", a)
	}
}

func TestDeriveKey_SeparatorPreventsConcatCollision(t *testing.T) {
	// Without a separator "ab" + "cd" and "abc" + "d" would both hash
	// the same bytes. Length-prefixing must prevent this.
	a := deriveKey("op", "ab", "cd")
	b := deriveKey("op", "abc", "d")
	if a == b {
		t.Fatalf("missing separator: %s collides", a)
	}
}

func TestDeriveKey_NullByteInInputDoesNotCollide(t *testing.T) {
	// Before length-prefixing, the separator \0 collided with \0
	// appearing inside an input. Verify the class is closed.
	a := deriveKey("op", "a\x00b")
	b := deriveKey("op", "a", "b")
	if a == b {
		t.Fatalf("\\0-injection collision: %s vs %s", a, b)
	}
}

func TestDeriveKey_EmptyStringDistinctFromMissing(t *testing.T) {
	// Length-prefixed encoding: empty string is a distinct input
	// from no-input-at-all. (Naïve no-separator schemes would treat
	// these identically.)
	a := deriveKey("op")
	b := deriveKey("op", "")
	if a == b {
		t.Fatalf("empty vs missing collide: both %s", a)
	}
}

func TestCanonicalMap_DeterministicAcrossRuns(t *testing.T) {
	// Maps iterate non-deterministically in Go; canonicalMap must sort.
	m := map[string]string{"b": "2", "a": "1", "c": "3"}
	want := "a=1\x00b=2\x00c=3"
	for i := 0; i < 50; i++ {
		if got := canonicalMap(m); got != want {
			t.Fatalf("non-deterministic on iter %d: %q", i, got)
		}
	}
}

func TestCanonicalMap_EmptyReturnsEmpty(t *testing.T) {
	if got := canonicalMap(nil); got != "" {
		t.Fatalf("nil map should canonicalize to empty; got %q", got)
	}
	if got := canonicalMap(map[string]string{}); got != "" {
		t.Fatalf("empty map should canonicalize to empty; got %q", got)
	}
}

func TestCallerKey_NilContextSafe(t *testing.T) {
	// Defensive: nil context shouldn't panic. We intentionally pass
	// nil (vs context.TODO) to exercise the nil-branch guard.
	//lint:ignore SA1012 nil-ctx test
	if k := callerKey(nil); k != "" {
		t.Fatalf("expected empty key from nil ctx; got %q", k)
	}
}

func TestWithIdempotencyKey_NilContextSafe(t *testing.T) {
	// Same: intentionally testing the nil-ctx defensive branch.
	//lint:ignore SA1012 nil-ctx test
	ctx := WithIdempotencyKey(nil, "key")
	if got := callerKey(ctx); got != "key" {
		t.Fatalf("expected key 'key'; got %q", got)
	}
}

func TestWithIdempotencyKey_EmptyStringIsValidOverride(t *testing.T) {
	// Setting "" stores empty, which falls through to derived key.
	// This is the documented way to "clear" an override.
	ctx := WithIdempotencyKey(context.Background(), "")
	p := &stripe.CustomerParams{}
	applyIdem(p, ctx, "customer.create")
	if p.IdempotencyKey == nil || !strings.HasPrefix(*p.IdempotencyKey, "customer.create:") {
		t.Fatalf("empty override should fall through to derived; got %v", p.IdempotencyKey)
	}
}

func TestApplyIdem_OnSubscriptionParams(t *testing.T) {
	// Sanity that other XxxParams types also satisfy idemSettable.
	p := &stripe.SubscriptionParams{}
	applyIdem(p, context.Background(), "subscription.update", "sub_X")
	if p.IdempotencyKey == nil {
		t.Fatalf("idem-key not applied to SubscriptionParams")
	}
}
