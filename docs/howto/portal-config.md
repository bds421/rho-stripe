# Customer Portal configuration (declarative sync)

Stripe's hosted Customer Portal exposes a per-account configuration:
which products customers can self-serve switch to, which fields they
can edit, whether cancellation happens immediately or at period end,
etc. Usually apps configure this once in the Stripe Dashboard, then
forget about it — drift between code expectations and dashboard
reality bites months later.

Declare the desired config in Go, sync at deploy time, treat the
same as catalog: PR-reviewable, drift-detectable.

## Declare

```go
var portalSpec = &portalconfig.Spec{
    Namespace:        "myapp",
    Name:             "My App billing portal",
    DefaultReturnURL: "https://myapp.com/account",
    BusinessHeadline: "Manage your subscription",
    PrivacyPolicyURL: "https://myapp.com/privacy",
    TermsOfServiceURL: "https://myapp.com/terms",

    Features: portalconfig.Features{
        InvoiceHistoryEnabled:      true,
        PaymentMethodUpdateEnabled: true,
        CustomerUpdate: portalconfig.CustomerUpdateFeature{
            Enabled:       true,
            AllowedFields: []string{"address", "email", "phone", "tax_id"},
        },
        SubscriptionCancel: portalconfig.SubscriptionCancelFeature{
            Enabled:           true,
            Mode:              "at_period_end",
            ProrationBehavior: "none",
        },
        SubscriptionUpdate: portalconfig.SubscriptionUpdateFeature{
            Enabled:               true,
            DefaultAllowedUpdates: []string{"price", "quantity", "promotion_code"},
            ProrationBehavior:     "create_prorations",
            Products: []portalconfig.SubscriptionUpdateProduct{
                {StripeProductID: "prod_…", StripePriceIDs: []string{"price_pro_monthly", "price_pro_yearly"}},
                {StripeProductID: "prod_…", StripePriceIDs: []string{"price_team_monthly"}},
            },
        },
    },
}
```

## Sync at deploy time

```go
res, err := conn.PortalConfig.Sync(ctx, portalSpec)
switch {
case res.Created:
    log.Info("portal config created", "id", res.ConfigID)
case res.Updated:
    log.Info("portal config updated", "id", res.ConfigID)
case res.Unchanged:
    log.Debug("portal config matches spec; no change")
}
```

Sync compares the spec to the existing config and only issues a
Stripe write when there's a real diff — re-running on every deploy
is free.

## Namespace

The config is found / persisted by `metadata.app_namespace`. Multiple
apps in one Stripe account each maintain their own portal config
under their own namespace.

## Limitations

The diff currently compares: `DefaultReturnURL`, `BusinessHeadline`,
`PrivacyPolicyURL`, `TermsOfServiceURL`, and each feature's enabled
flag. Sub-fields (the per-feature allowed-fields lists, the Products
list under SubscriptionUpdate) are NOT diffed — if you change those,
the next Sync issues an Update unconditionally. This is a deliberate
trade-off: those collections are harder to canonicalize cheaply.

## Using the config in a session

The portal session itself doesn't need a configuration id — Stripe
auto-uses the most recently active one for the account. To force a
specific configuration, pass `ConfigurationID` on `PortalSessionCreate`
(advanced, rarely needed).
