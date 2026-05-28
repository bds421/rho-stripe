# Customer Portal

Let customers self-manage their billing — update payment methods,
download invoices, cancel/upgrade subscriptions — without you
building UI for any of it.

```go
sess, err := conn.Checkout.CreatePortalSession(ctx, checkout.PortalInput{
    SubjectID:   "org_acme",
    ReturnURL: "https://app.example.com/billing",
})
http.Redirect(w, r, sess.URL, http.StatusSeeOther)
```

The lib resolves Subject → Stripe Customer via the `CustomerRepo`
you configured. ReturnURL is where Stripe sends the customer after
they finish (or click "back").

## Deep links into specific flows

Stripe supports "open the portal directly into the cancel-subscription
flow" / "open into the update-payment-method flow".

```go
sess, _ := conn.Checkout.CreatePortalSession(ctx, checkout.PortalInput{
    SubjectID:   "org_acme",
    ReturnURL: "...",
    Flow: &checkout.PortalFlow{
        Type:           checkout.PortalFlowSubscriptionCancel,
        SubscriptionID: "sub_…",  // required for cancel + update flows
    },
})
```

Supported flow types:
- `PortalFlowSubscriptionCancel`
- `PortalFlowSubscriptionUpdate`
- `PortalFlowPaymentMethodUpdate`

## Configuring what the portal shows

The portal's features (allow cancellation? show invoice history?
let customers switch plans?) are configured in **Stripe Dashboard →
Settings → Customer Portal**, NOT in the library. The lib's wrapper
just opens the portal — Stripe owns the rest.

## Apps without a CustomerRepo

If you're testing or don't yet have a CustomerRepo wired, the CLI
takes a `--customer cus_…` arg directly:

```bash
rho-stripe portal --customer cus_xyz --return https://app.example.com/billing
```
