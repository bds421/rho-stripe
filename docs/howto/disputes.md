# Disputes (chargebacks)

When a customer disputes a charge, Stripe gives you 7–21 days to
submit evidence before the network rules against you. `conn.Disputes`
wraps the submission side; the webhook handlers (`OnDisputeCreated`
etc., typed in slice 48) cover the observation side.

## Lifecycle

```
customer disputes
  → charge.dispute.created webhook
  → app has ~N days to respond
  → conn.Disputes.SubmitEvidence(...)
  → charge.dispute.updated → status="under_review"
  → network rules → charge.dispute.closed status="won" or "lost"
  → if lost → charge.dispute.funds_withdrawn
```

## Submit evidence

```go
_, err := conn.Disputes.SubmitEvidence(ctx, "dp_...", disputes.Evidence{
    CustomerName:           order.CustomerName,
    CustomerEmail:          order.CustomerEmail,
    CustomerPurchaseIP:     order.CustomerIPAtPurchase,
    ProductDescription:     order.ProductDescription,

    // Shipping evidence (for physical-product disputes)
    ShippingAddress:        order.ShippingAddress,
    ShippingDate:           order.ShippedAt.Format("2006-01-02"),
    ShippingCarrier:        order.Carrier,
    ShippingTrackingNumber: order.Tracking,
    ShippingDocumentation:  shippingPDFFileID,    // uploaded via Stripe Files API

    // Communication
    CustomerCommunication: emailReceiptFileID,
    Receipt:               invoicePDFFileID,
})
```

The `Evidence` struct mirrors Stripe's full chargeback-evidence
schema (20+ fields). Fill what's relevant to the dispute reason
(see Stripe's chargeback playbook).

## Visa Compelling Evidence 3.0

For Visa disputes specifically, the `EnhancedEvidence` field
unlocks the CE3.0 evidence program (significantly higher win rates):

```go
evidence.EnhancedEvidence = &disputes.EnhancedEvidence{
    VisaCompellingEvidence3: &disputes.VisaCE3{
        DisputedTransactionID: "ch_...",
        PriorUndisputedTransactions: []string{
            "ch_prior_1", "ch_prior_2",  // up to 5 prior charges
        },
    },
}
```

Apps fighting fraud disputes on Visa cards should always populate
this when they can — without it Visa assumes "no compelling evidence"
and rules for the cardholder.

## Acknowledge loss

If you won't fight the dispute, call `Close` to acknowledge — Stripe
treats this differently from "no response" (the latter incurs the
network's no-response fee on Mastercard):

```go
_, err := conn.Disputes.Close(ctx, "dp_...")
```

Irreversible.

## List + Get

```go
list, _ := conn.Disputes.List(ctx, disputes.ListInput{
    ChargeID: "ch_...",   // optional filter
    Limit:    50,
})

one, _ := conn.Disputes.Retrieve(ctx, "dp_...")
// one.Status, one.EvidenceDueBy, one.HasEvidence, one.Amount, ...
```

## Idempotency

`SubmitEvidence` keys on `(disputeID, content hash)` — re-submitting
the same evidence dedupes, but a re-submission with *new* content
gets a fresh key and actually replaces. This avoids the slice-51
class-bug where retrying with corrected evidence within 24h would
silently keep the prior submission.
