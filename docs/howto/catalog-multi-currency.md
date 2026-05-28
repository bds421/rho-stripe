# Multi-currency pricing

One Product, prices in EUR + USD + GBP + …

## Catalog declaration

```go
"pro": {
    Name:        "Pro Plan",
    TaxCategory: catalog.TaxCategorySaaSBusiness,
    Prices: map[string]catalog.Price{
        "monthly_eur": {Amount: 4900, Currency: "eur",
            Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
        "monthly_usd": {Amount: 5200, Currency: "usd",
            Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
        "monthly_gbp": {Amount: 4200, Currency: "gbp",
            Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalMonth},
        "yearly_eur":  {Amount: 49000, Currency: "eur",
            Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalYear},
        "yearly_usd":  {Amount: 52000, Currency: "usd",
            Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalYear},
        "yearly_gbp":  {Amount: 42000, Currency: "gbp",
            Type: catalog.PriceTypeRecurring, Interval: catalog.IntervalYear},
    },
},
```

Each currency = a separate Stripe Price object. `sync --apply`
creates them all.

## Apps that need price selection

The lib doesn't choose a currency for the customer — apps do, based
on their UX:

```go
priceKey := "pro.monthly_" + strings.ToLower(user.Currency)
sess, _ := conn.Checkout.CreateSession(ctx, checkout.Input{
    LineItems: []checkout.LineItem{{PriceKey: priceKey}},
    ...
})
```

Or pick a smarter strategy: geo-IP, account country, default to USD.

## Accepted currencies

The library accepts any ISO 4217 lowercase code Stripe supports
(EUR, USD, GBP, CHF, SEK, NOK, DKK, PLN, CZK, JPY, CAD, AUD, NZD,
SGD, HKD, INR, BRL, MXN, …). Multi-currency-test.go validates 21
common ones.

## What the lib does NOT do

- **No FX conversion.** App declares each price in each currency;
  the lib doesn't compute USD-from-EUR. Use Stripe's currency option
  for the destination charge or your own pricing-API.
- **No automatic locale-to-currency mapping.** App owns "user in DE
  → show €" UI logic.
- **No multi-currency subscriptions** (a single subscription can
  only have one currency). For mixed-currency carts, separate
  Checkouts.

## Stripe-side

Stripe Tax computes tax in the SAME currency as the price (no
cross-currency tax conversion needed). Stripe Invoices honor the
price's currency throughout the invoice lifecycle.
