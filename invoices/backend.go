package invoices

import "context"

// Lister is the Stripe-side surface the audit-log exporter depends
// on. Default implementation backed by stripe-go lives in the
// stripeapi package; tests inject fakes.
type Lister interface {
	// ListInvoices returns every invoice whose Created falls within
	// period. The lib's exporter wraps with date filtering; backends
	// only need to honor the provided range.
	ListInvoices(ctx context.Context, period Period) ([]AuditEntry, error)
}

// Backend extends Lister with the mutation surface needed by
// Operations (phase 6). Default implementation in stripeapi.
type Backend interface {
	Lister

	CreateDraft(ctx context.Context, in CreateInput) (Invoice, error)
	Finalize(ctx context.Context, invoiceID string) (Invoice, error)
	FinalizeAndSend(ctx context.Context, invoiceID string) (Invoice, error)
	Void(ctx context.Context, invoiceID string) error
	MarkUncollectible(ctx context.Context, invoiceID string) error
	IssueCreditNote(ctx context.Context, in CreditNoteInput) error
	ListOverdue(ctx context.Context) ([]Invoice, error)

	// ListByCustomer returns the customer's invoices in reverse
	// chronological order. Apps use this to power "Billing history"
	// pages without reaching into the raw Stripe SDK. Limit caps the
	// returned count (defaults to 50 when 0). status, when non-empty,
	// filters by Stripe invoice status ("draft", "open", "paid",
	// "void", "uncollectible").
	ListByCustomer(ctx context.Context, customerID StripeCustomerID, status string, limit int) ([]Invoice, error)

	// ListByCustomerPage returns a single page of invoices plus the
	// id of the last item (use as StartingAfter for the next call to
	// page deeper). HasMore reports whether more pages exist beyond
	// this one. Apps that need to iterate over more than `limit`
	// invoices use this instead of ListByCustomer.
	ListByCustomerPage(ctx context.Context, page InvoicePageRequest) (InvoicePage, error)
}

// InvoicePageRequest configures pagination.
type InvoicePageRequest struct {
	CustomerID    StripeCustomerID
	Status        string // optional filter
	Limit         int    // 1..100; 0 → defaults to 50
	StartingAfter string // Stripe invoice id; empty = first page
}

// InvoicePage is one page of results plus the cursor for the next.
type InvoicePage struct {
	Invoices []Invoice
	HasMore  bool
	// LastID is the id of the last item in Invoices. Pass as
	// StartingAfter on the next ListByCustomerPage call to continue.
	// Empty when Invoices is empty.
	LastID string
}
