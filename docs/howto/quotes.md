# Quotes (B2B sales workflow)

When sales talks to a customer before billing starts — "here's a
custom proposal: $5k/year for the Pro tier + free onboarding" — the
Quote API is the right Stripe surface. Apps without a sales motion
don't need this.

## Flow

```
sales rep prepares  → conn.Quotes.Create(...)            // draft
sales rep reviews   → conn.Quotes.Finalize(...)          // locks line items
                      (hosted URL becomes shareable)
customer accepts    → conn.Quotes.Accept(...)            // optional server-side
                      OR customer clicks accept in hosted UI
Stripe auto-creates → Subscription (recurring) or Invoice (one-shot)
mirror catches up   → customer.subscription.created webhook
```

## Create + finalize

```go
q, err := conn.Quotes.Create(ctx, quotes.CreateInput{
    StripeCustomerID: "cus_…",
    LineItems: []quotes.QuoteLine{
        {StripePriceID: catalog.LookupKey("pro.annual_eur"), Quantity: 1},
        {StripePriceID: catalog.LookupKey("onboarding.flat"), Quantity: 1},
    },
    Description:      "Annual Pro plan + onboarding",
    Header:           "Acme Corp — 2026 contract",
    Footer:           "Net 30, payable EUR.",
    ExpiresAt:        time.Now().AddDate(0, 0, 30),
    CollectionMethod: "send_invoice",
    TrialPeriodDays:  14,
    Metadata:         map[string]string{"sales_rep": "alice@app"},
})
finalized, _ := conn.Quotes.Finalize(ctx, q.StripeID)
```

After Finalize, the quote is locked and the hosted URL is shareable.

## Accept (server-side)

For internal accounts where sign-off is bypassed, accept directly:

```go
accepted, _ := conn.Quotes.Accept(ctx, q.StripeID)
// accepted.SubscriptionID is now populated
```

## Cancel a draft

```go
_, _ = conn.Quotes.Cancel(ctx, q.StripeID)
```

Already-accepted quotes can't be canceled — their downstream
Subscription / Invoice must be canceled through normal means.

## Idempotency

`Create` keys on a content hash of *every* input field
(StripeCustomerID, LineItems, TrialPeriodDays, Metadata, …). Two
calls with the same intent dedupe; a call with one differing field
gets a fresh quote — the slice-51 bug class where only line items
contributed to the hash is fixed.

## Namespace

Quote metadata gets `app_namespace=<your namespace>` stamped
automatically, so webhook events route correctly under the
multi-app pattern.
