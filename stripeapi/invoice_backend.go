package stripeapi

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/bds421/rho-stripe/invoices"
	stripe "github.com/stripe/stripe-go/v82"
)

// InvoiceBackend implements invoices.Lister against the live Stripe
// API via stripe-go. Audit-log queries iterate every invoice in the
// configured period, transforming Stripe's response into the lib's
// projection.
type InvoiceBackend struct {
	sc *stripe.Client
}

// NewInvoiceBackend wraps a stripe-go client.
func NewInvoiceBackend(sc *stripe.Client) *InvoiceBackend {
	if sc == nil {
		panic("stripeapi.NewInvoiceBackend: StripeClient is required")
	}
	return &InvoiceBackend{sc: sc}
}

var (
	_ invoices.Lister  = (*InvoiceBackend)(nil)
	_ invoices.Backend = (*InvoiceBackend)(nil)
)

// ListInvoices fetches every Stripe Invoice created in the period.
// Stripe's API has no server-side filter for "created in range", but
// it accepts created.gte / created.lte query params via the generic
// filter mechanism.
func (b *InvoiceBackend) ListInvoices(ctx context.Context, period invoices.Period) ([]invoices.AuditEntry, error) {
	params := &stripe.InvoiceListParams{}
	params.Filters.AddFilter("created", "gte", fmt.Sprintf("%d", period.Start.Unix()))
	params.Filters.AddFilter("created", "lt", fmt.Sprintf("%d", period.End.Unix()))
	params.Filters.AddFilter("limit", "", "100")

	var out []invoices.AuditEntry
	for inv, err := range b.sc.V1Invoices.List(ctx, params) {
		if err != nil {
			return nil, fmt.Errorf("list invoices: %w", err)
		}
		out = append(out, projectInvoice(inv))
	}
	return out, nil
}

func projectInvoice(inv *stripe.Invoice) invoices.AuditEntry {
	entry := invoices.AuditEntry{
		StripeID:         inv.ID,
		Number:           inv.Number,
		Type:             invoices.EntryTypeInvoice,
		Status:           string(inv.Status),
		Currency:         string(inv.Currency),
		AmountTotal:      inv.Total,
		AmountTax:        sumInvoiceTaxes(inv.TotalTaxes),
		AmountPaid:       inv.AmountPaid,
		Created:          time.Unix(inv.Created, 0).UTC(),
		HostedInvoiceURL: inv.HostedInvoiceURL,
	}
	if inv.Customer != nil {
		entry.CustomerID = inv.Customer.ID
		entry.CustomerEmail = inv.Customer.Email
		if inv.Customer.Address != nil {
			entry.CustomerCountry = inv.Customer.Address.Country
		}
	}
	if len(inv.CustomerTaxIDs) > 0 {
		entry.CustomerVATID = inv.CustomerTaxIDs[0].Value
	}
	if inv.StatusTransitions != nil {
		if inv.StatusTransitions.FinalizedAt > 0 {
			t := time.Unix(inv.StatusTransitions.FinalizedAt, 0).UTC()
			entry.Finalized = &t
		}
		if inv.StatusTransitions.PaidAt > 0 {
			t := time.Unix(inv.StatusTransitions.PaidAt, 0).UTC()
			entry.Paid = &t
		}
		if inv.StatusTransitions.VoidedAt > 0 {
			t := time.Unix(inv.StatusTransitions.VoidedAt, 0).UTC()
			entry.Voided = &t
		}
	}
	return entry
}

// sumInvoiceTaxes adds the per-jurisdiction tax amounts Stripe v82
// returns in TotalTaxes. A single invoice in mixed-jurisdiction
// configurations (uncommon for SaaS) may have multiple entries.
func sumInvoiceTaxes(taxes []*stripe.InvoiceTotalTax) int64 {
	var sum int64
	for _, t := range taxes {
		if t != nil {
			sum += t.Amount
		}
	}
	return sum
}

// --- Phase 6 mutation methods ---

// CreateDraft creates an invoice for the customer + attaches line
// items. Stripe's two-step flow: first create each line as an
// InvoiceItem on the customer (pending), then create the invoice
// (which pulls in all pending items).
func (b *InvoiceBackend) CreateDraft(ctx context.Context, in invoices.CreateInput) (invoices.Invoice, error) {
	for i, li := range in.LineItems {
		itemParams := &stripe.InvoiceItemCreateParams{
			Customer:    stripe.String(string(in.Customer)),
			Amount:      stripe.Int64(li.Amount * int64(qtyOrOne(li.Quantity))),
			Currency:    stripe.String(li.Currency),
			Description: stripe.String(li.Description),
		}
		// Item index in the line-items slice guarantees a stable
		// per-line key when callers retry the whole draft creation.
		applyIdem(itemParams, ctx, "invoice_item.create",
			string(in.Customer), canonicalInt64(int64(i)),
			li.Description, li.Currency,
			canonicalInt64(li.Amount), canonicalInt64(int64(li.Quantity)),
			in.NumberOverride, // distinguish per-draft if caller pre-allocated a number
			canonicalMap(in.Metadata),
		)
		if _, err := b.sc.V1InvoiceItems.Create(ctx, itemParams); err != nil {
			return invoices.Invoice{}, fmt.Errorf("stripe.InvoiceItems.Create: %w", err)
		}
	}

	params := &stripe.InvoiceCreateParams{
		Customer:         stripe.String(string(in.Customer)),
		CollectionMethod: stripe.String(string(stripe.InvoiceCollectionMethodSendInvoice)),
	}
	if in.DueIn > 0 {
		days := int64(in.DueIn / (24 * time.Hour))
		if days < 1 {
			days = 1
		}
		params.DaysUntilDue = stripe.Int64(days)
	}
	if in.Memo != "" {
		params.Description = stripe.String(in.Memo)
	}
	if in.NumberOverride != "" {
		params.Number = stripe.String(in.NumberOverride)
	}
	for k, v := range in.Metadata {
		params.AddMetadata(k, v)
	}
	applyIdem(params, ctx, "invoice.create_draft",
		string(in.Customer), in.NumberOverride, in.Memo,
		canonicalInt64(int64(in.DueIn)), canonicalMap(in.Metadata))

	inv, err := b.sc.V1Invoices.Create(ctx, params)
	if err != nil {
		return invoices.Invoice{}, fmt.Errorf("stripe.Invoices.Create: %w", err)
	}
	return projectInvoiceLite(inv), nil
}

func (b *InvoiceBackend) Finalize(ctx context.Context, invoiceID string) (invoices.Invoice, error) {
	params := &stripe.InvoiceFinalizeInvoiceParams{}
	applyIdem(params, ctx, "invoice.finalize", invoiceID)
	inv, err := b.sc.V1Invoices.FinalizeInvoice(ctx, invoiceID, params)
	if err != nil {
		return invoices.Invoice{}, fmt.Errorf("stripe.Invoices.FinalizeInvoice(%s): %w", invoiceID, err)
	}
	return projectInvoiceLite(inv), nil
}

func (b *InvoiceBackend) FinalizeAndSend(ctx context.Context, invoiceID string) (invoices.Invoice, error) {
	finalized, err := b.Finalize(ctx, invoiceID)
	if err != nil {
		return finalized, err
	}
	params := &stripe.InvoiceSendInvoiceParams{}
	applyIdem(params, ctx, "invoice.send", invoiceID)
	inv, err := b.sc.V1Invoices.SendInvoice(ctx, invoiceID, params)
	if err != nil {
		return finalized, fmt.Errorf("stripe.Invoices.SendInvoice(%s): %w", invoiceID, err)
	}
	return projectInvoiceLite(inv), nil
}

func (b *InvoiceBackend) Void(ctx context.Context, invoiceID string) error {
	params := &stripe.InvoiceVoidInvoiceParams{}
	applyIdem(params, ctx, "invoice.void", invoiceID)
	if _, err := b.sc.V1Invoices.VoidInvoice(ctx, invoiceID, params); err != nil {
		return fmt.Errorf("stripe.Invoices.VoidInvoice(%s): %w", invoiceID, err)
	}
	return nil
}

func (b *InvoiceBackend) MarkUncollectible(ctx context.Context, invoiceID string) error {
	params := &stripe.InvoiceMarkUncollectibleParams{}
	applyIdem(params, ctx, "invoice.mark_uncollectible", invoiceID)
	if _, err := b.sc.V1Invoices.MarkUncollectible(ctx, invoiceID, params); err != nil {
		return fmt.Errorf("stripe.Invoices.MarkUncollectible(%s): %w", invoiceID, err)
	}
	return nil
}

func (b *InvoiceBackend) IssueCreditNote(ctx context.Context, in invoices.CreditNoteInput) error {
	params := &stripe.CreditNoteCreateParams{
		Invoice: stripe.String(in.InvoiceID),
	}
	if in.Reason != "" {
		params.Reason = stripe.String(in.Reason)
	}
	if in.Refund {
		// in.Refund=true → also issue a Stripe refund alongside the
		// credit note. RefundAmount: 0 means "match the credit note
		// total." For more precise control, callers can set per-line
		// amounts and use the raw stripe-go client.
		params.RefundAmount = stripe.Int64(0)
	}
	for _, li := range in.Lines {
		lp := &stripe.CreditNoteCreateLineParams{
			Type:        stripe.String("custom_line_item"),
			Description: stripe.String(li.Description),
			UnitAmount:  stripe.Int64(li.Amount),
			Quantity:    stripe.Int64(int64(qtyOrOne(li.Quantity))),
		}
		params.Lines = append(params.Lines, lp)
	}
	applyIdem(params, ctx, "credit_note.create", in.InvoiceID, in.Reason,
		strconv.FormatBool(in.Refund), canonicalCreditNoteLines(in.Lines))
	if _, err := b.sc.V1CreditNotes.Create(ctx, params); err != nil {
		return fmt.Errorf("stripe.CreditNotes.Create(%s): %w", in.InvoiceID, err)
	}
	return nil
}

func canonicalCreditNoteLines(lines []invoices.CreateLineItem) string {
	parts := make([]string, 0, len(lines))
	for _, li := range lines {
		parts = append(parts, li.Description+"|"+
			canonicalInt64(li.Amount)+"|"+canonicalInt64(int64(li.Quantity)))
	}
	return canonicalStrings(parts)
}

// ListOverdue returns open invoices past their due date.
func (b *InvoiceBackend) ListOverdue(ctx context.Context) ([]invoices.Invoice, error) {
	params := &stripe.InvoiceListParams{
		Status: stripe.String("open"),
	}
	params.Filters.AddFilter("due_date", "lt", fmt.Sprintf("%d", time.Now().Unix()))
	params.Filters.AddFilter("limit", "", "100")

	var out []invoices.Invoice
	for inv, err := range b.sc.V1Invoices.List(ctx, params) {
		if err != nil {
			return nil, fmt.Errorf("stripe.Invoices.List(overdue): %w", err)
		}
		out = append(out, projectInvoiceLite(inv))
	}
	return out, nil
}

// ListByCustomer returns the customer's invoices in reverse-chrono
// order, optionally filtered by status. Used by Operations.ListByCustomer.
func (b *InvoiceBackend) ListByCustomer(ctx context.Context, customerID invoices.StripeCustomerID, status string, limit int) ([]invoices.Invoice, error) {
	if limit <= 0 {
		limit = 50
	}
	params := &stripe.InvoiceListParams{
		Customer: stripe.String(string(customerID)),
	}
	if status != "" {
		params.Status = stripe.String(status)
	}
	params.Filters.AddFilter("limit", "", fmt.Sprintf("%d", limit))

	var out []invoices.Invoice
	for inv, err := range b.sc.V1Invoices.List(ctx, params) {
		if err != nil {
			return nil, fmt.Errorf("stripe.Invoices.List(customer=%s): %w", customerID, err)
		}
		out = append(out, projectInvoiceLite(inv))
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// ListByCustomerPage implements invoices.Backend with pagination.
//
// HasMore detection: we request limit+1 items from Stripe and report
// HasMore=true iff the +1 actually arrived (then drop it from the
// returned slice). This eliminates the false-positive that the
// "len == limit" heuristic produced when a customer's invoice count
// happens to exactly equal the page size — the caller now reliably
// knows whether issuing another page request would return data.
func (b *InvoiceBackend) ListByCustomerPage(ctx context.Context, req invoices.InvoicePageRequest) (invoices.InvoicePage, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	probe := limit + 1
	params := &stripe.InvoiceListParams{
		Customer: stripe.String(string(req.CustomerID)),
	}
	if req.Status != "" {
		params.Status = stripe.String(req.Status)
	}
	if req.StartingAfter != "" {
		params.StartingAfter = stripe.String(req.StartingAfter)
	}
	params.Filters.AddFilter("limit", "", fmt.Sprintf("%d", probe))

	out := invoices.InvoicePage{Invoices: make([]invoices.Invoice, 0, limit)}
	count := 0
	for inv, err := range b.sc.V1Invoices.List(ctx, params) {
		if err != nil {
			return invoices.InvoicePage{}, fmt.Errorf("stripe.Invoices.List(page): %w", err)
		}
		count++
		if count > limit {
			// We requested one extra item solely to detect HasMore.
			// Don't include it in the returned slice.
			out.HasMore = true
			break
		}
		out.Invoices = append(out.Invoices, projectInvoiceLite(inv))
	}
	if len(out.Invoices) > 0 {
		out.LastID = out.Invoices[len(out.Invoices)-1].StripeID
	}
	return out, nil
}

func projectInvoiceLite(inv *stripe.Invoice) invoices.Invoice {
	out := invoices.Invoice{
		StripeID:         inv.ID,
		Number:           inv.Number,
		Status:           string(inv.Status),
		Currency:         string(inv.Currency),
		AmountTotal:      inv.Total,
		AmountPaid:       inv.AmountPaid,
		HostedInvoiceURL: inv.HostedInvoiceURL,
		PDFURL:           inv.InvoicePDF,
	}
	if inv.Customer != nil {
		out.Customer = invoices.StripeCustomerID(inv.Customer.ID)
	}
	if inv.DueDate > 0 {
		t := time.Unix(inv.DueDate, 0).UTC()
		out.DueDate = &t
	}
	return out
}

func qtyOrOne(q int) int {
	if q <= 0 {
		return 1
	}
	return q
}
