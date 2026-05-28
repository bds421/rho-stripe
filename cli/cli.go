// Package cli wires the catalog sync workflow behind a tiny
// subcommand interface that consuming apps embed in their own
// cmd/sync/main.go:
//
//	func main() { cli.Main(billing.Catalog) }
//
// Supported subcommands (default is "diff"):
//
//	diff            Print the sync plan without applying.
//	sync --apply    Apply the plan to Stripe.
//	verify-account  Quick health check: call Accounts.Get against
//	                the configured key to confirm reachability.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/bds421/rho-kit/data/v2/idempotency"
	"github.com/bds421/rho-stripe/catalog"
	"github.com/bds421/rho-stripe/checkout"
	"github.com/bds421/rho-stripe/coupons"
	"github.com/bds421/rho-stripe/invoices"
	"github.com/bds421/rho-stripe/stripeapi"
	"github.com/bds421/rho-stripe/subscriptions"
	"github.com/bds421/rho-stripe/webhooks"
)

// Options configures Run; defaults to os.Args / os.Stdout / os.Stderr /
// STRIPE_SECRET_KEY when used via Main.
type Options struct {
	Args      []string
	Stdout    io.Writer
	Stderr    io.Writer
	SecretKey string

	// BackendFor builds the catalog.Backend used by diff/sync. nil
	// uses the default stripeapi-backed implementation. Tests pass a
	// fake to avoid hitting Stripe.
	BackendFor func(secretKey string) catalog.Backend

	// PriceListerFor builds the catalog.PriceLister used by checkout
	// (for cache warmup). nil uses the default stripeapi backend.
	PriceListerFor func(secretKey string) catalog.PriceLister

	// CheckoutBackendFor builds the checkout.Backend used by the
	// checkout/portal subcommands. nil uses the default
	// stripeapi-backed implementation.
	CheckoutBackendFor func(secretKey string) checkout.Backend

	// SubscriptionBackendFor builds the subscriptions.Backend used by
	// the `subs` subcommand. nil uses the default stripeapi backend.
	SubscriptionBackendFor func(secretKey string) subscriptions.Backend

	// EventLogFactory builds the webhooks.EventLog used by the
	// `webhook replay` subcommand. Apps wire their real (Postgres-
	// backed) log here so replay can read events the live server
	// persisted. Nil disables `webhook replay`.
	EventLogFactory func() webhooks.EventLog

	// HandlersFactory builds the same webhooks.Handlers registry the
	// live server uses so replayed events run through the same code
	// paths. Nil disables `webhook replay`.
	HandlersFactory func() webhooks.Handlers
}

func (o Options) replayStore() idempotency.Store {
	// Replay always uses a fresh in-memory store: the replay path
	// bypasses dedup (we WANT the handler to run for the replayed id),
	// and using the production Store could pollute its dedup record.
	return idempotency.NewMemoryStore()
}

// Main is the convenience entrypoint apps invoke from cmd/sync/main.go.
// It reads from os.Args, the standard streams, and STRIPE_SECRET_KEY,
// then exits with the code Run returns.
func Main(spec *catalog.Spec) {
	os.Exit(Run(spec, Options{
		Args:      os.Args[1:],
		Stdout:    os.Stdout,
		Stderr:    os.Stderr,
		SecretKey: os.Getenv("STRIPE_SECRET_KEY"),
	}))
}

// Run dispatches a subcommand and returns its exit code. Stable for
// testing; pass a fake BackendFor to test sync flows without hitting
// Stripe.
func Run(spec *catalog.Spec, opts Options) int {
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	if opts.BackendFor == nil {
		opts.BackendFor = defaultBackendFor
	}

	if len(opts.Args) == 0 {
		opts.Args = []string{"diff"}
	}
	cmd, rest := opts.Args[0], opts.Args[1:]

	switch cmd {
	case "diff":
		return runDiff(spec, rest, opts)
	case "sync":
		return runSync(spec, rest, opts)
	case "verify-account":
		return runVerifyAccount(opts)
	case "checkout":
		return runCheckout(spec, rest, opts)
	case "portal":
		return runPortal(spec, rest, opts)
	case "drift-check":
		return runDriftCheck(spec, rest, opts)
	case "subs":
		return runSubs(spec, rest, opts)
	case "webhook":
		return runWebhook(spec, rest, opts)
	case "invoice":
		return runInvoice(spec, rest, opts)
	case "coupon":
		return runCoupon(spec, rest, opts)
	case "gdpr":
		return runGDPR(spec, rest, opts)
	case "tax-id":
		return runTaxID(spec, rest, opts)
	case "customer":
		return runCustomer(spec, rest, opts)
	case "completion":
		return runCompletion(rest, opts)
	case "help", "-h", "--help":
		printHelp(opts.Stderr)
		return 0
	default:
		fmt.Fprintf(opts.Stderr, "unknown subcommand %q\n\n", cmd)
		printHelp(opts.Stderr)
		return 2
	}
}

func runDiff(spec *catalog.Spec, args []string, opts Options) int {
	if err := requireKey(opts); err != nil {
		fmt.Fprintln(opts.Stderr, err)
		return 1
	}
	_ = flag.NewFlagSet("diff", flag.ContinueOnError).Parse(args)

	backend := opts.BackendFor(opts.SecretKey)
	current, err := backend.ListProductsByNamespace(context.Background(), spec.Namespace)
	if err != nil {
		fmt.Fprintf(opts.Stderr, "list products: %v\n", err)
		return 1
	}
	plan := catalog.Diff(spec, current)
	fmt.Fprint(opts.Stdout, plan.String())
	return 0
}

func runSync(spec *catalog.Spec, args []string, opts Options) int {
	if err := requireKey(opts); err != nil {
		fmt.Fprintln(opts.Stderr, err)
		return 1
	}
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "actually execute the plan (default: dry-run print only)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	backend := opts.BackendFor(opts.SecretKey)
	ctx := context.Background()
	current, err := backend.ListProductsByNamespace(ctx, spec.Namespace)
	if err != nil {
		fmt.Fprintf(opts.Stderr, "list products: %v\n", err)
		return 1
	}
	plan := catalog.Diff(spec, current)
	fmt.Fprint(opts.Stdout, plan.String())

	if !*apply {
		fmt.Fprintln(opts.Stdout, "(dry-run; pass --apply to execute)")
		return 0
	}
	if plan.Empty() {
		return 0
	}
	if err := catalog.Apply(ctx, backend, plan); err != nil {
		fmt.Fprintf(opts.Stderr, "apply failed: %v\n", err)
		return 1
	}
	fmt.Fprintln(opts.Stdout, "applied.")
	return 0
}

func runCheckout(spec *catalog.Spec, args []string, opts Options) int {
	if err := requireKey(opts); err != nil {
		fmt.Fprintln(opts.Stderr, err)
		return 1
	}
	fs := flag.NewFlagSet("checkout", flag.ContinueOnError)
	subject := fs.String("subject", "cli_demo", "subject id (org/tenant/user)")
	success := fs.String("success", "https://example.com/success", "success redirect URL (hosted mode)")
	cancel := fs.String("cancel", "https://example.com/cancel", "cancel redirect URL (hosted mode)")
	promo := fs.String("promo", "", "optional promo code (currently ignored on apply; user can enter on hosted page)")
	embed := fs.Bool("embed", false, "use embedded Payment Element (returns ClientSecret instead of hosted URL)")
	returnURL := fs.String("return", "https://example.com/return?cs={CHECKOUT_SESSION_ID}", "return URL (embedded mode only)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(opts.Stderr, "usage: checkout [flags] <price_key> [<price_key>...]")
		fmt.Fprintln(opts.Stderr, "example: checkout pro_plan.monthly_eur")
		return 2
	}

	ctx := context.Background()
	cache := catalog.NewCache(opts.priceListerFor(opts.SecretKey))
	if err := cache.Warm(ctx, spec); err != nil {
		fmt.Fprintf(opts.Stderr, "warm cache: %v\n", err)
		return 1
	}

	repo := checkout.NewMemoryCustomerRepo()
	ck := checkout.New(checkout.Config{
		Namespace: spec.Namespace,
		Spec:      spec,
		Resolver:  cache,
		Customers: repo,
		Backend:   opts.checkoutBackendFor(opts.SecretKey),
	})

	items := make([]checkout.LineItem, 0, fs.NArg())
	for _, k := range fs.Args() {
		items = append(items, checkout.LineItem{PriceKey: k})
	}

	in := checkout.Input{
		SubjectID:   checkout.SubjectID(*subject),
		LineItems: items,
		PromoCode: *promo,
	}
	if *embed {
		in.UIMode = checkout.UIModeEmbedded
		in.ReturnURL = *returnURL
	} else {
		in.SuccessURL = *success
		in.CancelURL = *cancel
	}

	sess, err := ck.CreateSession(ctx, in)
	if err != nil {
		fmt.Fprintf(opts.Stderr, "create checkout: %v\n", err)
		return 1
	}
	fmt.Fprintf(opts.Stdout, "session:  %s\n", sess.ID)
	if *embed {
		fmt.Fprintf(opts.Stdout, "client_secret: %s\n", sess.ClientSecret)
		fmt.Fprintln(opts.Stdout, "Pass client_secret to Stripe.js initEmbeddedCheckout({clientSecret}) in your frontend.")
	} else {
		fmt.Fprintf(opts.Stdout, "url:      %s\n", sess.URL)
		fmt.Fprintln(opts.Stdout, "Open the URL in a browser; complete payment with test card 4242 4242 4242 4242 (any future expiry, any CVC).")
	}
	if custID, ok, _ := repo.Get(ctx, checkout.SubjectID(*subject)); ok {
		fmt.Fprintf(opts.Stdout, "customer: %s (pass to `portal --customer ...`)\n", custID)
	}
	return 0
}

func runPortal(spec *catalog.Spec, args []string, opts Options) int {
	if err := requireKey(opts); err != nil {
		fmt.Fprintln(opts.Stderr, err)
		return 1
	}
	fs := flag.NewFlagSet("portal", flag.ContinueOnError)
	customerID := fs.String("customer", "", "Stripe Customer ID (cus_…) to open the portal for (required)")
	returnURL := fs.String("return", "https://example.com/billing", "where Stripe sends the customer after they finish")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *customerID == "" {
		fmt.Fprintln(opts.Stderr, "missing --customer cus_…")
		fmt.Fprintln(opts.Stderr, "(the demo CLI doesn't persist subject↔customer across runs; pass the cus_ id directly from your dashboard or from a prior `checkout` run's logs)")
		return 2
	}

	ctx := context.Background()

	// Pre-seed an in-memory CustomerRepo so the Checkout API path works
	// without a persistent store. Real apps use their own repo.
	repo := checkout.NewMemoryCustomerRepo()
	const demoSubject = checkout.SubjectID("cli_demo")
	_ = repo.Upsert(ctx, demoSubject, checkout.StripeCustomerID(*customerID))

	ck := checkout.New(checkout.Config{
		Namespace: spec.Namespace,
		Spec:      spec,
		Resolver:  noopResolver{}, // portal doesn't need price resolution
		Customers: repo,
		Backend:   opts.checkoutBackendFor(opts.SecretKey),
	})

	sess, err := ck.CreatePortalSession(ctx, checkout.PortalInput{
		SubjectID:   demoSubject,
		ReturnURL: *returnURL,
	})
	if err != nil {
		fmt.Fprintf(opts.Stderr, "create portal: %v\n", err)
		return 1
	}
	fmt.Fprintf(opts.Stdout, "session: %s\n", sess.ID)
	fmt.Fprintf(opts.Stdout, "url:     %s\n", sess.URL)
	fmt.Fprintln(opts.Stdout, "Open the URL in a browser; the customer can manage their subscriptions, payment methods, and invoices.")
	return 0
}

// noopResolver satisfies checkout.PriceResolver for code paths that
// don't need price resolution (currently: portal sessions).
type noopResolver struct{}

func (noopResolver) Lookup(_ string) (string, bool) { return "", false }

func runVerifyAccount(opts Options) int {
	if err := requireKey(opts); err != nil {
		fmt.Fprintln(opts.Stderr, err)
		return 1
	}
	sc := stripeapi.NewClient(stripeapi.Config{SecretKey: opts.SecretKey})
	acct, err := sc.V1Accounts.Retrieve(context.Background(), nil)
	if err != nil {
		fmt.Fprintf(opts.Stderr, "Accounts.Retrieve: %v\n", err)
		return 1
	}
	fmt.Fprintf(opts.Stdout, "ok — account=%s country=%s currency=%s\n",
		acct.ID, acct.Country, acct.DefaultCurrency)
	return 0
}

// runDriftCheck calls catalog.CheckDrift and prints the human-formatted
// report. Exit codes:
//
//	0 — no drift (or --no-fail given)
//	1 — drift detected (use to gate CI/CD)
//	2 — flag / config error
func runDriftCheck(spec *catalog.Spec, args []string, opts Options) int {
	if err := requireKey(opts); err != nil {
		fmt.Fprintln(opts.Stderr, err)
		return 1
	}
	fs := flag.NewFlagSet("drift-check", flag.ContinueOnError)
	noFail := fs.Bool("no-fail", false, "exit 0 even when drift is detected (still prints the report)")
	autoApply := fs.Bool("apply", false, "if drift is detected, re-check and apply it (be careful — this writes to Stripe)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	backend := opts.BackendFor(opts.SecretKey)
	rep, err := catalog.CheckDrift(context.Background(), backend, spec)
	if err != nil {
		fmt.Fprintf(opts.Stderr, "drift-check: %v\n", err)
		return 1
	}
	if !rep.HasDrift {
		fmt.Fprintln(opts.Stdout, "no drift")
		return 0
	}
	fmt.Fprint(opts.Stdout, rep.FormatHuman())
	if *autoApply {
		fresh, err := catalog.CheckDrift(context.Background(), backend, spec)
		if err != nil {
			fmt.Fprintf(opts.Stderr, "drift-check: re-check before apply: %v\n", err)
			return 1
		}
		if err := fresh.Apply(context.Background(), backend); err != nil {
			fmt.Fprintf(opts.Stderr, "drift-check: apply: %v\n", err)
			return 1
		}
		fmt.Fprintln(opts.Stdout, "drift applied.")
		return 0
	}
	if *noFail {
		return 0
	}
	return 1
}

// runSubs dispatches the `subs` family of subcommands.
func runSubs(spec *catalog.Spec, args []string, opts Options) int {
	if len(args) == 0 {
		fmt.Fprintln(opts.Stderr, "usage: subs <schedule|seats> [args]")
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "schedule":
		return runSubsSchedule(spec, rest, opts)
	case "seats":
		return runSubsSeats(spec, rest, opts)
	default:
		fmt.Fprintf(opts.Stderr, "unknown subs subcommand %q\n", cmd)
		return 2
	}
}

func runSubsSchedule(spec *catalog.Spec, args []string, opts Options) int {
	if err := requireKey(opts); err != nil {
		fmt.Fprintln(opts.Stderr, err)
		return 1
	}
	fs := flag.NewFlagSet("subs schedule", flag.ContinueOnError)
	customer := fs.String("customer", "", "Stripe Customer ID (cus_…)")
	phasesArg := fs.String("phases", "", "comma-separated phases: priceKey[:iterations[:couponKey]] (e.g. pro_plan.monthly_eur:3:SAVE20,pro_plan.monthly_eur)")
	end := fs.String("end-behavior", "release", "Stripe end_behavior: release | cancel")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *customer == "" || *phasesArg == "" {
		fmt.Fprintln(opts.Stderr, "usage: subs schedule --customer cus_… --phases 'pro.monthly_eur:3:SAVE20,pro.monthly_eur'")
		return 2
	}

	ctx := context.Background()
	cache := catalog.NewCache(opts.priceListerFor(opts.SecretKey))
	if err := cache.Warm(ctx, spec); err != nil {
		fmt.Fprintf(opts.Stderr, "warm cache: %v\n", err)
		return 1
	}

	phases, err := parseSchedulePhases(*phasesArg)
	if err != nil {
		fmt.Fprintf(opts.Stderr, "bad --phases: %v\n", err)
		return 2
	}

	subOps := subscriptions.New(subscriptions.Config{Backend: opts.subscriptionBackendFor(opts.SecretKey), Repo: subscriptions.NewMemoryRepo(), Spec: spec, Cache: cache})
	id, err := subOps.CreateSchedule(ctx, subscriptions.ScheduleInput{
		StripeCustomerID: *customer,
		Phases:           phases,
		EndBehavior:      *end,
	})
	if err != nil {
		fmt.Fprintf(opts.Stderr, "create schedule: %v\n", err)
		return 1
	}
	if id == nil {
		// Defensive: contract is non-nil on err == nil. A misbehaving
		// backend reaching this point would otherwise nil-deref below.
		fmt.Fprintln(opts.Stderr, "create schedule: backend returned nil schedule")
		return 1
	}
	fmt.Fprintf(opts.Stdout, "schedule: %s\n", id.StripeID)
	return 0
}

func parseSchedulePhases(arg string) ([]subscriptions.SchedulePhase, error) {
	if arg == "" {
		return nil, fmt.Errorf("empty phase list")
	}
	var out []subscriptions.SchedulePhase
	for i, raw := range splitCommas(arg) {
		parts := splitColons(raw)
		if len(parts) == 0 || parts[0] == "" {
			return nil, fmt.Errorf("phase[%d]: empty price key", i)
		}
		ph := subscriptions.SchedulePhase{PriceKey: parts[0]}
		if len(parts) >= 2 && parts[1] != "" {
			n, err := atoi(parts[1])
			if err != nil {
				return nil, fmt.Errorf("phase[%d]: bad iterations %q: %w", i, parts[1], err)
			}
			ph.Iterations = n
		}
		if len(parts) >= 3 {
			ph.CouponKey = parts[2]
		}
		out = append(out, ph)
	}
	return out, nil
}

func splitCommas(s string) []string { return splitOn(s, ',') }
func splitColons(s string) []string { return splitOn(s, ':') }

func splitOn(s string, sep byte) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func atoi(s string) (int, error) {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

func runSubsSeats(spec *catalog.Spec, args []string, opts Options) int {
	if err := requireKey(opts); err != nil {
		fmt.Fprintln(opts.Stderr, err)
		return 1
	}
	fs := flag.NewFlagSet("subs seats", flag.ContinueOnError)
	action := fs.String("action", "get", "get | set | add")
	subject := fs.String("subject", "cli_demo", "subject id (org/tenant)")
	subID := fs.String("sub", "", "Stripe Subscription ID (sub_…)")
	priceKey := fs.String("price", "", "catalog-relative price key for the seat product")
	n := fs.Int64("n", 0, "quantity (for set) / delta (for add)")
	prorate := fs.Bool("prorate", true, "let Stripe pro-rate mid-cycle adjustments")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *subID == "" || *priceKey == "" {
		fmt.Fprintln(opts.Stderr, "usage: subs seats --sub sub_… --price pro.per_seat_eur --action {get|set|add} [--n N]")
		return 2
	}

	ctx := context.Background()
	cache := catalog.NewCache(opts.priceListerFor(opts.SecretKey))
	if err := cache.Warm(ctx, spec); err != nil {
		fmt.Fprintf(opts.Stderr, "warm cache: %v\n", err)
		return 1
	}
	subOps := subscriptions.New(subscriptions.Config{Backend: opts.subscriptionBackendFor(opts.SecretKey), Repo: subscriptions.NewMemoryRepo(), Spec: spec, Cache: cache})

	subj := subscriptions.SubjectID(*subject)
	switch *action {
	case "get":
		count, err := subOps.SeatCount(ctx, subj, *subID, *priceKey)
		if err != nil {
			fmt.Fprintf(opts.Stderr, "seats get: %v\n", err)
			return 1
		}
		fmt.Fprintf(opts.Stdout, "seats: %d\n", count)
	case "set":
		if *n <= 0 {
			fmt.Fprintln(opts.Stderr, "--n must be > 0 for set")
			return 2
		}
		if err := subOps.SetSeats(ctx, subj, *subID, *priceKey, *n, *prorate); err != nil {
			fmt.Fprintf(opts.Stderr, "seats set: %v\n", err)
			return 1
		}
		fmt.Fprintf(opts.Stdout, "set seats to %d\n", *n)
	case "add":
		if err := subOps.AddSeats(ctx, subj, *subID, *priceKey, *n, *prorate); err != nil {
			fmt.Fprintf(opts.Stderr, "seats add: %v\n", err)
			return 1
		}
		fmt.Fprintf(opts.Stdout, "added %d seats\n", *n)
	default:
		fmt.Fprintf(opts.Stderr, "unknown --action %q\n", *action)
		return 2
	}
	return 0
}

// runWebhook dispatches the `webhook` family. Only "replay" exists
// today (re-runs a previously-logged event through the dispatcher).
// Needs an EventLogFactory injected via Options because the CLI is
// stateless: by default it constructs a no-op EventLog and reports
// "log not configured", so apps wanting to use replay against their
// real Postgres-backed log call cli.Run with Options.EventLogFactory
// set.
func runWebhook(spec *catalog.Spec, args []string, opts Options) int {
	_ = spec
	if len(args) == 0 {
		fmt.Fprintln(opts.Stderr, "usage: webhook replay <event-id>")
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "replay":
		return runWebhookReplay(rest, opts)
	default:
		fmt.Fprintf(opts.Stderr, "unknown webhook subcommand %q\n", cmd)
		return 2
	}
}

func runWebhookReplay(args []string, opts Options) int {
	if len(args) == 0 {
		fmt.Fprintln(opts.Stderr, "usage: webhook replay <event-id>")
		return 2
	}
	if opts.EventLogFactory == nil {
		fmt.Fprintln(opts.Stderr, "webhook replay: Options.EventLogFactory not set; apps must wire it (typically to a postgres-backed log)")
		return 2
	}
	if opts.HandlersFactory == nil {
		fmt.Fprintln(opts.Stderr, "webhook replay: Options.HandlersFactory not set; apps must provide the same handler registry the live server uses")
		return 2
	}
	eventID := args[0]
	wh := webhooks.New(webhooks.Config{
		SigningSecret: "whsec_replay_dummy", // replay path bypasses signature verification
		Store:         opts.replayStore(),
		Handlers:      opts.HandlersFactory(),
		EventLog:      opts.EventLogFactory(),
	})
	if err := wh.ReplayEvent(context.Background(), eventID); err != nil {
		fmt.Fprintf(opts.Stderr, "replay %s: %v\n", eventID, err)
		return 1
	}
	fmt.Fprintf(opts.Stdout, "replayed %s\n", eventID)
	return 0
}

// runInvoice dispatches the `invoice` family. Today only `create`
// exists.
func runInvoice(_ *catalog.Spec, args []string, opts Options) int {
	if len(args) == 0 {
		fmt.Fprintln(opts.Stderr, "usage: invoice create --customer cus_… --amount N --currency eur [--description … --finalize --send]")
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "create":
		return runInvoiceCreate(rest, opts)
	default:
		fmt.Fprintf(opts.Stderr, "unknown invoice subcommand %q\n", cmd)
		return 2
	}
}

func runInvoiceCreate(args []string, opts Options) int {
	if err := requireKey(opts); err != nil {
		fmt.Fprintln(opts.Stderr, err)
		return 1
	}
	fs := flag.NewFlagSet("invoice create", flag.ContinueOnError)
	customer := fs.String("customer", "", "Stripe Customer ID (cus_…)")
	amount := fs.Int64("amount", 0, "amount in smallest currency unit (e.g. 5000 = €50.00)")
	currency := fs.String("currency", "eur", "ISO 4217 (lowercase)")
	description := fs.String("description", "", "line-item description")
	dueDays := fs.Int("due", 30, "days until due (collection_method=send_invoice)")
	finalize := fs.Bool("finalize", false, "finalize the draft after creation")
	send := fs.Bool("send", false, "implies --finalize, and triggers Stripe to email the invoice link")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *customer == "" || *amount <= 0 {
		fmt.Fprintln(opts.Stderr, "missing required --customer and --amount > 0")
		return 2
	}

	sc := stripeapi.NewClient(stripeapi.Config{SecretKey: opts.SecretKey})
	ops := invoices.New(stripeapi.NewInvoiceBackend(sc))
	ctx := context.Background()
	inv, err := ops.CreateDraft(ctx, invoices.CreateInput{
		Customer:  invoices.StripeCustomerID(*customer),
		DueIn:     time.Duration(*dueDays) * 24 * time.Hour,
		LineItems: []invoices.CreateLineItem{{Description: *description, Amount: *amount, Currency: *currency, Quantity: 1}},
	})
	if err != nil {
		fmt.Fprintf(opts.Stderr, "create draft: %v\n", err)
		return 1
	}
	fmt.Fprintf(opts.Stdout, "draft: %s (status=%s)\n", inv.StripeID, inv.Status)

	if *send {
		out, err := ops.FinalizeAndSend(ctx, inv.StripeID)
		if err != nil {
			fmt.Fprintf(opts.Stderr, "finalize+send: %v\n", err)
			return 1
		}
		fmt.Fprintf(opts.Stdout, "sent: %s url=%s\n", out.StripeID, out.HostedInvoiceURL)
	} else if *finalize {
		out, err := ops.Finalize(ctx, inv.StripeID)
		if err != nil {
			fmt.Fprintf(opts.Stderr, "finalize: %v\n", err)
			return 1
		}
		fmt.Fprintf(opts.Stdout, "finalized: %s url=%s\n", out.StripeID, out.HostedInvoiceURL)
	}
	return 0
}

// runCoupon dispatches the `coupon` family — mint customer-facing
// promo codes referencing a catalog-declared Coupon.
func runCoupon(spec *catalog.Spec, args []string, opts Options) int {
	if len(args) == 0 {
		fmt.Fprintln(opts.Stderr, "usage: coupon promo --coupon-key SAVE20 --code WELCOME [--max N]")
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "promo":
		return runCouponPromo(spec, rest, opts)
	default:
		fmt.Fprintf(opts.Stderr, "unknown coupon subcommand %q\n", cmd)
		return 2
	}
}

func runCouponPromo(spec *catalog.Spec, args []string, opts Options) int {
	if err := requireKey(opts); err != nil {
		fmt.Fprintln(opts.Stderr, err)
		return 1
	}
	fs := flag.NewFlagSet("coupon promo", flag.ContinueOnError)
	couponKey := fs.String("coupon-key", "", "catalog-relative coupon key (must exist in the spec)")
	code := fs.String("code", "", "customer-facing code (e.g. WELCOME2026)")
	max := fs.Int64("max", 0, "MaxRedemptions; 0 = unlimited")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *couponKey == "" || *code == "" {
		fmt.Fprintln(opts.Stderr, "missing --coupon-key and/or --code")
		return 2
	}
	sc := stripeapi.NewClient(stripeapi.Config{SecretKey: opts.SecretKey})
	ops := coupons.New(coupons.Config{Backend: stripeapi.NewCouponBackend(sc), Spec: spec})
	in := coupons.CreateInput{
		CouponKey: *couponKey,
		Code:      *code,
	}
	if *max > 0 {
		in.MaxRedemptions = *max
	}
	pc, err := ops.CreatePromoCode(context.Background(), in)
	if err != nil {
		fmt.Fprintf(opts.Stderr, "create promo: %v\n", err)
		return 1
	}
	fmt.Fprintf(opts.Stdout, "promo: %s code=%s coupon=%s active=%v\n",
		pc.ID, pc.Code, pc.CouponID, pc.Active)
	return 0
}

func defaultSubscriptionBackendFor(secretKey string) subscriptions.Backend {
	sc := stripeapi.NewClient(stripeapi.Config{SecretKey: secretKey})
	return stripeapi.NewSubscriptionBackend(sc)
}

func (o Options) subscriptionBackendFor(secret string) subscriptions.Backend {
	if o.SubscriptionBackendFor != nil {
		return o.SubscriptionBackendFor(secret)
	}
	return defaultSubscriptionBackendFor(secret)
}

func defaultBackendFor(secretKey string) catalog.Backend {
	sc := stripeapi.NewClient(stripeapi.Config{SecretKey: secretKey})
	return stripeapi.NewBackend(sc)
}

func defaultPriceListerFor(secretKey string) catalog.PriceLister {
	sc := stripeapi.NewClient(stripeapi.Config{SecretKey: secretKey})
	return stripeapi.NewBackend(sc)
}

func defaultCheckoutBackendFor(secretKey string) checkout.Backend {
	sc := stripeapi.NewClient(stripeapi.Config{SecretKey: secretKey})
	return stripeapi.NewCheckoutBackend(sc)
}

// priceListerFor and checkoutBackendFor wrap the option fields with
// sensible defaults. Used by subcommand runners so tests can inject
// fakes via Options without runtime nil-checks scattered everywhere.
func (o Options) priceListerFor(secret string) catalog.PriceLister {
	if o.PriceListerFor != nil {
		return o.PriceListerFor(secret)
	}
	return defaultPriceListerFor(secret)
}

func (o Options) checkoutBackendFor(secret string) checkout.Backend {
	if o.CheckoutBackendFor != nil {
		return o.CheckoutBackendFor(secret)
	}
	return defaultCheckoutBackendFor(secret)
}

func requireKey(opts Options) error {
	if opts.SecretKey == "" {
		return fmt.Errorf("STRIPE_SECRET_KEY is not set")
	}
	return nil
}

func printHelp(w io.Writer) {
	fmt.Fprintln(w, `rho-stripe — manage the catalog declared in your app code.

Usage:
  rho-stripe <command> [flags]

Commands:
  diff                         Print the sync plan (default).
  sync --apply                 Apply the plan to Stripe. Without --apply, prints the plan and exits.
  verify-account               Hit Stripe Accounts.Get to confirm the key works.
  checkout [--embed] <key>     Create a Checkout Session (hosted by default; --embed returns ClientSecret).
  portal --customer cus_…      Create a Customer Portal session and print the URL.
  drift-check [--apply]        Compare Stripe state to spec; exit 1 on drift (unless --no-fail).
  subs schedule …              Create a multi-phase SubscriptionSchedule.
  subs seats …                 Get/set/add per-seat quantity on a subscription.
  webhook replay <event-id>    Replay an event from the EventLog through the handlers.
  invoice create …             Create (and optionally finalize/send) a draft invoice.
  coupon promo …               Mint a customer-facing promo code from a catalog coupon.
  gdpr export <subject>        Export every piece of data the lib + Stripe hold for a subject (Article 15).
  gdpr forget <subject>        Delete the Stripe customer + signal app to forget (Article 17).
  tax-id add --subject … --type eu_vat --value …
                               Attach a tax registration to the customer's Stripe record.
  tax-id list --subject …      Print all tax registrations for a subject.
  tax-id remove --subject … --id ti_…
                               Detach a tax registration.
  customer import --subject … --stripe-id cus_…
                               Backfill an existing Stripe customer into this namespace.
  completion bash|zsh          Emit a shell completion script.
  help                         Show this message.

Environment:
  STRIPE_SECRET_KEY  required for all commands except 'help' and 'completion'.`)
}

// --- GDPR / Tax ID / Customer subcommands ---

func runGDPR(spec *catalog.Spec, args []string, opts Options) int {
	if len(args) == 0 {
		fmt.Fprintln(opts.Stderr, "usage: gdpr export <subject> | gdpr forget <subject>")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "export":
		return runGDPRExport(spec, rest, opts)
	case "forget":
		return runGDPRForget(spec, rest, opts)
	default:
		fmt.Fprintf(opts.Stderr, "unknown gdpr subcommand %q\n", sub)
		return 2
	}
}

func runGDPRExport(spec *catalog.Spec, args []string, opts Options) int {
	if err := requireKey(opts); err != nil {
		fmt.Fprintln(opts.Stderr, err)
		return 1
	}
	if len(args) < 1 {
		fmt.Fprintln(opts.Stderr, "usage: gdpr export <subject>")
		return 2
	}
	subjectID := args[0]
	custOps, err := opts.customersOpsFor(opts.SecretKey)
	if err != nil {
		fmt.Fprintln(opts.Stderr, err)
		return 1
	}
	export, err := custOps.Export(context.Background(), customersSubjectID(subjectID))
	if err != nil {
		fmt.Fprintln(opts.Stderr, err)
		return 1
	}
	enc := jsonEncoder(opts.Stdout)
	if err := enc.Encode(export); err != nil {
		fmt.Fprintln(opts.Stderr, err)
		return 1
	}
	_ = spec
	return 0
}

func runGDPRForget(spec *catalog.Spec, args []string, opts Options) int {
	if err := requireKey(opts); err != nil {
		fmt.Fprintln(opts.Stderr, err)
		return 1
	}
	if len(args) < 1 {
		fmt.Fprintln(opts.Stderr, "usage: gdpr forget <subject>")
		return 2
	}
	subjectID := args[0]
	custOps, err := opts.customersOpsFor(opts.SecretKey)
	if err != nil {
		fmt.Fprintln(opts.Stderr, err)
		return 1
	}
	report, err := custOps.Forget(context.Background(), customersSubjectID(subjectID))
	if err != nil {
		fmt.Fprintln(opts.Stderr, err)
		return 1
	}
	enc := jsonEncoder(opts.Stdout)
	if err := enc.Encode(report); err != nil {
		fmt.Fprintln(opts.Stderr, err)
		return 1
	}
	_ = spec
	return 0
}

func runTaxID(spec *catalog.Spec, args []string, opts Options) int {
	if len(args) == 0 {
		fmt.Fprintln(opts.Stderr, "usage: tax-id add|list|remove …")
		return 2
	}
	sub, rest := args[0], args[1:]
	custOps, err := opts.customersOpsFor(opts.SecretKey)
	if err != nil {
		fmt.Fprintln(opts.Stderr, err)
		return 1
	}
	fs := flag.NewFlagSet("tax-id", flag.ContinueOnError)
	subject := fs.String("subject", "", "subject id")
	typ := fs.String("type", "", "tax-id type (e.g. eu_vat)")
	value := fs.String("value", "", "tax registration value (e.g. DE123456789)")
	id := fs.String("id", "", "tax id (ti_…) — required for remove")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if *subject == "" {
		fmt.Fprintln(opts.Stderr, "tax-id: --subject is required")
		return 2
	}
	switch sub {
	case "add":
		if *typ == "" || *value == "" {
			fmt.Fprintln(opts.Stderr, "tax-id add: --type and --value are required")
			return 2
		}
		tid, err := custOps.AddTaxID(context.Background(), customersAddTaxIDInput(*subject, *typ, *value))
		if err != nil {
			fmt.Fprintln(opts.Stderr, err)
			return 1
		}
		fmt.Fprintln(opts.Stdout, tid.StripeID)
		return 0
	case "list":
		tids, err := custOps.ListTaxIDs(context.Background(), customersSubjectID(*subject))
		if err != nil {
			fmt.Fprintln(opts.Stderr, err)
			return 1
		}
		for _, t := range tids {
			fmt.Fprintf(opts.Stdout, "%s\t%s\t%s\t%s\n", t.StripeID, t.Type, t.Value, t.Verification)
		}
		return 0
	case "remove":
		if *id == "" {
			fmt.Fprintln(opts.Stderr, "tax-id remove: --id is required")
			return 2
		}
		if err := custOps.RemoveTaxID(context.Background(), customersSubjectID(*subject), *id); err != nil {
			fmt.Fprintln(opts.Stderr, err)
			return 1
		}
		fmt.Fprintln(opts.Stdout, "ok")
		return 0
	default:
		fmt.Fprintf(opts.Stderr, "unknown tax-id subcommand %q\n", sub)
		return 2
	}
}

func runCustomer(spec *catalog.Spec, args []string, opts Options) int {
	if len(args) == 0 {
		fmt.Fprintln(opts.Stderr, "usage: customer import --subject … --stripe-id cus_…")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "import":
		fs := flag.NewFlagSet("customer import", flag.ContinueOnError)
		subject := fs.String("subject", "", "app subject id")
		stripeID := fs.String("stripe-id", "", "existing Stripe customer id (cus_…)")
		adopt := fs.Bool("adopt-namespace", false, "overwrite if customer is in a different namespace")
		skipSubs := fs.Bool("skip-subs", false, "don't backfill subscriptions")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if *subject == "" || *stripeID == "" {
			fmt.Fprintln(opts.Stderr, "customer import: --subject and --stripe-id are required")
			return 2
		}
		custOps, err := opts.customersOpsFor(opts.SecretKey)
		if err != nil {
			fmt.Fprintln(opts.Stderr, err)
			return 1
		}
		res, err := custOps.ImportCustomer(context.Background(), customersImportInput(*subject, *stripeID, spec.Namespace, *adopt, !*skipSubs))
		if err != nil {
			fmt.Fprintln(opts.Stderr, err)
			return 1
		}
		fmt.Fprintf(opts.Stdout, "imported subject=%s stripe=%s subs_backfilled=%d namespace_stamped=%t\n",
			res.SubjectID, res.StripeCustomerID, res.SubscriptionsBackfilled, res.NamespaceStamped)
		return 0
	default:
		fmt.Fprintf(opts.Stderr, "unknown customer subcommand %q\n", sub)
		return 2
	}
}

// runCompletion emits a shell completion script. Generated from the
// static command list above, so adding a subcommand only requires
// touching this string.
func runCompletion(args []string, opts Options) int {
	shell := "bash"
	if len(args) > 0 {
		shell = args[0]
	}
	switch shell {
	case "bash":
		fmt.Fprint(opts.Stdout, bashCompletionScript)
		return 0
	case "zsh":
		fmt.Fprint(opts.Stdout, zshCompletionScript)
		return 0
	default:
		fmt.Fprintf(opts.Stderr, "unknown shell %q (supported: bash, zsh)\n", shell)
		return 2
	}
}

const bashCompletionScript = `# rho-stripe bash completion
_stripe_connector_completion() {
    local cur="${COMP_WORDS[COMP_CWORD]}"
    local prev="${COMP_WORDS[COMP_CWORD-1]}"
    local cmds="diff sync verify-account checkout portal drift-check subs webhook invoice coupon gdpr tax-id customer completion help"
    if [ "$COMP_CWORD" -eq 1 ]; then
        COMPREPLY=($(compgen -W "$cmds" -- "$cur"))
        return
    fi
    case "$prev" in
        gdpr) COMPREPLY=($(compgen -W "export forget" -- "$cur"));;
        tax-id) COMPREPLY=($(compgen -W "add list remove" -- "$cur"));;
        customer) COMPREPLY=($(compgen -W "import" -- "$cur"));;
        subs) COMPREPLY=($(compgen -W "schedule seats" -- "$cur"));;
        webhook) COMPREPLY=($(compgen -W "replay" -- "$cur"));;
        invoice) COMPREPLY=($(compgen -W "create" -- "$cur"));;
        coupon) COMPREPLY=($(compgen -W "promo" -- "$cur"));;
        completion) COMPREPLY=($(compgen -W "bash zsh" -- "$cur"));;
    esac
}
complete -F _stripe_connector_completion rho-stripe
`

const zshCompletionScript = `#compdef rho-stripe
# rho-stripe zsh completion
_stripe_connector() {
    local -a commands subcommands
    commands=(
        'diff:Print the sync plan'
        'sync:Apply the plan to Stripe (--apply)'
        'verify-account:Confirm key works via Accounts.Get'
        'checkout:Create a Checkout Session'
        'portal:Create a Customer Portal session'
        'drift-check:Compare Stripe state to spec'
        'subs:Subscription operations (schedule, seats)'
        'webhook:Webhook ops (replay)'
        'invoice:Invoice ops (create)'
        'coupon:Coupon ops (promo)'
        'gdpr:GDPR export/forget (Article 15/17)'
        'tax-id:Tax registration management'
        'customer:Backfill existing Stripe customers'
        'completion:Emit shell completion script'
        'help:Show usage'
    )
    if (( CURRENT == 2 )); then
        _describe -t commands 'rho-stripe commands' commands
        return
    fi
    case "${words[2]}" in
        gdpr) subcommands=('export:Export Article-15 bundle' 'forget:Erase per Article 17')
              _describe -t subcommands 'gdpr commands' subcommands;;
        tax-id) subcommands=('add:Add a tax ID' 'list:List tax IDs' 'remove:Detach a tax ID')
                _describe -t subcommands 'tax-id commands' subcommands;;
        customer) subcommands=('import:Backfill existing customer'); _describe -t subcommands 'customer commands' subcommands;;
        completion) subcommands=('bash:Emit bash script' 'zsh:Emit zsh script'); _describe -t subcommands 'completion' subcommands;;
    esac
}
_stripe_connector "$@"
`
