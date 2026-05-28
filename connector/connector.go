// Package connector is the lib's single wiring entrypoint. One call
// to New constructs a *Connector with every phase-0 subsystem
// (catalog cache, checkout, webhooks) pre-wired against Stripe.
//
// Apps that need finer-grained control can construct subsystems
// directly from the catalog / checkout / webhooks packages; the
// facade is for the common case of "give me a *Connector and I'll
// call .Checkout / .Webhooks / .Catalog on it."
package connector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bds421/rho-kit/data/v2/idempotency"
	"github.com/bds421/rho-stripe/catalog"
	"github.com/bds421/rho-stripe/checkout"
	"github.com/bds421/rho-stripe/checkout/portalconfig"
	"github.com/bds421/rho-stripe/climate"
	"github.com/bds421/rho-stripe/coupons"
	"github.com/bds421/rho-stripe/credits"
	"github.com/bds421/rho-stripe/customers"
	"github.com/bds421/rho-stripe/disputes"
	"github.com/bds421/rho-stripe/invoices"
	"github.com/bds421/rho-stripe/metering"
	"github.com/bds421/rho-stripe/paymentmethods"
	"github.com/bds421/rho-stripe/payments"
	"github.com/bds421/rho-stripe/plans"
	"github.com/bds421/rho-stripe/quotes"
	"github.com/bds421/rho-stripe/subscriptions"
	"github.com/bds421/rho-stripe/webhooks"
	stripe "github.com/stripe/stripe-go/v82"
)

// Config is the single configuration struct apps populate.
type Config struct {
	// SecretKey is the Stripe API secret (sk_test_… / sk_live_…).
	// Required unless BackendOverride and CheckoutBackendOverride are
	// both provided (used by tests).
	SecretKey string

	// WebhookSecret is the signing secret for this app's webhook
	// endpoint (whsec_…). Required for webhook delivery; if empty,
	// the .Webhooks subsystem will panic on construction.
	WebhookSecret string

	// AppNamespace identifies this app in a shared Stripe account.
	// Must match Catalog.Namespace. Stamped on every Stripe object
	// the lib creates; used by the webhook dispatcher to filter
	// events that belong to other apps. Required.
	AppNamespace string

	// Catalog is the declared spec. Required. Cache is warmed eagerly
	// at New time per adr-0003; New fails if any declared lookup_key
	// can't be resolved against Stripe.
	Catalog *catalog.Spec

	// Customers persists SubjectID ↔ Stripe Customer mappings.
	// Required. Use checkout.NewMemoryCustomerRepo() for tests.
	Customers checkout.CustomerRepo

	// Events deduplicates webhook deliveries. Required. Use
	// idempotency.NewMemoryStore() for tests; pgstore.Store for
	// production with Postgres.
	Events idempotency.Store

	// Handlers are the typed event handlers the webhook dispatcher
	// routes to. Optional; unset handlers cause matching events to
	// be marked processed without invoking app code.
	Handlers webhooks.Handlers

	// Credits optionally enables the auto-grant flow. When set, the
	// connector wraps Handlers.OnCheckoutCompleted with a pre-step
	// that reads credit_grants metadata from the completed session
	// and creates ledger entries via this repo. Apps that don't sell
	// credit-grant products leave it nil.
	Credits credits.CreditRepo

	// Usage optionally enables usage-based metering. Set to a
	// metering.UsageRepo; the connector builds a *metering.Operations
	// and exposes it as conn.Metering.
	Usage metering.UsageRepo

	// Subscriptions optionally enables subscription state mirroring.
	// When set, the connector wraps OnSubscription{Created,Updated,
	// Canceled} with a pre-step that parses the event and upserts the
	// mirror via this repo. Hot-path queries use subscriptions.ListActive
	// / HasActivePrice / ListByPriceKey against the repo directly.
	Subscriptions subscriptions.SubscriptionRepo

	// Logger is forwarded to subsystems. Defaults to slog.Default().
	Logger *slog.Logger

	// HTTPTimeout overrides the default per-request timeout in
	// stripeapi.Config. Optional.
	HTTPTimeout time.Duration

	// DedupTTL overrides webhooks.DefaultDedupTTL. Optional.
	DedupTTL time.Duration

	// SignatureTolerance overrides webhooks.DefaultSignatureTolerance.
	// Optional.
	SignatureTolerance time.Duration

	// BackendOverride and CheckoutBackendOverride let tests inject
	// fakes instead of constructing real stripe-go-backed backends
	// from SecretKey. When both are non-nil, SecretKey may be empty
	// and no Stripe client is constructed. SubscriptionBackendOverride
	// is optional; when nil, the default stripeapi.SubscriptionBackend
	// is constructed if Config.Subscriptions is set.
	BackendOverride             catalog.Backend
	CheckoutBackendOverride     checkout.Backend
	SubscriptionBackendOverride subscriptions.Backend

	// InvoiceBackendOverride lets tests wire a fake invoice backend
	// when SecretKey is empty (Stripe client not constructed).
	// Without this, Connector.Invoices would be nil in test setups
	// that use the catalog/checkout overrides — silently dropping
	// the invoice operations surface for any test that wanted to
	// exercise the auto-NumberRepo wiring or the OrphanHook path.
	InvoiceBackendOverride invoices.Backend

	// AsyncWebhooks, when non-nil, enables async webhook dispatch via
	// an internally-managed in-memory queue. Defer connector.Shutdown
	// to drain in-flight events on graceful shutdown.
	//
	// Apps needing a durable queue (Postgres-backed, NATS, SQS) skip
	// this and call conn.Webhooks.SetQueue(...) themselves after New.
	AsyncWebhooks *AsyncWebhookConfig

	// WebhookPerEventPolicy is passed through to webhooks.Config.PerEventPolicy.
	// Use to cap retry attempts on selected event types (write-offs,
	// admin events) with a give-up callback.
	WebhookPerEventPolicy map[string]webhooks.EventPolicy

	// WebhookEventLog is forwarded to webhooks.Config.EventLog.
	// Enables Webhooks.StuckEvents / RetrieveEvent / PruneOldEvents helpers
	// + populates the LoggedEvent fed to per-event policy OnExhausted.
	WebhookEventLog webhooks.EventLog

	// InvoiceNumberRepo enables gapless app-side invoice numbering.
	// When set, CreateDraft auto-allocates numbers and stamps the
	// orphan hook so apps can recover from ledger/Stripe split-brain.
	InvoiceNumberRepo invoices.NumberRepo

	// InvoiceOrphanHook is invoked when InvoiceNumberRepo and Stripe
	// disagree (see invoices.OrphanHook). Strongly recommended whenever
	// InvoiceNumberRepo is set so apps can alert / replay.
	InvoiceOrphanHook invoices.OrphanHook

	// DriftDetector, when non-nil, starts a background goroutine that
	// runs CheckDrift at the configured interval and invokes OnReport
	// for each result. The connector's Shutdown stops it cleanly.
	DriftDetector *DriftDetectorConfig

	// AutoRevokeCreditsOnRefund, when true, wraps the app's
	// OnRefundCreated handler with credits.ApplyRefundReversal.
	// Any grants whose SourceRef matches the refunded charge /
	// payment_intent are revoked automatically (idempotent).
	//
	// Requires Config.Credits to be non-nil; otherwise the option is
	// silently ignored (no ledger to revoke from).
	AutoRevokeCreditsOnRefund bool

	// AutoCancelSubscriptionOnFailedRefund, when true, cancels the
	// subscription tied to a refund whose Stripe attempt failed.
	//
	// Requires:
	//   - Config.Subscriptions non-nil
	//   - PaymentIntentToSubscriptionResolver supplied (Stripe doesn't
	//     directly carry the PI→subscription link on refund events;
	//     apps maintain their own index, typically populated at
	//     checkout-completion time).
	//
	// Without the resolver the flag logs a warning and does nothing.
	AutoCancelSubscriptionOnFailedRefund bool

	// PaymentIntentToSubscriptionResolver maps a Stripe PaymentIntent
	// id to a Subscription id. Used by the auto-cancel-on-failed-refund
	// flow. Apps wire it to their own table (typically populated when
	// checkout.session.completed lands carrying both ids).
	PaymentIntentToSubscriptionResolver subscriptions.PaymentIntentToSubscriptionResolver

	// AppDataExporter is invoked by Customers.Export(subjectID) to
	// gather app-owned data for inclusion in the GDPR export bundle.
	// Optional — without it, the export only includes lib + Stripe
	// data. See docs/howto/gdpr.md.
	AppDataExporter func(ctx context.Context, s customers.SubjectID) (map[string]any, error)

	// AppDataForgetter is invoked by Customers.Forget(subjectID) AFTER
	// Stripe + lib data have been deleted. The app deletes (or
	// anonymizes) its own records here. Strongly recommended for GDPR
	// compliance; without it, app-owned data persists past the Forget.
	AppDataForgetter func(ctx context.Context, s customers.SubjectID) error

	// RequireLiveKey, when true, makes connector.New refuse to start
	// with a test-mode Stripe key (sk_test_…). Set true in production
	// deployments to fail-fast on misconfiguration; leave false
	// (default) in dev / staging so test keys are accepted.
	//
	// Has no effect when BackendOverride is in use (test seam).
	RequireLiveKey bool

	// WebhookAutoPrune, when non-zero, starts a background goroutine
	// that periodically prunes EventLog rows older than the supplied
	// duration. Without this OR a caller-driven prune, the EventLog
	// accumulates PII forever (GDPR liability).
	//
	// Recommended: 90 days for typical apps (covers Stripe's 30-day
	// retry window + 60-day operator response budget).
	//
	// No-op when Config.WebhookEventLog is nil.
	WebhookAutoPruneMaxAge time.Duration

	// WebhookAutoPruneInterval is how often the auto-prune loop runs.
	// Defaults to 24h when WebhookAutoPruneMaxAge is set and this
	// field is zero.
	WebhookAutoPruneInterval time.Duration

	// WarmTimeout bounds the initial catalog cache warm-up call
	// against Stripe. Defaults to DefaultWarmTimeout (30s). Set to
	// a longer value for very large catalogs (>1000 lookup keys);
	// set to 0 to use the default. Catalog warm-up is critical-path
	// on connector.New, so without this bound a slow Stripe API
	// would block pod readiness indefinitely.
	WarmTimeout time.Duration
}

// DefaultWarmTimeout is the default upper bound on catalog cache
// warm-up at connector.New time. Real-world catalogs warm in well
// under 5 seconds; 30s is generous headroom.
const DefaultWarmTimeout = 30 * time.Second

// AsyncWebhookConfig enables in-memory async webhook dispatch.
type AsyncWebhookConfig struct {
	// Capacity bounds the enqueue channel. When full, Enqueue blocks
	// (the HTTP request stays open until either a worker drains the
	// queue or the request's context is canceled). Defaults to 256.
	Capacity int

	// Workers is the number of goroutines draining the queue.
	// Defaults to 4.
	Workers int
}

// DriftDetectorConfig configures the background drift detection loop.
type DriftDetectorConfig struct {
	// Interval between checks. Defaults to 15 minutes.
	Interval time.Duration

	// OnReport is invoked for each report — both with and without drift.
	// Apps typically branch on report.HasDrift to alert / auto-remediate.
	OnReport func(catalog.DriftReport)
}

// Connector exposes the wired subsystems and the raw Stripe client
// for advanced escape-hatch calls.
//
// Field count rationale: ~18 sub-system fields is large, but every
// field corresponds to a distinct Stripe-domain concept (Catalog,
// Checkout, Webhooks, Subscriptions, etc.). We considered grouping
// (e.g. `conn.Billing.Subscriptions`, `conn.Customer.Disputes`) and
// rejected it:
//
//   - Grouping is cosmetic — the underlying API surface is identical
//     and every adopter still depends on every subsystem they use.
//   - Flat fields work best with Go's import + autocomplete:
//     `conn.Quotes.Create(...)` reads cleaner than
//     `conn.Billing.Quotes.Create(...)`.
//   - Forcing a refactor on every adopter for a rename costs more
//     than it saves.
//
// New subsystems should be added as flat fields here unless they
// truly compose with existing ones (in which case nesting is fine).
type Connector struct {
	// Catalog is the warmed lookup-key cache. Apps rarely call this
	// directly; Checkout uses it under the hood.
	Catalog *catalog.Cache

	// Checkout creates Checkout Sessions and Customer Portal sessions.
	Checkout *checkout.Checkout

	// Webhooks is the HTTP handler for Stripe events.
	Webhooks *webhooks.Webhooks

	// Subscriptions is the operations + hot-path query surface for
	// subscriptions. nil when Config.Subscriptions is unset.
	Subscriptions *subscriptions.Operations

	// Coupons mints and manages customer-facing promo codes (the
	// catalog declares the underlying Coupons). Always non-nil when
	// a Stripe client could be constructed.
	Coupons *coupons.Operations

	// Credits is the credit-ledger operations + hot-path query
	// surface. nil when Config.Credits is unset.
	Credits *credits.Operations

	// Metering is the usage-recording + querying surface. nil when
	// Config.Usage is unset.
	Metering *metering.Operations

	// Invoices is the invoice creation + management surface. Always
	// non-nil when a Stripe client could be constructed.
	Invoices *invoices.Operations

	// Refunds issues refunds against past charges / payment intents.
	// Always non-nil when a Stripe client could be constructed.
	// Apps that need EU-compliant credit notes use conn.Invoices.IssueCreditNote
	// instead — Refunds is for the no-invoice / B2C case.
	Refunds *payments.Operations

	// Plans encapsulates "what can this customer do" — limits +
	// feature flags + remaining-credits + grace-period status,
	// aggregated across every active subscription. Non-nil when
	// Config.Subscriptions is set.
	//
	// See docs/howto/plan-limits.md for the metadata convention.
	Plans *plans.Operations

	// Climate wraps Stripe's Climate Orders API for apps that want
	// to programmatically buy carbon-removal contributions. Nil when
	// no Stripe client could be constructed.
	Climate *climate.Operations

	// Customers groups customer-aggregate operations: GDPR export +
	// forget, B2B Tax ID management, and backfill for migrating
	// existing Stripe customers into the lib's namespace.
	//
	// nil when no Stripe client could be constructed.
	Customers *customers.Operations

	// PaymentMethods covers card-on-file flows that don't go through
	// Checkout: SetupIntent creation, list/attach/detach/set-default
	// saved payment methods. Nil when no Stripe client.
	PaymentMethods *paymentmethods.Operations

	// Disputes wraps Stripe's chargeback API for submitting evidence
	// or acknowledging loss. Nil when no Stripe client.
	Disputes *disputes.Operations

	// Quotes wraps Stripe's Quote API for B2B sales-led workflows.
	// Nil when no Stripe client.
	Quotes *quotes.Operations

	// PortalConfig declaratively syncs the Stripe Customer Portal
	// configuration (features customers see in the portal). Nil when
	// no Stripe client. Call conn.PortalConfig.Sync(ctx, spec) at
	// deploy time, similar to catalog.Apply.
	PortalConfig *portalconfig.Operations

	// Stripe is the raw stripe-go *stripe.Client — the escape hatch for
	// Stripe operations the lib doesn't wrap yet.
	//
	// STABILITY CONTRACT: this field is part of the public stable
	// API. We will not remove it, retype it, or wrap it in another
	// layer. Once apps depend on `conn.Stripe.V1Whatever.Method(...)`
	// we cannot break that without forcing a coordinated rewrite
	// across every adopter. Earlier docs said "reserves the right
	// to change" — that was unenforceable wishful thinking; once
	// exposed, an API IS public.
	//
	// What this means in practice:
	//   - Major-version-locked to stripe-go's v82. Bumping stripe-go
	//     to v83 will require a major version bump of this lib.
	//   - We will NOT silently swap *stripe.Client for a wrapper type.
	//   - We MAY add new lib subsystems (conn.NewThing) that
	//     supersede a raw-Stripe pattern; the escape hatch keeps
	//     working unchanged.
	//
	// Prefer the typed surfaces when available (they handle
	// idempotency keys, namespace stamping, mirror updates).
	Stripe *stripe.Client

	spec     *catalog.Spec
	shutdown []func(context.Context) error // ordered shutdown hooks
	health   *healthState
}

// Shutdown gracefully tears down resources the connector owns:
//   - async webhook queue (drains in-flight workers)
//   - background drift detector
//
// Also releases the namespace lock so a subsequent New for the same
// namespace can proceed (used in tests + zero-downtime reload flows).
//
// Safe to call multiple times. Returns the first non-nil error
// from any registered shutdown hook.
//
// The namespace-lock release is deferred FIRST so even a panicking
// shutdown hook still releases the lock — otherwise a single bad
// hook would permanently block re-construction.
func (c *Connector) Shutdown(ctx context.Context) error {
	if c.spec != nil {
		defer activeNamespaces.Delete(c.spec.Namespace)
	}
	var firstErr error
	for _, hook := range c.shutdown {
		if err := hook(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	c.shutdown = nil
	return firstErr
}

// activeNamespaces tracks live Connectors so a process accidentally
// calling connector.New twice for the same namespace fails fast
// instead of silently constructing two competing webhook dispatchers
// (which would leave dedup state inconsistent across the two).
//
// Lifecycle caveat: a Connector that's GC'd without Shutdown being
// called still holds its namespace entry — there's no finalizer.
// In tests this is benign (test process exits between runs). In
// production this can leak only if a long-running process spawns
// Connectors dynamically and forgets to Shutdown previous ones,
// which would be unusual.
var activeNamespaces sync.Map // map[string]struct{}

// ErrDuplicateNamespace is returned by [New] when a Connector for the
// requested AppNamespace is already alive in this process (Shutdown
// not yet called on the prior instance). The typical cause is a test
// that forgot to t.Cleanup the connector; multi-tenant hot paths that
// rotate connectors on config-reload also need to call Shutdown first.
var ErrDuplicateNamespace = errors.New("connector: a Connector for this namespace is already active in this process — call Shutdown on the prior instance first")

// New validates the config, constructs every subsystem, warms the
// catalog cache against Stripe, and returns the wired Connector.
// Fails fast on misconfiguration (missing fields, namespace mismatch,
// unreachable Stripe) so deployment problems surface at startup
// instead of mid-checkout.
//
// Returns [ErrDuplicateNamespace] if a Connector with the same
// AppNamespace is already alive in this process. Apps that rotate
// connectors on config-reload check for this with errors.Is and call
// Shutdown on the prior instance before retrying.
func New(ctx context.Context, cfg Config) (*Connector, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if _, loaded := activeNamespaces.LoadOrStore(cfg.AppNamespace, struct{}{}); loaded {
		return nil, fmt.Errorf("%w: namespace %q", ErrDuplicateNamespace, cfg.AppNamespace)
	}
	// Release the lock if New returns an error so the caller can retry
	// after fixing the problem. Successful return leaves the lock held
	// until Shutdown.
	ok := false
	defer func() {
		if !ok {
			activeNamespaces.Delete(cfg.AppNamespace)
		}
	}()

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	sc, backend, checkoutBackend := buildStripeClient(cfg)

	cache := catalog.NewCache(asPriceLister(backend))
	warmTimeout := cfg.WarmTimeout
	if warmTimeout <= 0 {
		warmTimeout = DefaultWarmTimeout
	}
	if err := warmCatalogWithTimeout(ctx, cache, cfg.Catalog, warmTimeout); err != nil {
		return nil, err
	}

	ck := checkout.New(checkout.Config{
		Namespace: cfg.AppNamespace,
		Spec:      cfg.Catalog,
		Resolver:  cache,
		Customers: cfg.Customers,
		Backend:   checkoutBackend,
	})

	handlers := cfg.Handlers
	if cfg.Credits != nil {
		userOnCheckoutCompleted := handlers.OnCheckoutCompleted
		handlers.OnCheckoutCompleted = func(ctx context.Context, evt webhooks.Event) error {
			if err := credits.ApplyGrantsFromSession(ctx, cfg.Credits, evt, logger); err != nil {
				return err
			}
			if userOnCheckoutCompleted != nil {
				return userOnCheckoutCompleted(ctx, evt)
			}
			return nil
		}
	}
	// Construct subOps early so handler-wrap closures below can
	// capture it (auto-cancel-on-failed-refund needs it).
	subOps, err := buildSubscriptionOps(cfg, sc, cache)
	if err != nil {
		return nil, err
	}

	if cfg.Subscriptions != nil {
		handlers.OnSubscriptionCreated = wrapSubscriptionHandler(cfg.Subscriptions, cache, logger, handlers.OnSubscriptionCreated)
		handlers.OnSubscriptionUpdated = wrapSubscriptionHandler(cfg.Subscriptions, cache, logger, handlers.OnSubscriptionUpdated)
		handlers.OnSubscriptionCanceled = wrapSubscriptionHandler(cfg.Subscriptions, cache, logger, handlers.OnSubscriptionCanceled)
	}
	if cfg.AutoCancelSubscriptionOnFailedRefund && subOps != nil {
		userOnRefundFailed := handlers.OnRefundFailed
		handlers.OnRefundFailed = func(ctx context.Context, evt webhooks.Event) error {
			if err := subscriptions.CancelSubscriptionFromFailedRefund(ctx, subOps, cfg.PaymentIntentToSubscriptionResolver, evt, logger); err != nil {
				logger.WarnContext(ctx, "connector: auto-cancel on failed refund", "err", err)
			}
			if userOnRefundFailed != nil {
				return userOnRefundFailed(ctx, evt)
			}
			return nil
		}
	}
	if cfg.Credits != nil {
		userOnInvoicePaid := handlers.OnInvoicePaid
		handlers.OnInvoicePaid = func(ctx context.Context, evt webhooks.Event) error {
			if err := credits.ApplyRecurringGrantsFromInvoice(ctx, cfg.Credits, cfg.Catalog, evt, logger); err != nil {
				return err
			}
			if userOnInvoicePaid != nil {
				return userOnInvoicePaid(ctx, evt)
			}
			return nil
		}
		if cfg.AutoRevokeCreditsOnRefund {
			userOnRefundCreated := handlers.OnRefundCreated
			handlers.OnRefundCreated = func(ctx context.Context, evt webhooks.Event) error {
				if err := credits.ApplyRefundReversal(ctx, cfg.Credits, evt, logger); err != nil {
					return err
				}
				if userOnRefundCreated != nil {
					return userOnRefundCreated(ctx, evt)
				}
				return nil
			}
		}
	}

	wh := webhooks.New(webhooks.Config{
		SigningSecret:      cfg.WebhookSecret,
		Store:              cfg.Events,
		Namespace:          cfg.AppNamespace,
		Handlers:           handlers,
		DedupTTL:           cfg.DedupTTL,
		SignatureTolerance: cfg.SignatureTolerance,
		Logger:             logger,
		PerEventPolicy:     cfg.WebhookPerEventPolicy,
		EventLog:           cfg.WebhookEventLog,
	})

	var shutdownHooks []func(context.Context) error

	// Wire the async queue AFTER constructing wh so the worker
	// dispatch can refer to wh.ProcessQueued (chicken-and-egg).
	if cfg.AsyncWebhooks == nil {
		// Log a one-time warning. Sync dispatch is fine for fast
		// handlers (< 100ms), but Stripe's 30s ACK timeout means a
		// slow handler triggers a retry storm. AsyncWebhooks is the
		// well-trodden mitigation; we flag the trade-off explicitly
		// so production operators see it in startup logs.
		logger.WarnContext(ctx, "connector: webhook dispatch is SYNCHRONOUS (default) — handlers must complete in <5s. Stripe's 30s ACK timeout triggers retry storms on slow handlers. Consider Config.AsyncWebhooks for production workloads. See docs/howto/webhooks-async-dispatch.md.")
	} else {
		capacity := cfg.AsyncWebhooks.Capacity
		workers := cfg.AsyncWebhooks.Workers
		q := webhooks.NewMemoryQueue(capacity, workers, wh.ProcessQueued)
		wh.SetQueue(q)
		shutdownHooks = append(shutdownHooks, q.Shutdown)
	}

	if cfg.DriftDetector != nil {
		stop := catalog.RunDriftDetector(
			ctx, backend, cfg.Catalog,
			cfg.DriftDetector.Interval, cfg.DriftDetector.OnReport, logger,
		)
		shutdownHooks = append(shutdownHooks, func(_ context.Context) error {
			stop()
			return nil
		})
	}

	if cfg.WebhookAutoPruneMaxAge > 0 && cfg.WebhookEventLog != nil {
		interval := cfg.WebhookAutoPruneInterval
		if interval == 0 {
			interval = 24 * time.Hour
		}
		stop := wh.StartAutoPrune(cfg.WebhookAutoPruneMaxAge, interval)
		shutdownHooks = append(shutdownHooks, func(_ context.Context) error {
			stop()
			return nil
		})
	}

	invoiceOps := buildInvoiceOps(cfg, sc)
	creditOps, meteringOps, planOps := buildOptionalSubsystems(cfg)
	agg := buildCustomerAggregate(cfg, sc, subOps)

	ok = true
	return &Connector{
		Catalog:        cache,
		Checkout:       ck,
		Webhooks:       wh,
		Subscriptions:  subOps,
		Coupons:        agg.Coupons,
		Credits:        creditOps,
		Metering:       meteringOps,
		Invoices:       invoiceOps,
		Refunds:        agg.Refunds,
		Plans:          planOps,
		Climate:        agg.Climate,
		Customers:      agg.Customers,
		PaymentMethods: agg.PaymentMethods,
		Disputes:       agg.Disputes,
		Quotes:         agg.Quotes,
		PortalConfig:   agg.PortalConfig,
		Stripe:         sc,
		spec:           cfg.Catalog,
		shutdown:       shutdownHooks,
		health:         newHealthState(),
	}, nil
}

// Spec returns the catalog spec the Connector was constructed with.
// Useful for code paths (e.g. ops dashboards) that need to enumerate
// what the app sells.
//
// Note: c.Catalog (the *catalog.Cache field) and Spec() serve
// different roles — Cache holds runtime price-id lookups, Spec()
// returns the declarative catalog. Apps doing read-only catalog
// introspection should call Spec(); apps doing runtime price lookups
// should use Catalog.Lookup.
func (c *Connector) Spec() *catalog.Spec { return c.spec }

func validateConfig(cfg Config) error {
	if cfg.AppNamespace == "" {
		return errors.New("connector: AppNamespace is required")
	}
	if cfg.Catalog == nil {
		return errors.New("connector: Catalog is required")
	}
	if cfg.Catalog.Namespace != cfg.AppNamespace {
		return fmt.Errorf("connector: AppNamespace (%q) must match Catalog.Namespace (%q)",
			cfg.AppNamespace, cfg.Catalog.Namespace)
	}
	if cfg.Customers == nil {
		return errors.New("connector: Customers is required")
	}
	if cfg.Events == nil {
		return errors.New("connector: Events is required")
	}
	hasOverrides := cfg.BackendOverride != nil && cfg.CheckoutBackendOverride != nil
	if !hasOverrides && cfg.SecretKey == "" {
		return errors.New("connector: SecretKey is required (or provide both backend overrides)")
	}
	if cfg.RequireLiveKey && !hasOverrides {
		// We refuse to start with a test key in "production mode".
		// This prevents the canonical incident where a deployment
		// inherits a sk_test_ from dev secrets manager and silently
		// processes nothing.
		if len(cfg.SecretKey) >= 7 && cfg.SecretKey[:7] == "sk_test" {
			return errors.New("connector: RequireLiveKey=true but SecretKey is a test-mode key (sk_test_…) — refusing to start. Either set a live key or set RequireLiveKey=false explicitly")
		}
	}
	return nil
}

// wrapSubscriptionHandler returns a webhook handler that first runs
// subscriptions.ApplyEventToMirror (so the mirror is updated before
// the app sees the event) and then delegates to the app's handler
// (when non-nil).
func wrapSubscriptionHandler(
	repo subscriptions.SubscriptionRepo,
	resolver subscriptions.PriceKeyResolver,
	logger *slog.Logger,
	userHandler func(context.Context, webhooks.Event) error,
) func(context.Context, webhooks.Event) error {
	return func(ctx context.Context, evt webhooks.Event) error {
		if err := subscriptions.ApplyEventToMirror(ctx, repo, resolver, evt, logger); err != nil {
			return err
		}
		if userHandler != nil {
			return userHandler(ctx, evt)
		}
		return nil
	}
}

// asPriceLister returns b as a catalog.PriceLister when possible.
// The default stripeapi.Backend implements both; tests passing a
// catalog.Backend that doesn't also implement PriceLister fail at
// cache warmup with a clear error.
func asPriceLister(b catalog.Backend) catalog.PriceLister {
	if pl, ok := b.(catalog.PriceLister); ok {
		return pl
	}
	return errorLister{}
}

type errorLister struct{}

func (errorLister) ListByLookupKeys(_ context.Context, _ []string) ([]catalog.ResolvedPrice, error) {
	return nil, errors.New("connector: BackendOverride does not implement catalog.PriceLister; pass a backend that satisfies both interfaces")
}
