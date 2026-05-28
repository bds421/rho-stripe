# Tax IDs (B2B VAT)

B2B customers want their tax registration printed on the invoice — it
makes the invoice "reverse-chargeable" in the EU (no VAT collected,
buyer self-accounts) and is mandatory in most jurisdictions for
expense-claim purposes.

rho-stripe exposes Stripe's customer tax-ID API via
`conn.Customers`:

## Add

```go
tid, err := conn.Customers.AddTaxID(ctx, customers.AddTaxIDInput{
    SubjectID: subjectID,
    Type:      "eu_vat",       // see Stripe's tax-ID type list
    Value:     "DE123456789",  // the registration number
})
```

Stripe validates the format synchronously. For EU VAT specifically,
Stripe queues an asynchronous VIES lookup; the result lands later via
the `customer.tax_id.updated` webhook (wire `OnCustomerTaxIDUpdated`
when this matters to you).

## List

```go
tids, err := conn.Customers.ListTaxIDs(ctx, subjectID)
for _, t := range tids {
    fmt.Printf("%s (%s) verified=%s\n", t.Value, t.Type, t.Verification)
}
```

`t.Verification` is `""`, `"pending"`, `"verified"`, or `"unverified"`.

## Remove

```go
err := conn.Customers.RemoveTaxID(ctx, subjectID, "ti_…")
```

## Supported types

Stripe supports ~100 tax-ID types worldwide. Common ones:

| Type      | Example          | Region |
|-----------|------------------|--------|
| `eu_vat`  | `DE123456789`    | EU member states |
| `gb_vat`  | `GB123456789`    | United Kingdom |
| `us_ein`  | `12-3456789`     | United States |
| `au_abn`  | `12345678901`    | Australia |
| `ca_bn`   | `123456789`      | Canada (business number) |
| `br_cnpj` | `01.234.567/0001-89` | Brazil |
| `ch_vat`  | `CHE-123.456.789 MWST` | Switzerland |

See the full list in
[Stripe's customer-tax-id docs](https://docs.stripe.com/api/customer_tax_ids).

## Idempotency

`AddTaxID` derives an Idempotency-Key from `(customer, type, value)`,
so re-running the same call within 24h returns the existing record
rather than creating a duplicate. Safe to retry on transient errors.

## Verification

`TestLive_TaxIDRoundTrip` exercises Add → List → Remove against a
real Stripe customer; the test confirms the registration appears in
the list after Add and disappears after Remove.

## Where the tax ID surfaces

- **Hosted Checkout** — collected automatically when `TaxIDCollection`
  is enabled (default for B2B SessionDefaults).
- **Invoice PDF** — Stripe renders the customer's verified tax IDs
  below the customer address block.
- **`conn.Customers.Export(...)`** — every tax ID surfaces under
  `export.Stripe.TaxIDs` for GDPR fulfilment.
