# Disabling B2B defaults for B2C apps

The library ships with B2B-friendly Checkout defaults: automatic tax,
tax-ID collection, required billing address, customer address+name
auto-update, promo codes allowed. B2C apps usually want most of this off.

## Disable everything

```go
no := false
sess, err := conn.Checkout.CreateSession(ctx, checkout.Input{
    SubjectID:    "user_alice",
    LineItems:  []checkout.LineItem{{PriceKey: "premium.monthly_eur"}},
    SuccessURL: "https://app.example.com/s",
    CancelURL:  "https://app.example.com/c",
    Defaults: checkout.SessionDefaults{
        AutomaticTax:             &no, // pointer to false explicitly
        TaxIDCollection:          &no,
        BillingAddressCollection: "auto",  // don't require, just collect when given
        AllowPromotionCodes:      &no,
    },
})
```

## Per-field summary

| Field | Default | B2C override | Why |
|---|---|---|---|
| `AutomaticTax` | `true` (pointer to true via fallback) | `&false` | Stripe Tax adds friction; B2C apps without nexus complexity skip it |
| `TaxIDCollection` | `true` | `&false` | Consumers don't have VAT-IDs |
| `BillingAddressCollection` | `"required"` | `"auto"` | Reduce form length on mobile |
| `CustomerUpdateAddress` | `"auto"` | `"never"` | Keep customer record untouched |
| `CustomerUpdateName` | `"auto"` | `"never"` | Same |
| `AllowPromotionCodes` | `true` | `&false` | Hide the "Got a code?" field |
| `RequirePaymentMethodForTrial` | `true` | `&false` | Useful for "no card required" trials (see [trials.md](trials.md)) |

## Field-level granularity

Override only what you need; un-set fields keep the library default.

```go
Defaults: checkout.SessionDefaults{
    AutomaticTax: &no,  // skip tax
    // ... everything else stays default
}
```

Pointer-to-bool encoding is intentional: `nil` means "use the lib's
default", `&true` and `&false` are explicit. This lets the lib add
new defaults later without forcing every existing caller to update.
