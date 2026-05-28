package invoices_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bds421/rho-stripe/invoices"
)

type fakeLister struct {
	entries []invoices.AuditEntry
	err     error
}

func (f *fakeLister) ListInvoices(_ context.Context, _ invoices.Period) ([]invoices.AuditEntry, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.entries, nil
}

func sampleEntries() []invoices.AuditEntry {
	t1 := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 1, 20, 11, 0, 0, 0, time.UTC)
	finalized1 := t1.Add(time.Hour)
	paid1 := t1.Add(2 * time.Hour)
	return []invoices.AuditEntry{
		{
			StripeID: "in_2", Number: "INV-0002", Type: invoices.EntryTypeInvoice,
			Status: "paid", CustomerID: "cus_b", Currency: "eur",
			AmountTotal: 58800, AmountTax: 9800, AmountPaid: 58800,
			Created: t2, Finalized: &finalized1, Paid: &paid1,
		},
		{
			StripeID: "in_1", Number: "INV-0001", Type: invoices.EntryTypeInvoice,
			Status: "paid", CustomerID: "cus_a", Currency: "eur",
			AmountTotal: 5880, AmountTax: 980, AmountPaid: 5880,
			Created: t1, Finalized: &finalized1, Paid: &paid1,
		},
	}
}

func TestExportAuditLog_SortsByCreated(t *testing.T) {
	rep, err := invoices.ExportAuditLog(t.Context(), &fakeLister{entries: sampleEntries()}, invoices.AuditOptions{
		Period: invoices.Period{
			Start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			End:   time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		},
	})
	if err != nil {
		t.Fatalf("ExportAuditLog: %v", err)
	}
	if len(rep.Entries) != 2 {
		t.Fatalf("len = %d, want 2", len(rep.Entries))
	}
	if rep.Entries[0].StripeID != "in_1" || rep.Entries[1].StripeID != "in_2" {
		t.Errorf("entries not sorted by Created asc: %v %v", rep.Entries[0].StripeID, rep.Entries[1].StripeID)
	}
}

func TestExportAuditLog_TotalsPerCurrency(t *testing.T) {
	rep, _ := invoices.ExportAuditLog(t.Context(), &fakeLister{entries: sampleEntries()}, invoices.AuditOptions{
		Period: invoices.Period{Start: time.Time{}, End: time.Now()},
	})
	if rep.TotalsTotal["eur"] != 58800+5880 {
		t.Errorf("TotalsTotal[eur] = %d, want %d", rep.TotalsTotal["eur"], 58800+5880)
	}
	if rep.TotalsTax["eur"] != 9800+980 {
		t.Errorf("TotalsTax[eur] = %d", rep.TotalsTax["eur"])
	}
}

func TestExportAuditLog_MultiCurrencyTotals(t *testing.T) {
	entries := []invoices.AuditEntry{
		{StripeID: "i1", Currency: "eur", AmountTotal: 1000, AmountTax: 200, Created: time.Now()},
		{StripeID: "i2", Currency: "usd", AmountTotal: 2000, AmountTax: 0, Created: time.Now()},
		{StripeID: "i3", Currency: "eur", AmountTotal: 500, AmountTax: 100, Created: time.Now()},
	}
	rep, _ := invoices.ExportAuditLog(t.Context(), &fakeLister{entries: entries}, invoices.AuditOptions{
		Period: invoices.Period{Start: time.Time{}, End: time.Now()},
	})
	if rep.TotalsTotal["eur"] != 1500 || rep.TotalsTotal["usd"] != 2000 {
		t.Errorf("multi-currency totals wrong: %v", rep.TotalsTotal)
	}
}

func TestExportAuditLog_EncodeCSV(t *testing.T) {
	rep, _ := invoices.ExportAuditLog(t.Context(), &fakeLister{entries: sampleEntries()}, invoices.AuditOptions{
		Period: invoices.Period{Start: time.Time{}, End: time.Now()},
	})
	out, err := rep.Encode(invoices.FormatCSV)
	if err != nil {
		t.Fatalf("Encode CSV: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "stripe_id,number,type,status") {
		t.Errorf("CSV missing header: %s", s)
	}
	if !strings.Contains(s, "INV-0001") || !strings.Contains(s, "INV-0002") {
		t.Errorf("CSV missing invoice numbers")
	}
	if !strings.Contains(s, "#totals_currency,amount_total,amount_tax,amount_paid") {
		t.Errorf("CSV missing totals section")
	}
	if !strings.Contains(s, "eur,64680,") {
		t.Errorf("CSV totals row wrong: %s", s)
	}
}

func TestExportAuditLog_EncodeJSON(t *testing.T) {
	rep, _ := invoices.ExportAuditLog(t.Context(), &fakeLister{entries: sampleEntries()}, invoices.AuditOptions{
		Period: invoices.Period{Start: time.Time{}, End: time.Now()},
	})
	out, err := rep.Encode(invoices.FormatJSON)
	if err != nil {
		t.Fatalf("Encode JSON: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"Entries"`) || !strings.Contains(s, `"TotalsTotal"`) {
		t.Errorf("JSON missing expected fields: %s", s[:200])
	}
}

func TestExportAuditLog_RejectsInvertedPeriod(t *testing.T) {
	_, err := invoices.ExportAuditLog(t.Context(), &fakeLister{}, invoices.AuditOptions{
		Period: invoices.Period{
			Start: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
			End:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	})
	if err == nil {
		t.Error("expected error for End < Start")
	}
}

func TestExportAuditLog_NilListerErrors(t *testing.T) {
	_, err := invoices.ExportAuditLog(t.Context(), nil, invoices.AuditOptions{})
	if err == nil {
		t.Error("expected error for nil lister")
	}
}

func TestExportAuditLog_EmptyEntries(t *testing.T) {
	rep, err := invoices.ExportAuditLog(t.Context(), &fakeLister{entries: nil}, invoices.AuditOptions{
		Period: invoices.Period{Start: time.Time{}, End: time.Now()},
	})
	if err != nil {
		t.Fatalf("empty entries should not error: %v", err)
	}
	if len(rep.Entries) != 0 {
		t.Errorf("len = %d, want 0", len(rep.Entries))
	}
	out, _ := rep.Encode(invoices.FormatCSV)
	if !strings.Contains(string(out), "stripe_id,number") {
		t.Error("CSV header missing even for empty report")
	}
}
