// Command embedded_frontend is a minimal full-stack demo of the
// embedded Payment Element backed by rho-stripe.
//
// Run:
//
//	set -a; source .env; set +a
//	# Make sure saas_tiers catalog is synced first:
//	go run ./examples/saas_tiers/main sync --apply
//	go run ./examples/embedded_frontend
//
// Then open http://localhost:8090 in a browser.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bds421/rho-kit/data/v2/idempotency"
	"github.com/bds421/rho-stripe/checkout"
	"github.com/bds421/rho-stripe/connector"
	"github.com/bds421/rho-stripe/examples/saas_tiers"
	"github.com/bds421/rho-stripe/webhooks"
)

//go:embed index.html
var indexHTML string

func main() {
	secretKey := os.Getenv("STRIPE_SECRET_KEY")
	publishableKey := os.Getenv("STRIPE_PUBLISHABLE_KEY")
	if secretKey == "" {
		log.Fatal("STRIPE_SECRET_KEY env var is required")
	}
	if publishableKey == "" {
		log.Println("WARN: STRIPE_PUBLISHABLE_KEY not set; frontend will use a placeholder and Stripe.js will fail")
	}

	ctx := context.Background()
	spec := saas_tiers.Spec()
	conn, err := connector.New(ctx, connector.Config{
		SecretKey:     secretKey,
		WebhookSecret: "whsec_unused_in_this_demo",
		AppNamespace:  spec.Namespace,
		Catalog:       spec,
		Customers:     checkout.NewMemoryCustomerRepo(),
		Events:        idempotency.NewMemoryStore(),
		Handlers:      webhooks.Handlers{},
	})
	if err != nil {
		log.Fatalf("connector.New: %v", err)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		// Inject the publishable key into the page so the browser's
		// Stripe.js loader has the right account.
		page := strings.Replace(
			indexHTML,
			`window.STRIPE_PUBLISHABLE_KEY || 'pk_test_placeholder_replace_me'`,
			fmt.Sprintf("%q", publishableKey),
			1,
		)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	})

	mux.HandleFunc("/create-checkout-session", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		sess, err := conn.Checkout.CreateSession(r.Context(), checkout.Input{
			SubjectID:   "embedded_demo_subject",
			LineItems: []checkout.LineItem{{PriceKey: "standard.monthly_eur"}},
			UIMode:    checkout.UIModeEmbedded,
			ReturnURL: "http://localhost:8090/return?cs={CHECKOUT_SESSION_ID}",
		})
		if err != nil {
			log.Printf("create session: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"client_secret": sess.ClientSecret,
		})
	})

	mux.HandleFunc("/return", func(w http.ResponseWriter, r *http.Request) {
		cs := r.URL.Query().Get("cs")
		fmt.Fprintf(w, "Checkout session %s completed. (In a real app this page would render the receipt.)", cs)
	})

	srv := &http.Server{
		Addr:              ":8090",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Println("listening on http://localhost:8090 — open / in a browser, click Start checkout")
	log.Fatal(srv.ListenAndServe())
}
