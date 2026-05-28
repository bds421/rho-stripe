# Saved payment methods (card-on-file)

For first-payment flows (collect card + purchase in one step), use
**Checkout** — Stripe's hosted page handles PCI scope.

For everything else — "update card from settings", multi-card per
customer, trial with optional card-on-file, B2B invoice flows where
the card backs only failed-payment retries — use `conn.PaymentMethods`.

## Add a card without a purchase

```go
si, err := conn.PaymentMethods.CreateSetupIntent(ctx, paymentmethods.CreateSetupIntentInput{
    SubjectID: subjectID,
    Usage:     "off_session",       // optional; default
    // PaymentMethodTypes: []string{"card", "us_bank_account"},  // optional; default ["card"]
})
// hand si.ClientSecret to the frontend Stripe.js Elements UI
```

Then on the frontend:

```js
stripe.confirmCardSetup(clientSecret, {
    payment_method: { card: cardElement }
}).then(result => {
    if (result.setupIntent.status === 'succeeded') {
        // PaymentMethod is attached to the customer
    }
});
```

## SCA / 3DS challenges

When the bank requires authentication, `SetupIntent.Status` becomes
`"requires_action"` and `NextAction` is populated:

```go
if si.NextAction != nil && si.NextAction.Type == "redirect_to_url" {
    http.Redirect(w, r, si.NextAction.RedirectToURL, http.StatusSeeOther)
}
```

Most apps don't need to inspect this directly — `stripe.confirmCardSetup`
on the frontend handles the redirect transparently. Server-side
inspection is only needed for custom UIs.

## List

```go
pms, err := conn.PaymentMethods.List(ctx, subjectID)        // cards only
pms, err := conn.PaymentMethods.List(ctx, subjectID,        // multiple types
    "card", "us_bank_account", "link")
```

Each `PaymentMethod` carries `IsDefault` set if it's the customer's
invoice-default.

## Set the default for invoices

Stripe uses the customer's default payment method for subscription
renewals. Apps usually let the customer pick:

```go
err := conn.PaymentMethods.SetDefaultPaymentMethod(ctx, subjectID, "pm_…")
```

## Attach an existing PaymentMethod

When the frontend created a PaymentMethod on a separate customer (or
without a customer at all) and the app needs to attach it:

```go
err := conn.PaymentMethods.AttachPaymentMethod(ctx, subjectID, "pm_…")
```

## Detach

```go
err := conn.PaymentMethods.DetachPaymentMethod(ctx, "pm_…")
```

After detach, the PaymentMethod remains in Stripe but cannot be used
for new charges on the customer.

## Idempotency

Every write derives an idempotency key from `(customer, paymentMethodID)`
or the SetupIntent's `(customer, usage)` — re-running a network-failed
request returns the same Stripe resource.

## Verification

`TestLive_SetupIntentForCardOnFile` creates a SetupIntent against a
real Stripe customer and asserts `ClientSecret` is populated.
