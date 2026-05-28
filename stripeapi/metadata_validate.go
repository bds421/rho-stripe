package stripeapi

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

// Stripe metadata limits (from https://stripe.com/docs/api/metadata):
//   - max 50 keys per object
//   - key max 40 characters
//   - value max 500 characters
//
// Stripe returns 400 Bad Request when these are exceeded. We surface
// the limit before sending to Stripe so the error message is
// actionable (Stripe's response says "metadata size exceeded" with no
// indication of which key offended).
const (
	MetadataMaxKeys       = 50
	MetadataMaxKeyChars   = 40
	MetadataMaxValueChars = 500
)

// ValidateMetadata returns an error describing the first limit
// violation in m, or nil if m is valid for use as Stripe metadata.
// Apps that set metadata from user-controlled inputs should call this
// before the create call.
//
// The lib's own metadata-stamping paths (app_namespace, app_subject_id)
// stay well under the limits; this helper exists for apps mixing
// arbitrary input into metadata.
func ValidateMetadata(m map[string]string) error {
	if len(m) > MetadataMaxKeys {
		return fmt.Errorf("stripeapi.ValidateMetadata: %d keys exceeds Stripe limit of %d", len(m), MetadataMaxKeys)
	}
	for k, v := range m {
		if utf8.RuneCountInString(k) > MetadataMaxKeyChars {
			return fmt.Errorf("stripeapi.ValidateMetadata: key %q exceeds %d characters", k, MetadataMaxKeyChars)
		}
		if utf8.RuneCountInString(v) > MetadataMaxValueChars {
			return fmt.Errorf("stripeapi.ValidateMetadata: value for key %q exceeds %d characters (got %d)",
				k, MetadataMaxValueChars, utf8.RuneCountInString(v))
		}
	}
	return nil
}

// ErrMetadataTooLarge is the sentinel type apps can errors.Is against
// when they want to discriminate metadata-validation failures.
var ErrMetadataTooLarge = errors.New("stripeapi: metadata exceeds Stripe limits")

// NormalizeCurrency lowercases a currency code so it matches Stripe's
// API contract (lowercase ISO 4217: "eur", "usd", "gbp"). Stripe
// rejects uppercase codes. Apps passing user-typed currency values
// should run them through this first.
//
// The lib's internal call sites still pass currency strings unchanged
// (apps' catalog spec writes lowercase consistently); this helper is
// for app code constructing input from untrusted sources.
func NormalizeCurrency(currency string) string {
	// Manual lowercase avoids importing strings just for this; the
	// only valid characters are 3 ASCII letters anyway.
	b := make([]byte, len(currency))
	for i := 0; i < len(currency); i++ {
		c := currency[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}
