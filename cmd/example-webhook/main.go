// example-webhook is a tiny HTTP server that stands up the full
// rho-stripe pipeline (Catalog + Checkout + Webhooks + Credits)
// for end-to-end testing with `stripe listen --forward-to`.
//
// Usage:
//
//	# Terminal 1 — start the forwarder; copy the whsec_ it prints.
//	stripe listen --forward-to localhost:8080/webhook
//
//	# Terminal 2 — run this server.
//	set -a; source .env; set +a
//	STRIPE_WEBHOOK_SECRET=whsec_<from-terminal-1> \
//	  go run ./cmd/example-webhook
//
//	# Terminal 3 — fire a sample event (proves the pipeline works).
//	stripe trigger checkout.session.completed
//
//	# OR for the real credit-grant path: create a checkout in a browser.
//	go run ./cmd/example-sync checkout credit_pack_1000_ai.default
//	# (open the printed URL; complete with 4242 4242 4242 4242)
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bds421/rho-kit/data/v2/idempotency"
	"github.com/bds421/rho-stripe/catalog"
	"github.com/bds421/rho-stripe/checkout"
	"github.com/bds421/rho-stripe/connector"
	"github.com/bds421/rho-stripe/credits"
	"github.com/bds421/rho-stripe/invoices"
	"github.com/bds421/rho-stripe/subscriptions"
	"github.com/bds421/rho-stripe/webhooks"
)

var demoCatalog = catalog.MustSpec(catalog.Spec{
	Namespace: "scdemo",
	Products: map[string]catalog.Product{
		"pro_plan": {
			Name:        "Demo Pro Plan",
			TaxCategory: catalog.TaxCategorySaaS,
			Prices: map[string]catalog.Price{
				"monthly_eur": {Amount: 4900, Currency: "eur", Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
				"yearly_eur":  {Amount: 49000, Currency: "eur", Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalYear},
			},
		},
		"credit_pack_1000_ai": {
			Name:        "1000 Demo AI Credits",
			TaxCategory: catalog.TaxCategorySaaS,
			Prices: map[string]catalog.Price{
				"default": {Amount: 1000, Currency: "eur", Type: catalog.PriceTypeOneTime},
			},
			CreditGrant: &catalog.CreditGrant{Bucket: "ai", Amount: 1000, ValidDays: 90},
		},
	},
})

func main() {
	secretKey := os.Getenv("STRIPE_SECRET_KEY")
	webhookSecret := os.Getenv("STRIPE_WEBHOOK_SECRET")
	if secretKey == "" {
		log.Fatal("STRIPE_SECRET_KEY env var is required")
	}
	if webhookSecret == "" {
		log.Fatal("STRIPE_WEBHOOK_SECRET env var is required (copy from `stripe listen` output)")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	creditRepo := credits.NewMemoryRepo()
	customerRepo := checkout.NewMemoryCustomerRepo()
	subRepo := subscriptions.NewMemoryRepo()
	numberRepo := invoices.NewMemoryNumberRepo()

	ctx := context.Background()
	conn, err := connector.New(ctx, connector.Config{
		SecretKey:     secretKey,
		WebhookSecret: webhookSecret,
		AppNamespace:  "scdemo",
		Catalog:       demoCatalog,
		Customers:     customerRepo,
		Events:        idempotency.NewMemoryStore(),
		Credits:       creditRepo,
		Subscriptions: subRepo,
		Logger:        logger,

		// Slice 35: async dispatch + per-event policy.
		AsyncWebhooks: &connector.AsyncWebhookConfig{Capacity: 256, Workers: 4},
		WebhookPerEventPolicy: map[string]webhooks.EventPolicy{
			// Demo: after 5 failed attempts on invoice.paid, give up
			// with a loud log line instead of retrying forever.
			"invoice.paid": {
				MaxAttempts: 5,
				OnExhausted: func(_ context.Context, evt webhooks.LoggedEvent) error {
					log.Printf("[policy] invoice.paid event %s exhausted after %d attempts; giving up", evt.EventID, evt.AttemptCount)
					return nil
				},
			},
		},
		WebhookEventLog: webhooks.NewMemoryLog(),

		// Slice 36: continuous drift detection.
		DriftDetector: &connector.DriftDetectorConfig{
			Interval: 5 * time.Minute,
			OnReport: func(rep catalog.DriftReport) {
				if rep.HasDrift {
					log.Printf("[drift] %s", rep.FormatHuman())
				} else {
					log.Printf("[drift] check at %s — no drift", rep.CheckedAt.Format(time.RFC3339))
				}
			},
		},

		// Slice 41: invoice number ownership for compliance-mode apps.
		InvoiceNumberRepo: numberRepo,
		InvoiceOrphanHook: func(_ context.Context, info invoices.OrphanInfo) error {
			log.Printf("[invoice-orphan] reason=%s number=%s stripe=%s ledger=%v stripe_err=%v",
				info.Reason, info.Number, info.StripeInvoiceID, info.LedgerError, info.StripeError)
			return nil
		},

		Handlers: webhooks.Handlers{
			OnCheckoutCompleted: func(ctx context.Context, evt webhooks.Event) error {
				log.Printf("[handler] checkout.session.completed evt=%s", evt.ID)
				// Auto-grant has already run; show what's in the ledger.
				logBalances(ctx, creditRepo)
				return nil
			},
			OnPaymentSucceeded: func(_ context.Context, evt webhooks.Event) error {
				log.Printf("[handler] payment_intent.succeeded evt=%s", evt.ID)
				return nil
			},
			OnSubscriptionCreated: func(ctx context.Context, evt webhooks.Event) error {
				log.Printf("[handler] customer.subscription.created evt=%s", evt.ID)
				logSubscriptions(ctx, subRepo)
				return nil
			},
			OnSubscriptionUpdated: func(ctx context.Context, evt webhooks.Event) error {
				log.Printf("[handler] customer.subscription.updated evt=%s", evt.ID)
				logSubscriptions(ctx, subRepo)
				return nil
			},
			OnSubscriptionCanceled: func(ctx context.Context, evt webhooks.Event) error {
				log.Printf("[handler] customer.subscription.deleted evt=%s", evt.ID)
				logSubscriptions(ctx, subRepo)
				return nil
			},
			OnInvoicePaid: func(_ context.Context, evt webhooks.Event) error {
				log.Printf("[handler] invoice.paid evt=%s", evt.ID)
				return nil
			},
			OnOtherEvent: func(_ context.Context, evt webhooks.Event) error {
				log.Printf("[handler] other-event evt=%s type=%s", evt.ID, evt.Type)
				return nil
			},
		},
	})
	if err != nil {
		log.Fatalf("connector.New: %v", err)
	}

	if err := conn.Webhooks.TestSignatureVerification(); err != nil {
		log.Fatalf("startup self-test failed: %v", err)
	}
	log.Println("signature verification self-test ok")

	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", conn.Webhooks.Handle)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	srv := &http.Server{
		Addr:              ":8080",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Graceful shutdown: drain the async queue + stop the drift
	// detector before exiting.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("shutdown signal received; draining queue + stopping drift detector")
		shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = conn.Shutdown(shutCtx)
		_ = srv.Shutdown(shutCtx)
	}()

	log.Println("listening on :8080 (/webhook, /health) — async webhooks ON, drift detector ON")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server: %v", err)
	}
}

// logSubscriptions dumps every known subscription mirror row.
func logSubscriptions(ctx context.Context, repo subscriptions.SubscriptionRepo) {
	for _, subj := range []subscriptions.SubjectID{"cli_demo", "ns_test_user", "org_acme"} {
		list, _ := repo.ListBySubject(ctx, subj)
		for _, s := range list {
			items := ""
			for _, it := range s.Items {
				items += " " + it.PriceKey
			}
			log.Printf("[subs-mirror] subject=%s sub=%s status=%s items=%s period_end=%s",
				s.SubjectID, s.StripeID, s.Status, items, s.CurrentPeriodEnd.Format(time.RFC3339))
		}
	}
}

// logBalances dumps every subject's grants for visibility. Demo only —
// real apps would expose a balance endpoint or page to the customer.
func logBalances(ctx context.Context, repo credits.CreditRepo) {
	// MemoryRepo doesn't expose a "list subjects" method, so we just
	// query for the demo subjects the CLI uses.
	for _, subj := range []credits.SubjectID{"cli_demo", "ns_test_user"} {
		list, _ := repo.ListBySubject(ctx, subj)
		if len(list) == 0 {
			continue
		}
		for _, g := range list {
			log.Printf("[ledger] subject=%s bucket=%s amount=%d expires_at=%v source_ref=%s",
				g.SubjectID, g.Bucket, g.AmountInitial, g.ExpiresAt, g.SourceRef)
		}
	}
}
