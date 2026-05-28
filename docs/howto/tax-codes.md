# Picking the right TaxCategory

Stripe Tax uses a per-product tax code to determine the correct rate
per jurisdiction. The lib's `catalog.TaxCategory` is a typed string
that gets passed verbatim to Stripe.

The constants in `catalog/types.go` are convenience aliases for the
most-used Stripe codes (from
[docs.stripe.com/tax/tax-codes](https://docs.stripe.com/tax/tax-codes)).
**Any value of the form `"txcd_…"` works** — if your product needs a
code we don't expose, declare it inline:

```go
TaxCategory: catalog.TaxCategory("txcd_20030003"),  // Pet Grooming
```

## Pick what you SELL, not what your business does

A SaaS company selling a one-time e-book uses an e-book code, not a
SaaS code. The tax rate depends on what changed hands.

## Common picks

| What you sell | Constant |
|---|---|
| Generic B2B SaaS (Pro plan, Team plan) | `TaxCategorySaaSBusiness` (`txcd_10103001`) |
| Generic B2C SaaS (consumer app) | `TaxCategorySaaSPersonal` (`txcd_10103000`) |
| AI-powered SaaS (LLM wrapper, image gen) | `TaxCategoryAIaaSCloudBusiness` / `TaxCategoryAIaaSCloudPersonal` |
| Downloadable software (one-time desktop app) | `TaxCategoryDownloadableSoftwareBusiness` / `…Personal` |
| Custom-built software for one customer | `TaxCategoryDownloadableSoftwareCustomBusiness` / `…Personal` |
| Web hosting | `TaxCategoryWebsiteHosting` |
| Website ads | `TaxCategoryWebsiteAdvertising` |
| Cloud infrastructure (IaaS) | `TaxCategoryIaaSBusiness` / `…Personal` |
| Cloud platform (PaaS) | `TaxCategoryPaaSBusiness` / `…Personal` |
| E-book (one-time, permanent download) | `TaxCategoryDigitalBookDownloadPermanent` |
| Audiobook | `TaxCategoryAudiobook` |
| Music subscription (Spotify-like) | `TaxCategoryDigitalAudioStreamedSubscription` |
| Video subscription (Netflix-like) | `TaxCategoryDigitalVideoStreamedSubscription` |
| Digital magazine subscription | `TaxCategoryDigitalMagazineDownloadSubscription` |
| Online course (pre-recorded) | `TaxCategoryDigitalBookDownloadPermanent` (per Stripe docs; courses generally classified as "digital books" for tax) |
| Live online training / webinar | `TaxCategoryGeneralService` (services category) |
| Consulting | `TaxCategoryGeneralService` |
| Physical goods / merch | `TaxCategoryGeneralTangible` (charges general sales tax) |
| Truly nontaxable | `TaxCategoryNontaxable` (`txcd_00000000`) — different from "general"! |

## Personal vs Business variants

Many EU jurisdictions tax B2C and B2B differently. Stripe Tax uses
the `…Personal` vs `…Business` variant + the customer's tax status
(VAT-registered or not) to pick the right rate. If your app serves
both, use the variant that matches your **dominant** customer type
and let Stripe Tax handle reverse-charge edge cases.

## Verifying

After `sync --apply`, check your test-mode dashboard:
**Tax → Products → [your product] → Tax code** should match what you
declared. If a tax registration is missing for the jurisdiction your
test customer is in, Stripe will return a clear error during checkout
("Tax registration required in …").

## Updating the code

Tax code on an existing Stripe Product is **not updatable** via the
sync. To change it: archive the product (`UpdateProductActive=false`)
and create a new one with the desired tax code (which the diff does
automatically if you change `TaxCategory` in the spec — the old
product is archived, a new one is created with the new code,
subscriptions migrate at next renewal).
