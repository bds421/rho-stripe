# Programmatic invoice creation

For invoiced (net-30) flows: enterprise quotes, custom-priced
contracts, one-off invoices outside the recurring-subscription flow.

## Quick: from the CLI

```bash
rho-stripe invoice create \
    --customer cus_xyz \
    --amount 50000 \
    --currency eur \
    --description "Annual contract — Acme Corp" \
    --due 30 \
    --send
```

`--send` triggers Stripe to email the customer the hosted invoice
link immediately after finalization. Without it, the draft sits
for review.

## Programmatic

```go
inv, err := conn.Invoices.CreateDraft(ctx, invoices.CreateInput{
    Customer:  "cus_xyz",
    DueIn:     30 * 24 * time.Hour,
    Memo:      "Per our agreement signed 2026-01-15",
    LineItems: []invoices.CreateLineItem{
        {Description: "Pro plan, annual contract", Amount: 50000, Currency: "eur", Quantity: 1},
        {Description: "Onboarding & integration", Amount: 25000, Currency: "eur", Quantity: 1},
    },
})

// Review → finalize → send
inv, _ = conn.Invoices.FinalizeAndSend(ctx, inv.StripeID)
log.Printf("invoice sent: %s", inv.HostedInvoiceURL)
```

## Recurring net-30 subscriptions (different path)

For a recurring "send invoice every month, customer wires by net 30":

```go
sub, err := conn.Subscriptions.CreateInvoiced(ctx, subscriptions.InvoicedInput{
    StripeCustomerID: "cus_xyz",
    PriceKey:         "enterprise.annual_eur",
    DueIn:            30 * 24 * time.Hour,
    SetupFee:         100000,  // €1000 one-time onboarding on the first invoice
})
```

Stripe handles the recurring invoice generation; `SetupFee` only
applies to the first cycle.

## Custom invoice numbering (gapless / EU compliance)

For Austrian §11 UStG and similar — see
[invoices-gapless-numbering.md](invoices-gapless-numbering.md).

## Voiding + credit notes

Apps that need to cancel a sent invoice:

```go
err := conn.Invoices.Void(ctx, "in_…")  // status → "void"
```

For partial refunds via the EU-compliant credit-note path:

```go
err := conn.Invoices.IssueCreditNote(ctx, invoices.CreditNoteInput{
    InvoiceID: "in_…",
    Lines:     []invoices.CreateLineItem{{Amount: 10000, Currency: "eur", Description: "Partial refund"}},
    Reason:    "order_change",
})
```

For non-EU refunds (raw refund, no credit note), use `conn.Refunds.Refund` instead.

## Listing customer invoices

```go
// Single page (limit defaults to 50):
list, _ := conn.Invoices.ListByCustomer(ctx, "cus_xyz", "", 0)

// With pagination cursor for > 50:
req := invoices.InvoicePageRequest{CustomerID: "cus_xyz", Limit: 100}
for {
    page, _ := conn.Invoices.ListByCustomerPage(ctx, req)
    handle(page.Invoices)
    if !page.HasMore { break }
    req.StartingAfter = page.LastID
}
```

## Audit log

```go
report, _ := conn.Invoices.ExportAuditLog(ctx, invoices.AuditOptions{
    Period: invoices.Period{Start: q1Start, End: q1End},
    Format: invoices.FormatCSV,
})
os.WriteFile("q1-invoices.csv", report.Bytes, 0644)
```

Per-currency totals, voided invoices, refunds, credit notes — the
shape your accountant expects.
