package webhooks_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bds421/rho-kit/data/v2/idempotency"
	"github.com/bds421/rho-stripe/webhooks"
)

// maxFuzzBodyBytes caps the fuzz-generated payload size. Real Stripe
// events are well under 100KiB; the lib's DefaultMaxBodyBytes is 1MiB.
// Capping the FUZZ domain (vs the lib's runtime cap) keeps the harness
// focused on shape-of-input bugs rather than burning workers on the
// JSON parser's worst-case performance with megabyte-scale adversarial
// inputs (which would just exercise encoding/json, not our code).
const maxFuzzBodyBytes = 64 * 1024

// maxHandleDuration is the per-input cutoff. A non-async-queue Handle
// call with no registered handlers does roughly: read body, verify
// signature (sub-ms), JSON-parse, dedup-claim (memory store: sub-ms),
// namespace filter (sub-ms), dispatch to no-op (sub-ms), return 200.
// In practice <10ms. A second is 100x headroom; if Handle takes longer
// than that on any input, it's a real performance bug worth a
// reproducer rather than a flaky CI failure.
const maxHandleDuration = 1 * time.Second

// FuzzHandleSignedArbitraryBody asserts the webhook handler never
// panics AND returns within maxHandleDuration on any signature-valid
// input. The fuzzer signs whatever it generates so we exercise the
// post-verify code paths (JSON parse, dedup, namespace filter,
// dispatch), which are the realistic attack surface for a request
// that already passed signature verification.
//
// On stall: the per-input timeout converts a worker hang into a
// `t.Fatal` so the fuzz framework saves the input as a regression
// seed in `testdata/fuzz/FuzzHandleSignedArbitraryBody/...`. Without
// the wrapper a stall just stops execs/sec until the fuzz harness's
// own deadline fires — that produced a stuck `0 execs/sec` followed
// by `context deadline exceeded` in CI with no reproducer.
func FuzzHandleSignedArbitraryBody(f *testing.F) {
	const secret = "whsec_fuzz_secret"
	signer := webhooks.NewSigner(secret)

	f.Add([]byte(`{"id":"evt_1","object":"event","type":"checkout.session.completed","api_version":"2025-08-27.basil","created":1,"data":{"object":{}}}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"type":"unknown","data":{"object":{"nested":{"deep":true}}}}`))
	f.Add([]byte(`null`))

	f.Fuzz(func(t *testing.T, body []byte) {
		// Skip empty (short-circuits inside Handle on JSON parse) and
		// over-sized inputs (out-of-scope per maxFuzzBodyBytes).
		if len(body) == 0 || len(body) > maxFuzzBodyBytes {
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

		// Contracts asserted: Handle must NEVER panic, and must return
		// within maxHandleDuration. Any 4xx/5xx body is acceptable —
		// the fuzz is about robustness + responsiveness, not about
		// returning the "right" status for nonsense input.
		done := make(chan struct{})
		go func() {
			defer close(done)
			wh.Handle(rec, req)
		}()
		select {
		case <-done:
		case <-time.After(maxHandleDuration):
			t.Fatalf("Handle did not return within %s on input of %d bytes — likely unbounded loop or hot-path with adversarial input", maxHandleDuration, len(body))
		}
	})
}
