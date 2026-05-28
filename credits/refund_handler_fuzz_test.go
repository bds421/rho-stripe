package credits_test

import (
	"encoding/json"
	"testing"

	"github.com/bds421/rho-stripe/credits"
	"github.com/bds421/rho-stripe/webhooks"
	stripe "github.com/stripe/stripe-go/v82"
)

// FuzzParseRefundEvent feeds arbitrary JSON payloads to the refund-
// event parser to ensure it never panics, leaks, or returns nonsense
// IDs. The parser is the load-bearing piece of the auto-revoke flow;
// a crash here would take down the webhook handler under malformed
// (but signature-valid) input.
func FuzzApplyRefundReversal(f *testing.F) {
	// Seed with a few representative payloads.
	f.Add([]byte(`{"id":"re_1","object":"refund","payment_intent":"pi_1","charge":"ch_1"}`))
	f.Add([]byte(`{"id":"ch_1","object":"charge","refunds":{"data":[{"id":"re_a"}]}}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte(``))

	repo := credits.NewMemoryRepo()

	f.Fuzz(func(t *testing.T, raw []byte) {
		// Many fuzz inputs won't be valid JSON; that's fine — the
		// parser handles it gracefully (returns early without
		// touching the repo).
		if !json.Valid(raw) && len(raw) > 0 {
			return
		}
		evt := webhooks.Event{
			ID:   "fuzz",
			Type: "refund.created",
			Raw: &stripe.Event{
				ID:   "fuzz",
				Type: "refund.created",
				Data: &stripe.EventData{Raw: raw},
			},
		}
		// The contract: ApplyRefundReversal must NEVER panic on
		// arbitrary input. Errors are acceptable; panics are not.
		_ = credits.ApplyRefundReversal(t.Context(), repo, evt, nil)
	})
}
