package webhooks_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bds421/rho-kit/data/v2/idempotency"
	"github.com/bds421/rho-stripe/webhooks"
)

// FuzzHandle_DoesNotPanicOnArbitraryBody asserts the webhook
// handler never panics on adversarial bodies. We restrict the fuzz
// to "valid signature, garbage body" by signing the input — this
// targets the JSON / event-parsing paths inside Handle, which are
// the realistic attack surface after a sig-verified body.
func FuzzHandleSignedArbitraryBody(f *testing.F) {
	const secret = "whsec_fuzz_secret"
	signer := webhooks.NewSigner(secret)

	f.Add([]byte(`{"id":"evt_1","object":"event","type":"checkout.session.completed","api_version":"2025-08-27.basil","created":1,"data":{"object":{}}}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"type":"unknown","data":{"object":{"nested":{"deep":true}}}}`))
	f.Add([]byte(`null`))

	f.Fuzz(func(t *testing.T, body []byte) {
		// Sign whatever the fuzz produced so signature verification
		// passes; we want to exercise the post-verify code paths.
		// Empty body short-circuits inside Handle on JSON parse, so
		// we accept that as a fast-exit.
		if len(body) == 0 {
			return
		}
		sig := signer.SignNow(body)

		wh := webhooks.New(webhooks.Config{
			SigningSecret: secret,
			Store:         idempotency.NewMemoryStore(),
		})
		req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
		req.Header.Set("Stripe-Signature", sig)
		rec := httptest.NewRecorder()

		// The contract: Handle must NEVER panic. Any 4xx/5xx is
		// acceptable — the fuzz is about robustness, not about
		// returning the "right" status for nonsense input.
		wh.Handle(rec, req)
	})
}
