// Package stripeapi constructs the Stripe-Go client used by the rest of
// the lib. It wires rho-kit's resilient HTTP client (retry + circuit
// breaker + tracing) into stripe-go's Backends so every Stripe API call
// the lib makes gets the same observability and reliability treatment.
//
// Resilience stack — verified non-cascading:
//
//  1. rho-kit's *http.Client provides circuit-breaking + connection
//     pooling + per-request timeouts. It does NOT retry at the
//     transport layer (rho-kit's own design rule, since transport
//     retries can't safely re-send consumed bodies). When the circuit
//     opens, requests fail-fast with circuitbreaker.ErrCircuitOpen.
//
//  2. Stripe-go's MaxNetworkRetries is the ONLY retry layer. It
//     honors the `Stripe-Should-Retry` response header (Stripe tells
//     the client whether a particular call is safe to replay) and the
//     `Retry-After` header for 429 rate limits. When the rho-kit CB
//     trips, the resulting ErrCircuitOpen is not a Stripe-marked-
//     retriable failure — stripe-go gives up immediately, so a tripped
//     CB doesn't amplify load against Stripe.
//
// Combined with the Idempotency-Key (see idempotency.go) this is the
// canonical Stripe-recommended pattern for safe retries.
package stripeapi

import (
	"net/http"
	"time"

	"github.com/bds421/rho-kit/httpx/v2"
	stripe "github.com/stripe/stripe-go/v82"
)

// Config configures the Stripe client.
type Config struct {
	// SecretKey is the Stripe API secret (sk_test_… in test mode,
	// sk_live_… in live mode). Required.
	//
	// Secret-lifecycle note: the string is held by value in this
	// Config and copied into stripe-go's internals. The byte data
	// lives until GC. For high-security deployments needing key
	// rotation without process restart, instantiate a fresh
	// *stripe.Client with the new key (Stripe accepts arbitrary
	// client lifetimes) and atomic-swap the connector's Stripe field
	// via Shutdown + New cycle.
	SecretKey string

	// Timeout bounds each Stripe HTTP request. Defaults to 30s if zero.
	Timeout time.Duration

	// MaxNetworkRetries is the number of times stripe-go will retry
	// a failed request (in addition to the original attempt) when
	// Stripe's response includes `Stripe-Should-Retry: true` or when
	// the failure is a network-level error. Defaults to 3 if zero.
	//
	// Stripe-go's retry implements exponential backoff with jitter
	// and respects the `Retry-After` header from 429 responses.
	//
	// Setting to a negative value disables retries entirely (useful
	// in tests that want fast deterministic failures).
	MaxNetworkRetries int

	// RoundTripperMiddleware optionally wraps the resilient HTTP
	// client's transport with caller-supplied middleware (e.g.
	// observability/metrics.Collector.StripeRoundTripper for outbound
	// Prometheus metrics). Receives the existing transport and returns
	// the wrapped one; nil leaves the transport unchanged.
	RoundTripperMiddleware func(http.RoundTripper) http.RoundTripper
}

const defaultMaxNetworkRetries = 3

// NewClient returns a configured *stripe.Client with rho-kit's resilient
// HTTP client backing every call AND stripe-go's smart retry enabled.
//
// Uses the modern stripe.Client (the legacy *client.API was deprecated
// in stripe-go v82 in favor of this surface). Apps that need a service
// the lib doesn't yet wrap reach for `conn.Stripe.V1XYZ.Method(ctx, ...)`.
func NewClient(cfg Config) *stripe.Client {
	if cfg.SecretKey == "" {
		panic("stripeapi.NewClient: SecretKey is required")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	retries := cfg.MaxNetworkRetries
	if retries == 0 {
		retries = defaultMaxNetworkRetries
	}
	if retries < 0 {
		retries = 0
	}

	httpClient := httpx.NewResilientHTTPClient(
		httpx.WithResilientTimeout(cfg.Timeout),
	)
	if cfg.RoundTripperMiddleware != nil {
		httpClient.Transport = cfg.RoundTripperMiddleware(httpClient.Transport)
	}

	backendCfg := &stripe.BackendConfig{
		HTTPClient:        httpClient,
		MaxNetworkRetries: stripe.Int64(int64(retries)),
	}
	// Stripe-go's Backends struct has four slots: API (the core
	// /v1/* surface), Connect (multi-account marketplace endpoints),
	// Uploads (file uploads for disputes / identity / etc.), and
	// MeterEvents (the dedicated v2 meter-events ingest endpoint).
	//
	// We deliberately don't initialize Connect — this library is
	// single-account by design (ADR-0007). Uploads + MeterEvents ARE
	// wired because Disputes.SubmitEvidence / Quotes.PDF stream
	// files and metering events use their own ingest path.
	backends := &stripe.Backends{
		API:         stripe.GetBackendWithConfig(stripe.APIBackend, backendCfg),
		Uploads:     stripe.GetBackendWithConfig(stripe.UploadsBackend, backendCfg),
		MeterEvents: stripe.GetBackendWithConfig(stripe.MeterEventsBackend, backendCfg),
	}

	return stripe.NewClient(cfg.SecretKey, stripe.WithBackends(backends))
}
