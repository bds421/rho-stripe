# Bank transfers / SEPA / wire payments

Three different patterns Stripe supports; the lib provides presets
and lets Stripe handle the multi-step UX on the hosted page.

## Pattern 1: SEPA Direct Debit (EU recurring)

Customer enters their IBAN; Stripe collects the SEPA mandate;
recurring charges debit automatically.

```go
import "github.com/bds421/rho-stripe/payments"

sess, _ := conn.Checkout.CreateSession(ctx, checkout.Input{
    SubjectID:        "org_acme",
    LineItems:      []checkout.LineItem{{PriceKey: "pro.monthly_eur"}},
    SuccessURL:     "...",
    CancelURL:      "...",
    PaymentMethods: payments.MethodsFor(payments.IntentSEPAOnly),
    // or hard-code: []string{"sepa_debit"}
})
```

Stripe handles the mandate on the hosted page. Your webhook receives
`mandate.updated` events (no typed handler today — use `OnOtherEvent`
if you need it).

## Pattern 2: Bank transfer / wire (B2B EU)

Customer pre-funds their `customer.balance` via a wire transfer with
a virtual bank account Stripe gives them. Invoices then auto-debit
the balance.

```go
sess, _ := conn.Checkout.CreateSession(ctx, checkout.Input{
    LineItems:      []checkout.LineItem{{PriceKey: "pro.annual_eur"}},
    PaymentMethods: payments.MethodsFor(payments.IntentBankTransferEU),
})
```

When the wire arrives, Stripe credits the customer_balance and fires
`customer_balance_transaction.created`. Apps don't need to do
anything — Stripe auto-applies the funds to outstanding invoices.

Other geos:
- `payments.IntentBankTransferUS` — ACH + Plaid-backed ACH debit
- `payments.IntentBankTransferUK` — BACS / Faster Payments

## Pattern 3: Manual invoice + customer-balance reconciliation

For sales-led enterprise: create a custom invoice, customer wires
the money to Stripe's virtual account, the funds appear in their
balance, the invoice auto-pays.

```go
inv, _ := conn.Invoices.CreateDraft(ctx, invoices.CreateInput{
    Customer:  "cus_xyz",
    DueIn:     30 * 24 * time.Hour,
    LineItems: []invoices.CreateLineItem{{Amount: 50000, Currency: "eur", Description: "Annual contract"}},
})
inv, _ = conn.Invoices.FinalizeAndSend(ctx, inv.StripeID)
// Customer receives hosted invoice link → pays via wire → Stripe
// auto-settles when funds arrive.
```

## What the lib does NOT handle

- **SEPA mandate UI.** Stripe's hosted page does it; apps using the
  embedded element write their own (Stripe.js provides the components).
- **Bank-transfer payment confirmation events.** App-side: subscribe
  to `customer_balance_transaction.created` via `OnOtherEvent` and
  trigger your fulfillment workflow when funds arrive.
- **ACH micro-deposit verification.** Done via Stripe's hosted UI.

## Payment-method presets

| Constant | What it includes | Use case |
|---|---|---|
| `PresetSEPADirectDebit` | sepa_debit | EU recurring (subscription) |
| `PresetBankTransferEU` | customer_balance | EU B2B wire / SEPA Credit Transfer |
| `PresetBankTransferUS` | customer_balance, us_bank_account | US ACH + Plaid |
| `PresetBankTransferUK` | customer_balance, bacs_debit | UK BACS / Faster Payments |
| `PresetCardOnly` | card | Reject all non-card flows (digital goods needing instant settlement) |
| `PresetCreditTopUp` | card, apple_pay, google_pay | Credit packs (SEPA is too slow — 1-3 day confirmation) |
| `PresetSubscription` | card, sepa_debit, apple_pay, google_pay | Generic subscriptions |
