# Embedded Checkout — frontend example

A minimal full-stack demo showing how the Go library's
`UIModeEmbedded` session integrates with Stripe.js on the
browser side.

The library is **backend-only**. This folder exists so you can copy
the frontend snippets verbatim (HTML + 30 lines of JS) into your own
app's templates without writing them from scratch.

## Layout

- `index.html` — single-page demo loading Stripe.js and mounting the
  embedded Payment Element.
- `server.go` — a tiny Go HTTP server that serves the page and
  exposes `POST /create-checkout-session` returning the
  `client_secret` from the library.

## Run

```bash
set -a; source .env; set +a

# 1. Apply a catalog so the price exists in Stripe.
go run ./examples/saas_tiers/main sync --apply

# 2. Start the demo server.
go run ./examples/embedded_frontend  # listens on :8090

# 3. Open http://localhost:8090 in a browser.
#    Complete the embedded form with test card 4242 4242 4242 4242.
```

## Snippet to copy into your app

The interesting part is in `index.html`:

```html
<div id="checkout"></div>
<script src="https://js.stripe.com/v3/"></script>
<script>
  const stripe = Stripe('pk_test_…');  // your publishable key

  fetch('/create-checkout-session', { method: 'POST' })
    .then(r => r.json())
    .then(({ clientSecret }) => stripe.initEmbeddedCheckout({ clientSecret }))
    .then(checkout => checkout.mount('#checkout'));
</script>
```

That's it on the frontend. The library handles everything backend-side:

```go
sess, _ := conn.Checkout.CreateSession(ctx, checkout.Input{
    SubjectID:   "org_acme",
    LineItems: []checkout.LineItem{{PriceKey: "standard.monthly_eur"}},
    UIMode:    checkout.UIModeEmbedded,
    ReturnURL: "https://your.app/return?cs={CHECKOUT_SESSION_ID}",
})
// sess.ClientSecret → return to the browser
```
