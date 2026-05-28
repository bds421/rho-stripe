package invoices

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"
)

// ExportAuditLog fetches every invoice in the configured period from
// the Stripe lister, computes per-currency totals, and returns an
// AuditReport. The returned report can be serialized to CSV or JSON
// via its Encode method.
//
// Implementations of Lister are expected to handle pagination
// internally (Stripe API limits results per call).
func ExportAuditLog(ctx context.Context, lister Lister, opts AuditOptions) (*AuditReport, error) {
	if lister == nil {
		return nil, fmt.Errorf("invoices: ExportAuditLog: Lister is required")
	}
	if opts.Period.End.Before(opts.Period.Start) {
		return nil, fmt.Errorf("invoices: ExportAuditLog: Period.End (%s) is before Period.Start (%s)",
			opts.Period.End, opts.Period.Start)
	}

	entries, err := lister.ListInvoices(ctx, opts.Period)
	if err != nil {
		return nil, fmt.Errorf("invoices: list invoices: %w", err)
	}

	// Stable order: by Created ascending, ties broken by StripeID.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Created.Equal(entries[j].Created) {
			return entries[i].StripeID < entries[j].StripeID
		}
		return entries[i].Created.Before(entries[j].Created)
	})

	report := &AuditReport{
		Period:      opts.Period,
		Entries:     entries,
		TotalsTotal: map[string]int64{},
		TotalsTax:   map[string]int64{},
		TotalsPaid:  map[string]int64{},
	}
	for _, e := range entries {
		c := e.Currency
		report.TotalsTotal[c] += e.AmountTotal
		report.TotalsTax[c] += e.AmountTax
		report.TotalsPaid[c] += e.AmountPaid
	}
	return report, nil
}

// Encode serializes the report in the given format. CSV is suitable
// for handing to a tax advisor or importing into accounting software.
// JSON is suitable for programmatic consumers.
func (r *AuditReport) Encode(format Format) ([]byte, error) {
	switch format {
	case "", FormatCSV:
		return r.encodeCSV()
	case FormatJSON:
		return r.encodeJSON()
	default:
		return nil, fmt.Errorf("invoices: unknown format %q", format)
	}
}

func (r *AuditReport) encodeCSV() ([]byte, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	header := []string{
		"stripe_id", "number", "type", "status",
		"customer_id", "customer_email", "customer_vat_id", "customer_country",
		"currency", "amount_total", "amount_tax", "amount_paid",
		"created", "finalized", "paid", "voided",
		"hosted_invoice_url",
	}
	if err := w.Write(header); err != nil {
		return nil, err
	}
	for _, e := range r.Entries {
		row := []string{
			e.StripeID,
			e.Number,
			string(e.Type),
			e.Status,
			e.CustomerID,
			e.CustomerEmail,
			e.CustomerVATID,
			e.CustomerCountry,
			e.Currency,
			strconv.FormatInt(e.AmountTotal, 10),
			strconv.FormatInt(e.AmountTax, 10),
			strconv.FormatInt(e.AmountPaid, 10),
			formatTime(&e.Created),
			formatTime(e.Finalized),
			formatTime(e.Paid),
			formatTime(e.Voided),
			e.HostedInvoiceURL,
		}
		if err := w.Write(row); err != nil {
			return nil, err
		}
	}

	// Totals summary at the end (commented row + totals).
	if err := w.Write([]string{}); err != nil {
		return nil, err
	}
	if err := w.Write([]string{"#totals_currency", "amount_total", "amount_tax", "amount_paid"}); err != nil {
		return nil, err
	}
	for _, c := range sortedCurrencies(r.TotalsTotal) {
		if err := w.Write([]string{
			c,
			strconv.FormatInt(r.TotalsTotal[c], 10),
			strconv.FormatInt(r.TotalsTax[c], 10),
			strconv.FormatInt(r.TotalsPaid[c], 10),
		}); err != nil {
			return nil, err
		}
	}

	w.Flush()
	if err := w.Error(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (r *AuditReport) encodeJSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

func formatTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

func sortedCurrencies(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
