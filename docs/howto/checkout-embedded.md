# Embedded Payment Element

Render the payment form inline in your app instead of redirecting
to Stripe's hosted page. Requires Stripe.js on the frontend.

## Backend

```go
sess, err := conn.Checkout.CreateSession(ctx, checkout.Input{
    SubjectID:   "org_acme",
    LineItems: []checkout.LineItem{{PriceKey: "pro.monthly_eur"}},
    UIMode:    checkout.UIModeEmbedded,
    ReturnURL: "https://app.example.com/return?cs={CHECKOUT_SESSION_ID}",
})

// Don't redirect — return the client_secret to the browser.
json.NewEncoder(w).Encode(map[string]string{
    "client_secret": sess.ClientSecret,
})
```

## Frontend

```html
<div id="checkout"></div>
<script src="https://js.stripe.com/v3/"></script>
<script>
const stripe = Stripe('pk_test_…');
fetch('/create-checkout-session', { method: 'POST' })
    .then(r => r.json())
    .then(({ client_secret }) => stripe.initEmbeddedCheckout({ clientSecret: client_secret }))
    .then(checkout => checkout.mount('#checkout'));
</script>
```

Full runnable demo: [`examples/embedded_frontend`](../../examples/embedded_frontend).

## Differences from hosted mode

- `UIMode = UIModeEmbedded` instead of default
- `ReturnURL` (with optional `{CHECKOUT_SESSION_ID}` placeholder) replaces `SuccessURL`/`CancelURL`
- `Session.URL` is empty; use `Session.ClientSecret` instead
- Apps own the visual chrome (header, layout) around the embedded element

The `metadata.app_namespace` stamp still flows; webhook routing works identically.
