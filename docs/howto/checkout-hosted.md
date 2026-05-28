# Hosted Checkout (default)

The simplest payment flow: redirect the customer to Stripe's hosted
page, they pay, they bounce back to your `SuccessURL`.

```go
sess, err := conn.Checkout.CreateSession(ctx, checkout.Input{
    SubjectID:    "org_acme",
    LineItems:  []checkout.LineItem{{PriceKey: "pro.monthly_eur"}},
    SuccessURL: "https://app.example.com/success?cs={CHECKOUT_SESSION_ID}",
    CancelURL:  "https://app.example.com/pricing",
})
http.Redirect(w, r, sess.URL, http.StatusSeeOther)
```

That's it. The library handles:

- Resolving `pro.monthly_eur` to a Stripe price id via the catalog cache
- Creating (or reusing) the Stripe Customer for `Subject`
- Stamping `metadata.app_namespace` so webhooks route correctly
- B2B defaults: automatic tax, VAT-ID collection, billing address required
- Inferring `mode=subscription` vs `mode=payment` from the line-item types

## Multi-line carts

```go
LineItems: []checkout.LineItem{
    {PriceKey: "pro.monthly_eur", Quantity: 5},          // 5 seats
    {PriceKey: "addon.api_throughput", Quantity: 1},
}
```

Mixed one-time + recurring rejected with `ErrMixedItemTypes` — Stripe
doesn't allow it. Sell add-ons via a separate session OR via
subscription `AddInvoiceItems`.

## Per-actor attribution

```go
sess, err := conn.Checkout.CreateSession(ctx, checkout.Input{
    SubjectID: "org_acme",        // who pays
    Actor:   "user_alice",      // who clicked Buy
    ...
})
```

`Actor` lands in Stripe's `client_reference_id` for audit trails.

## Capping payment methods

By default Stripe shows whatever methods are enabled on your account.
To restrict:

```go
import "github.com/bds421/rho-stripe/payments"

LineItems:      ...,
PaymentMethods: payments.MethodsFor(payments.IntentSubscription),
// or hard-code: PaymentMethods: []string{"card", "sepa_debit"}
```

## After completion

Stripe POSTs `checkout.session.completed` → wire `Handlers.OnCheckoutCompleted`
to provision access. If your product has `CreditGrant` in the spec,
the lib auto-grants credits BEFORE your handler runs (no extra wiring).

## See also

- [checkout-embedded.md](checkout-embedded.md) — same backend, frontend-side rendering
- [checkout-b2c.md](checkout-b2c.md) — turning off tax / VAT-ID collection
- [customer-portal.md](customer-portal.md) — letting customers self-manage
- [trials.md](trials.md) — adding a free trial
