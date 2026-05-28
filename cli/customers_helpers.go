package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/bds421/rho-stripe/checkout"
	"github.com/bds421/rho-stripe/customers"
	"github.com/bds421/rho-stripe/stripeapi"
	stripe "github.com/stripe/stripe-go/v82"
)

// customersOpsFor wires customers.Operations against an in-memory
// CustomerRepo seeded by Stripe metadata lookup. The CLI looks up the
// Stripe Customer by app_subject_id metadata, since the CLI process
// doesn't carry the app's persistent subject→customer table.
//
// Apps stamp app_subject_id on customer creation via
// Checkout.CreateCustomer (the connector does this automatically).
func (o Options) customersOpsFor(secret string) (*customers.Operations, error) {
	if secret == "" {
		return nil, errors.New("STRIPE_SECRET_KEY is not set")
	}
	sc := stripeapi.NewClient(stripeapi.Config{SecretKey: secret})
	return customers.New(customers.Config{
		StripeClient: sc,
		CustomerRepo: &stripeMetadataCustomerRepo{sc: sc},
	}), nil
}

func customersSubjectID(s string) customers.SubjectID {
	return customers.SubjectID(s)
}

func customersAddTaxIDInput(subjectID, typ, value string) customers.AddTaxIDInput {
	return customers.AddTaxIDInput{
		SubjectID: customers.SubjectID(subjectID),
		Type:      typ,
		Value:     value,
	}
}

func customersImportInput(subjectID, stripeID, namespace string, adopt, backfillSubs bool) customers.ImportCustomerInput {
	return customers.ImportCustomerInput{
		SubjectID:             customers.SubjectID(subjectID),
		StripeCustomerID:      stripeID,
		Namespace:             namespace,
		AdoptNamespace:        adopt,
		BackfillSubscriptions: backfillSubs,
	}
}

func jsonEncoder(w io.Writer) *json.Encoder {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc
}

// stripeMetadataCustomerRepo resolves SubjectID → StripeCustomerID via
// Stripe metadata lookup (Customers.Search). Suitable for CLI-only
// ad-hoc operations where the app's persistent repo isn't accessible.
//
// IMPORTANT caveats vs. an app-supplied CustomerRepo:
//
//   - Search API has eventual consistency (typically a few seconds).
//     A customer just created via Checkout may not be findable yet.
//     Retry if a CLI op needs strict read-after-write.
//
//   - Search is currently free on Stripe accounts; if Stripe later
//     gates it behind a paid tier, this CLI path stops working.
//
//   - Per-account search latency is higher than a Get-by-id (~200ms vs.
//     ~50ms). Tolerable for interactive CLI use; do NOT use this repo
//     in app hot paths — apps should implement CustomerRepo against
//     their own DB.
//
// Upsert is a no-op since the CLI doesn't create customers.
type stripeMetadataCustomerRepo struct {
	sc *stripe.Client
}

func (r *stripeMetadataCustomerRepo) Get(ctx context.Context, s checkout.SubjectID) (checkout.StripeCustomerID, bool, error) {
	// Stripe Search query: metadata['app_subject_id']:'<subject>'
	params := &stripe.CustomerSearchParams{
		SearchParams: stripe.SearchParams{
			Query: `metadata["app_subject_id"]:"` + string(s) + `"`,
		},
	}
	for c, err := range r.sc.V1Customers.Search(ctx, params) {
		if err != nil {
			return "", false, err
		}
		return checkout.StripeCustomerID(c.ID), true, nil
	}
	return "", false, nil
}

func (r *stripeMetadataCustomerRepo) Upsert(_ context.Context, _ checkout.SubjectID, _ checkout.StripeCustomerID) error {
	return nil
}
