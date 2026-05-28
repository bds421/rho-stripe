package invoices

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/bds421/rho-stripe/meta"
)

// stderrSink is the destination for the package's last-resort
// orphan-hook-failed log line. Tests can swap this via the package
// helper SetStderrSink to capture output.
var stderrSink io.Writer = os.Stderr

// SetStderrSink replaces the package's last-resort logging destination
// (default: os.Stderr). Intended for tests; production code should not
// call this.
func SetStderrSink(w io.Writer) { stderrSink = w }

// GetStderrSink returns the current sink so tests can save + restore.
func GetStderrSink() io.Writer { return stderrSink }

// Operations is the public surface apps use for invoice creation +
// management. Constructed via New; connector facade exposes as
// conn.Invoices.
type Operations struct {
	backend   Backend
	numbers   NumberRepo // optional; when set, Next() drives NumberOverride
	namespace string     // app_namespace stamped onto every created Stripe object
	onOrphan  OrphanHook // optional; observability hook for orphaned invoice numbers
}

// OrphanHook is invoked when the number-ledger reaches a state the
// connector cannot resolve atomically: either MarkVoided failed after
// a Stripe error (number stuck in "issued"), or MarkUsed failed after
// a Stripe success (Stripe invoice exists but ledger doesn't know it).
//
// Apps wire this to alerting / a recovery queue. Returning an error
// from the hook is logged but does not change the original CreateDraft
// outcome (Stripe state is already what it is).
//
// Reason values: "void_failed_after_stripe_error", "used_failed_after_stripe_success".
type OrphanHook func(ctx context.Context, info OrphanInfo) error

// OrphanInfo describes a number-ledger orphan.
type OrphanInfo struct {
	Number          string
	Reason          string // see OrphanHook docstring
	StripeInvoiceID string // empty when Stripe call failed
	LedgerError     error  // the MarkUsed/MarkVoided error
	StripeError     error  // populated only for "void_failed_after_stripe_error"
}

// New wraps a Backend in an Operations facade.
func New(backend Backend, opts ...Option) *Operations {
	if backend == nil {
		panic("invoices.New: backend is required")
	}
	o := &Operations{backend: backend}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// Option configures Operations at construction time.
type Option func(*Operations)

// WithNumberRepo wires an app-supplied invoice-number issuer.
// Required for jurisdictions where the app must own sequential
// invoice numbering (Austrian §11 UStG and similar). When set, the
// connector calls NumberRepo.Next before each draft creation and
// fills CreateInput.NumberOverride from the result.
//
// The repo's MarkUsed is called on Stripe success; MarkVoided on
// Stripe error so the issued number is recorded as intentionally
// skipped.
func WithNumberRepo(r NumberRepo) Option {
	return func(o *Operations) { o.numbers = r }
}

// WithNamespace sets the app_namespace value stamped onto every
// Stripe object the connector creates through this Operations
// facade. The connector facade always passes this; bare-construction
// callers (tests) can omit it and the stamp becomes a no-op.
func WithNamespace(ns string) Option {
	return func(o *Operations) { o.namespace = ns }
}

// WithOrphanHook registers a hook invoked when the number-ledger
// reaches an unresolvable state (see [OrphanHook] for the reasons).
// Without a hook, orphans are still logged but apps lose the ability
// to alert / replay; wiring the hook is strongly recommended whenever
// WithNumberRepo is set.
func WithOrphanHook(h OrphanHook) Option {
	return func(o *Operations) { o.onOrphan = h }
}

func (o *Operations) stampNamespace(in map[string]string) map[string]string {
	return meta.StampNamespace(in, o.namespace)
}

// CreateDraft creates an editable draft invoice. Returns the draft
// for review; call Finalize or FinalizeAndSend to make it billable.
//
// When Operations was constructed with WithNumberRepo AND the caller
// didn't pre-fill NumberOverride, the repo's Next is invoked to
// allocate a number BEFORE the Stripe call. On Stripe success the
// number is MarkUsed; on Stripe error MarkVoided is called so the
// number is recorded as intentionally skipped (gapless audit trail).
//
// NumberPrefix selects the per-prefix sequence to draw from when
// auto-allocation is active. Apps typically encode jurisdiction +
// year (e.g. "AT-2026-").
func (o *Operations) CreateDraft(ctx context.Context, in CreateInput) (Invoice, error) {
	if in.Customer == "" {
		return Invoice{}, errors.New("invoices: CreateInput.Customer is required")
	}
	if len(in.LineItems) == 0 {
		return Invoice{}, errors.New("invoices: CreateInput.LineItems is required")
	}
	for i, li := range in.LineItems {
		if li.Amount <= 0 {
			return Invoice{}, fmt.Errorf("invoices: LineItems[%d].Amount must be positive", i)
		}
		if li.Currency == "" {
			return Invoice{}, fmt.Errorf("invoices: LineItems[%d].Currency is required", i)
		}
	}

	allocatedNumber := ""
	if o.numbers != nil && in.NumberOverride == "" {
		num, err := o.numbers.Next(ctx, in.NumberPrefix)
		if err != nil {
			return Invoice{}, fmt.Errorf("invoices.CreateDraft: NumberRepo.Next: %w", err)
		}
		in.NumberOverride = num
		allocatedNumber = num
	}
	in.Metadata = o.stampNamespace(in.Metadata)

	inv, err := o.backend.CreateDraft(ctx, in)
	if err != nil {
		if allocatedNumber != "" {
			if voidErr := o.numbers.MarkVoided(ctx, allocatedNumber, "stripe CreateDraft failed: "+err.Error()); voidErr != nil {
				// The number is now orphaned in "issued" state — the
				// next Next() call won't reuse it (counter advanced),
				// breaking the gapless invariant. Notify the orphan hook
				// + log; the original Stripe error still flows up to
				// the caller as the primary failure.
				o.reportOrphan(ctx, OrphanInfo{
					Number:      allocatedNumber,
					Reason:      "void_failed_after_stripe_error",
					LedgerError: voidErr,
					StripeError: err,
				})
			}
		}
		return Invoice{}, err
	}
	if allocatedNumber != "" {
		if markErr := o.numbers.MarkUsed(ctx, allocatedNumber, inv.StripeID); markErr != nil {
			// Stripe succeeded; the ledger doesn't know the mapping.
			// Apps need both pieces of info (the issued number AND the
			// Stripe id) for compliance audits, so we surface a hard
			// error AND fire the orphan hook so apps can record the
			// mapping out-of-band.
			o.reportOrphan(ctx, OrphanInfo{
				Number:          allocatedNumber,
				Reason:          "used_failed_after_stripe_success",
				StripeInvoiceID: inv.StripeID,
				LedgerError:     markErr,
			})
			return inv, fmt.Errorf("invoices.CreateDraft: number %q assigned to %q but MarkUsed failed: %w",
				allocatedNumber, inv.StripeID, markErr)
		}
	}
	return inv, nil
}

// reportOrphan invokes the OrphanHook if registered. Hook errors are
// logged via stderr formatting (the orphan info already captures the
// underlying cause); the connector deliberately doesn't accept a
// logger here to keep the package dependency-free of slog.
func (o *Operations) reportOrphan(ctx context.Context, info OrphanInfo) {
	if o.onOrphan == nil {
		return
	}
	if err := o.onOrphan(ctx, info); err != nil {
		// Hook itself failed. Nothing further we can do; the orphan
		// info plus the hook error are both part of the audit trail
		// the operator needs to reconcile. Apps that need persistent
		// orphan logging should make the hook itself persist before
		// returning.
		fmt.Fprintf(stderrSink, "invoices: OrphanHook failed: %v (orphan: %+v)\n", err, info)
	}
}

// Finalize transitions a draft to open (immutable, payable, not yet
// sent to the customer).
func (o *Operations) Finalize(ctx context.Context, invoiceID string) (Invoice, error) {
	if invoiceID == "" {
		return Invoice{}, errors.New("invoices: invoiceID is required")
	}
	return o.backend.Finalize(ctx, invoiceID)
}

// FinalizeAndSend transitions a draft to open AND triggers Stripe to
// email the customer the hosted invoice link. The customer-facing
// flow for net-30 invoices.
func (o *Operations) FinalizeAndSend(ctx context.Context, invoiceID string) (Invoice, error) {
	if invoiceID == "" {
		return Invoice{}, errors.New("invoices: invoiceID is required")
	}
	return o.backend.FinalizeAndSend(ctx, invoiceID)
}

// Void cancels an open invoice that should not be paid. Stripe-side
// status flips to "void"; the invoice number is preserved (compliance).
func (o *Operations) Void(ctx context.Context, invoiceID string) error {
	if invoiceID == "" {
		return errors.New("invoices: invoiceID is required")
	}
	return o.backend.Void(ctx, invoiceID)
}

// MarkUncollectible marks an unpaid invoice as a write-off. Common
// for net-30 invoices the customer never paid after dunning.
func (o *Operations) MarkUncollectible(ctx context.Context, invoiceID string) error {
	if invoiceID == "" {
		return errors.New("invoices: invoiceID is required")
	}
	return o.backend.MarkUncollectible(ctx, invoiceID)
}

// IssueCreditNote issues a credit note (partial or full) against a
// finalized invoice. EU jurisdictions require credit notes for any
// refund; this is the compliant path.
func (o *Operations) IssueCreditNote(ctx context.Context, in CreditNoteInput) error {
	if in.InvoiceID == "" {
		return errors.New("invoices: CreditNoteInput.InvoiceID is required")
	}
	if len(in.Lines) == 0 {
		return errors.New("invoices: CreditNoteInput.Lines is required")
	}
	return o.backend.IssueCreditNote(ctx, in)
}

// ListOverdue returns every open invoice past its due date. Apps use
// this to drive dunning email schedules.
func (o *Operations) ListOverdue(ctx context.Context) ([]Invoice, error) {
	return o.backend.ListOverdue(ctx)
}

// ListByCustomer returns the customer's invoices, most-recent first.
// Status (optional: "open" | "paid" | "void" | "uncollectible" |
// "draft") restricts to that single status. Limit caps the result
// count; 0 means default (50).
//
// For pagination beyond `limit` use ListByCustomerPage.
func (o *Operations) ListByCustomer(ctx context.Context, customerID StripeCustomerID, status string, limit int) ([]Invoice, error) {
	if customerID == "" {
		return nil, errors.New("invoices.ListByCustomer: customerID is required")
	}
	return o.backend.ListByCustomer(ctx, customerID, status, limit)
}

// ListByCustomerPage returns one page of invoices plus a cursor for
// the next. Apps that may need more than `limit` invoices (active
// enterprise customers with monthly invoices over many years) call
// this in a loop:
//
//	req := invoices.InvoicePageRequest{CustomerID: id, Limit: 100}
//	for {
//	    page, err := conn.Invoices.ListByCustomerPage(ctx, req)
//	    if err != nil { return err }
//	    handle(page.Invoices)
//	    if !page.HasMore { break }
//	    req.StartingAfter = page.LastID
//	}
func (o *Operations) ListByCustomerPage(ctx context.Context, req InvoicePageRequest) (InvoicePage, error) {
	if req.CustomerID == "" {
		return InvoicePage{}, errors.New("invoices.ListByCustomerPage: CustomerID is required")
	}
	return o.backend.ListByCustomerPage(ctx, req)
}
