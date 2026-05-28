package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// Signer constructs the Stripe-Signature header value for a payload
// using the given webhook signing secret. The lib uses this in tests
// (so test code can POST signed events at its own Handle without
// going through real Stripe) and in TestSignatureVerification.
//
// The algorithm matches Stripe's documented scheme:
//
//	t=<unix-seconds>,v1=<hex(HMAC-SHA256(secret, t.body))>
type Signer struct {
	secret string
}

// NewSigner returns a Signer configured with secret (typically the
// "whsec_…" value Stripe reveals for a webhook endpoint, or any test
// secret used in unit tests).
func NewSigner(secret string) *Signer {
	if secret == "" {
		panic("webhooks.NewSigner: secret is required")
	}
	return &Signer{secret: secret}
}

// Sign returns the Stripe-Signature header value for body signed at
// the given timestamp.
func (s *Signer) Sign(body []byte, t time.Time) string {
	value, err := signPayload(s.secret, body, t)
	if err != nil {
		panic(fmt.Sprintf("webhooks.Signer.Sign: %v", err)) // unreachable
	}
	return value
}

// SignNow is a convenience for Sign(body, time.Now()).
func (s *Signer) SignNow(body []byte) string {
	return s.Sign(body, time.Now())
}

func signPayload(secret string, body []byte, t time.Time) (string, error) {
	ts := t.Unix()
	mac := hmac.New(sha256.New, []byte(secret))
	if _, err := fmt.Fprintf(mac, "%d.", ts); err != nil {
		return "", err
	}
	if _, err := mac.Write(body); err != nil {
		return "", err
	}
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil))), nil
}
