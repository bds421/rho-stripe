// Package stripeapi: idempotency-key support for outbound Stripe writes.
//
// Stripe's Idempotency-Key header guarantees that a write with the same
// key within ~24h returns the exact same response — even if the prior
// call succeeded but the response was lost. This protects against:
//
//   - In-flight HTTP retries (network blip mid-call)
//   - Process restarts mid-write (crash after Stripe accepted but
//     before app DB recorded)
//   - Sync re-runs (drift-check or backfill replays an operation)
//
// Strategy: every write call derives a *deterministic* key from the
// operation name plus the canonical inputs that identify intent.
// Callers may override with stripeapi.WithIdempotencyKey when they
// need explicit control (e.g., to *force* a fresh attempt by changing
// the key, or to share a key across logically-equivalent calls).
package stripeapi

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"sort"
	"strconv"
	"strings"
)

type idemKeyCtxType struct{}

// WithIdempotencyKey returns a derived context that overrides the
// auto-derived Idempotency-Key for the *next* outbound Stripe write.
//
// Use for explicit caller-controlled dedup scope — e.g., when retrying
// a known-failed external workflow and you want Stripe to treat the
// retry as the same logical operation.
//
// Pass empty string to *clear* a previously-set override (the next
// write will fall through to the auto-derived key). This is the only
// documented way to undo a WithIdempotencyKey set further up the
// call stack.
//
// Implementation note: empty-string semantically means "no override",
// which is exactly what we want for the clear-pattern. If you find
// yourself accidentally setting an empty key from a user-input value,
// fix the input validation at the call site — don't expect this
// helper to reject empty (it's a feature, not a bug).
func WithIdempotencyKey(ctx context.Context, key string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, idemKeyCtxType{}, key)
}

// callerKey returns the caller-supplied override, or "" if none set.
func callerKey(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(idemKeyCtxType{}).(string)
	return v
}

// IdempotencyKeySetter is satisfied by every *stripe.XxxParams via the
// embedded *stripe.Params.SetIdempotencyKey method. Exported so other
// rho-stripe packages can call ApplyIdempotencyKey on their own param
// types without re-implementing the helper.
type IdempotencyKeySetter interface {
	SetIdempotencyKey(string)
}

// ApplyIdempotencyKey sets a Stripe Idempotency-Key on the params:
// honors a caller-supplied WithIdempotencyKey(ctx, key) override if
// present, otherwise derives a deterministic key from op + inputs.
//
// inputs should canonically identify the operation's intent — e.g.,
// for CreateCustomer pass subject + sorted metadata; for CreateInvoice
// pass customer + line item hash. The derivation is stable across
// processes and call sites — same intent → same key → safe dedup.
//
// Use the CanonicalMap / CanonicalInt64 / CanonicalStrings helpers to
// stabilize inputs derived from maps or numeric values.
func ApplyIdempotencyKey(p IdempotencyKeySetter, ctx context.Context, op string, inputs ...string) {
	if k := callerKey(ctx); k != "" {
		p.SetIdempotencyKey(k)
		return
	}
	p.SetIdempotencyKey(deriveKey(op, inputs...))
}

// applyIdem is the unexported alias retained for the dense stripeapi
// internal call sites. New code (especially in sub-packages) should
// reach for ApplyIdempotencyKey directly.
func applyIdem(p IdempotencyKeySetter, ctx context.Context, op string, inputs ...string) {
	ApplyIdempotencyKey(p, ctx, op, inputs...)
}

// deriveKey hashes op + inputs with sha256, returning op-prefixed hex
// so server-side logs remain debuggable ("checkout.session.create:abcd").
//
// Truncating to 16 bytes (32 hex chars) yields 128 bits — collision
// probability vanishingly small for the call volumes a single app
// makes inside Stripe's 24h dedup window.
//
// Each input is length-prefixed (8 bytes, big-endian) before being
// hashed. A naive `\0` separator would collide when an input itself
// contains `\0` — e.g., deriveKey("op", "a\x00b") and
// deriveKey("op", "a", "b") would hash identical bytes. Length
// prefixes make the encoding unambiguous regardless of input
// content, eliminating the collision class entirely.
func deriveKey(op string, inputs ...string) string {
	h := sha256.New()
	writeLenPrefixed(h, op)
	for _, s := range inputs {
		writeLenPrefixed(h, s)
	}
	return op + ":" + hex.EncodeToString(h.Sum(nil)[:16])
}

// writeLenPrefixed writes a uint64-length-prefixed string. Using
// uint64 (not uint32) lets us never hit a "string > 4GiB rejected"
// branch and keeps the encoding total-function.
//
// hash.Hash.Write is documented to never return an error (per
// hash.Hash contract: "It never returns an error"), so the
// underscore-ignored returns here are safe rather than sloppy.
func writeLenPrefixed(h hash.Hash, s string) {
	var lenBuf [8]byte
	binary.BigEndian.PutUint64(lenBuf[:], uint64(len(s)))
	_, _ = h.Write(lenBuf[:]) // safe: hash.Hash.Write never errors per contract
	_, _ = h.Write([]byte(s))
}

// CanonicalMap renders a map[string]string into a deterministic
// "k1=v1\x00k2=v2\x00..." string with keys sorted lexically. Use as
// an input to ApplyIdempotencyKey to stabilize derivation across map
// iteration orders.
func CanonicalMap(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(0)
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(m[k])
	}
	return b.String()
}

// CanonicalStrings joins a slice deterministically (caller must
// pre-sort or pass already-canonical order). Use as an input to
// ApplyIdempotencyKey for slice-valued fields.
func CanonicalStrings(xs []string) string {
	return strings.Join(xs, "\x00")
}

// CanonicalInt64 stringifies an int64 deterministically. Use for
// amounts, IDs, timestamps, or any integer that feeds into an
// idempotency-key derivation.
func CanonicalInt64(n int64) string { return strconv.FormatInt(n, 10) }

// ContentHash returns a 32-hex-char (128-bit) sha256 digest of the
// named (key, value) pairs. Use when a payload has many optional fields
// and you want a single string suitable for ApplyIdempotencyKey: two
// payloads with any differing field get distinct hashes; reordering
// the map has no effect (we sort keys lexically before hashing).
//
// Each pair is length-prefixed before hashing — see writeLenPrefixed
// for why the naive `\0` separator was rejected (input-collision class).
//
// Caller passes the operation name in `op` so the hash domain is
// scoped per operation; this prevents accidental cross-op collisions
// even if two ops happen to hash the same field set.
func ContentHash(op string, fields map[string]string) string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	writeLenPrefixed(h, op)
	for _, k := range keys {
		writeLenPrefixed(h, k)
		writeLenPrefixed(h, fields[k])
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// canonicalMap / canonicalStrings / canonicalInt64 are the lowercase
// aliases retained for the dense stripeapi internal call sites.
func canonicalMap(m map[string]string) string { return CanonicalMap(m) }
func canonicalStrings(xs []string) string     { return CanonicalStrings(xs) }
func canonicalInt64(n int64) string           { return CanonicalInt64(n) }
