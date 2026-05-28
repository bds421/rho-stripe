# Refunds + auto credit-ledger reversal

Two different APIs for "give the money back":

1. **`conn.Refunds.Refund`** — raw Stripe refund. Use for B2C / no-invoice
   flows / when you want money returned to the original payment method.
2. **`conn.Invoices.IssueCreditNote`** — EU-compliant credit note
   tied to a finalized invoice. Use for B2B / VAT-registered transactions
   where the auditor expects a credit note.

## Raw refund

```go
ref, err := conn.Refunds.Refund(ctx, payments.RefundInput{
    ChargeID: "ch_…",     // or PaymentIntentID: "pi_…"
    Amount:   0,           // 0 = full refund
    Reason:   payments.RefundReasonRequestedByCustomer,
})
```

The Reason enum is the same one Stripe accepts: `duplicate`,
`fraudulent`, `requested_by_customer`. Defaults to
`requested_by_customer` when empty.

Partial refunds: pass `Amount: 2500` for €25.00.

## Auto credit-ledger reversal

If your product grants credits on purchase (`CreditGrant` in catalog),
those credits should disappear when the customer refunds. Wire:

```go
cfg := connector.Config{
    AutoRevokeCreditsOnRefund: true,
    Credits: pgrepo.NewCreditRepo(db),
    // ...
}
```

The lib wraps `OnRefundCreated` with `credits.ApplyRefundReversal`:

1. Receives `charge.refunded` or `refund.created` event
2. Extracts the refunded `PaymentIntent` / `Charge` ID
3. Calls `repo.FindGrantsBySourceRef(piID / chargeID)`
4. `RevokeGrant` for each matching grant

Idempotent: re-delivered refund events don't re-revoke.

## What it doesn't auto-do

- **Subscription cancel-on-refund.** Refunding a sub invoice doesn't
  cancel the sub. App decides.
- **Partial-refund partial-revoke.** Currently revokes the entire
  matching grant on any refund (full or partial). Apps wanting
  prorated revocation handle it themselves via the `OnRefundCreated`
  handler chain (your handler runs AFTER the auto-revoke).

## Webhook flow

```go
Handlers: webhooks.Handlers{
    OnRefundCreated: func(ctx context.Context, evt webhooks.Event) error {
        // Runs AFTER ApplyRefundReversal (when AutoRevokeCreditsOnRefund=true).
        // Use for app-side logic: notify finance, update analytics, etc.
        return nil
    },
},
```

## Refund metadata

`Refund.Metadata` is stamped with `app_namespace` for webhook routing.
Apps can add their own metadata via `RefundInput.Metadata`.
