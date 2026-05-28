// 30-second quickstart for rho-stripe.
//
// What this does:
//  1. Declares a single Pro plan in Go.
//  2. Syncs it to Stripe (creates Product + Price).
//  3. Spins up an HTTP server with:
//     POST /checkout/:subject  → returns a hosted Checkout URL
//     POST /webhook            → verifies + dispatches Stripe events
//     GET  /healthz            → readiness probe
//
// Run:
//
//	export STRIPE_SECRET_KEY=sk_test_…
//	export STRIPE_WEBHOOK_SECRET=whsec_…
//	go run .
//
// Then in another terminal:
//
//	curl -X POST localhost:8080/checkout/test-user-1
//
// To replay webhooks locally:  `stripe listen --forward-to localhost:8080/webhook`
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/bds421/rho-kit/data/v2/idempotency"
	"github.com/bds421/rho-stripe/catalog"
	"github.com/bds421/rho-stripe/checkout"
	"github.com/bds421/rho-stripe/connector"
	"github.com/bds421/rho-stripe/webhooks"
)

const namespace = "quickstart"

var spec = catalog.MustSpec(catalog.Spec{
	Namespace: namespace,
	Products: map[string]catalog.Product{
		"pro": {
			Name:        "Pro plan",
			TaxCategory: catalog.TaxCategorySaaSBusiness,
			Prices: map[string]catalog.Price{
				"monthly": {
					Amount:   1900, // €19.00
					Currency: "eur",
					Type:     catalog.PriceTypeRecurring,
					Interval: catalog.IntervalMonth,
				},
			},
		},
	},
})

func main() {
	// log.Fatal* exits without running deferred functions, so all
	// resource-owning code lives in run() — that lets `defer
	// conn.Shutdown` actually fire on error paths.
	if err := run(); err != nil {
		log.Fatalf("quickstart: %v", err)
	}
}

func run() error {
	ctx := context.Background()

	secretKey, err := requireEnv("STRIPE_SECRET_KEY")
	if err != nil {
		return err
	}
	webhookSecret, err := requireEnv("STRIPE_WEBHOOK_SECRET")
	if err != nil {
		return err
	}

	conn, err := connector.New(ctx, connector.Config{
		SecretKey:     secretKey,
		WebhookSecret: webhookSecret,
		AppNamespace:  namespace,
		Catalog:       spec,
		Customers:     checkout.NewMemoryCustomerRepo(),
		Events:        idempotency.NewMemoryStore(),
		Handlers: webhooks.Handlers{
			OnInvoicePaid: func(_ context.Context, evt webhooks.Event) error {
				log.Printf("invoice paid: %s", evt.ID)
				return nil
			},
			OnSubscriptionCreated: func(_ context.Context, evt webhooks.Event) error {
				log.Printf("subscription created: %s", evt.ID)
				return nil
			},
		},
	})
	if err != nil {
		return fmt.Errorf("connector.New: %w", err)
	}
	defer func() {
		if err := conn.Shutdown(context.Background()); err != nil {
			log.Printf("shutdown error: %v", err)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /checkout/{subject}", func(w http.ResponseWriter, r *http.Request) {
		subj := r.PathValue("subject")
		sess, err := conn.Checkout.CreateSession(r.Context(), checkout.Input{
			SubjectID:    checkout.SubjectID(subj),
			LineItems:  []checkout.LineItem{{PriceKey: "pro.monthly"}},
			SuccessURL: "https://example.com/success",
			CancelURL:  "https://example.com/cancel",
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fmt.Fprintln(w, sess.URL)
	})
	mux.HandleFunc("POST /webhook", conn.Webhooks.Handle)
	mux.Handle("GET /healthz", conn.HealthHandler())

	log.Println("quickstart listening on :8080")
	if err := http.ListenAndServe(":8080", mux); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("listen: %w", err)
	}
	return nil
}

func requireEnv(key string) (string, error) {
	v := os.Getenv(key)
	if v == "" {
		return "", fmt.Errorf("env %s is required", key)
	}
	return v, nil
}
