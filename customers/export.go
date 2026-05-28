package customers

import (
	"context"
	"fmt"
	"time"

	"github.com/bds421/rho-stripe/credits"
	"github.com/bds421/rho-stripe/subject"
	"github.com/bds421/rho-stripe/subscriptions"
	stripe "github.com/stripe/stripe-go/v82"
)

// Export bundles every piece of customer data the lib + Stripe + app
// can produce, suitable for fulfilling a GDPR Subject Access Request
// (Article 15 of GDPR; "data portability" Article 20).
//
// Callers typically marshal this to JSON and hand it to the customer
// (or to a secure download URL). The lib does NOT persist exports —
// each call hits Stripe live and rebuilds the bundle.
type Export struct {
	SubjectID        SubjectID                     `json:"subject_id"`
	StripeCustomerID string                        `json:"stripe_customer_id"`
	GeneratedAt      time.Time                     `json:"generated_at"`
	Stripe           StripeCustomerExport          `json:"stripe"`
	Subscriptions    []*subscriptions.Subscription `json:"local_subscription_mirror,omitempty"`
	Credits          *CreditsExport                `json:"credits,omitempty"`
	App              map[string]any                `json:"app,omitempty"`
}

// StripeCustomerExport mirrors the Stripe-side state for the customer.
// We don't return raw *stripe.* types — they're not stable across
// stripe-go versions and they carry expansion-related fields users
// shouldn't see.
type StripeCustomerExport struct {
	Customer       *CustomerSnapshot        `json:"customer"`
	Subscriptions  []*SubscriptionSnapshot  `json:"subscriptions,omitempty"`
	Invoices       []*InvoiceSnapshot       `json:"invoices,omitempty"`
	Charges        []*ChargeSnapshot        `json:"charges,omitempty"`
	TaxIDs         []*TaxIDSnapshot         `json:"tax_ids,omitempty"`
	PaymentMethods []*PaymentMethodSnapshot `json:"payment_methods,omitempty"`
}

// CustomerSnapshot is a stable projection of stripe.Customer.
type CustomerSnapshot struct {
	ID          string            `json:"id"`
	Email       string            `json:"email,omitempty"`
	Name        string            `json:"name,omitempty"`
	Description string            `json:"description,omitempty"`
	Phone       string            `json:"phone,omitempty"`
	Currency    string            `json:"currency,omitempty"`
	Balance     int64             `json:"balance,omitempty"`
	Created     time.Time         `json:"created"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Address     *AddressSnapshot  `json:"address,omitempty"`
}

// AddressSnapshot mirrors stripe.Address.
type AddressSnapshot struct {
	Line1      string `json:"line1,omitempty"`
	Line2      string `json:"line2,omitempty"`
	City       string `json:"city,omitempty"`
	State      string `json:"state,omitempty"`
	PostalCode string `json:"postal_code,omitempty"`
	Country    string `json:"country,omitempty"`
}

// SubscriptionSnapshot projects stripe.Subscription for export.
type SubscriptionSnapshot struct {
	ID                string    `json:"id"`
	Status            string    `json:"status"`
	CurrentPeriodEnd  time.Time `json:"current_period_end,omitempty"`
	CancelAtPeriodEnd bool      `json:"cancel_at_period_end"`
	Created           time.Time `json:"created"`
}

// InvoiceSnapshot projects stripe.Invoice.
type InvoiceSnapshot struct {
	ID        string    `json:"id"`
	Number    string    `json:"number,omitempty"`
	Status    string    `json:"status"`
	Total     int64     `json:"total"`
	Currency  string    `json:"currency"`
	Created   time.Time `json:"created"`
	HostedURL string    `json:"hosted_url,omitempty"`
	PDFURL    string    `json:"pdf_url,omitempty"`
}

// ChargeSnapshot projects stripe.Charge.
type ChargeSnapshot struct {
	ID       string    `json:"id"`
	Amount   int64     `json:"amount"`
	Currency string    `json:"currency"`
	Status   string    `json:"status"`
	Paid     bool      `json:"paid"`
	Refunded bool      `json:"refunded"`
	Created  time.Time `json:"created"`
}

// TaxIDSnapshot projects stripe.TaxID.
type TaxIDSnapshot struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Value   string `json:"value"`
	Country string `json:"country,omitempty"`
}

// PaymentMethodSnapshot projects stripe.PaymentMethod (card-only for
// now; expand other types as needed).
type PaymentMethodSnapshot struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Last4    string `json:"last4,omitempty"`
	Brand    string `json:"brand,omitempty"`
	ExpMonth int64  `json:"exp_month,omitempty"`
	ExpYear  int64  `json:"exp_year,omitempty"`
}

// CreditsExport summarises ledger state for a subject.
type CreditsExport struct {
	Balances map[string]int64      `json:"balances"`
	Grants   []credits.Grant       `json:"grants,omitempty"`
	History  []credits.LedgerEntry `json:"history,omitempty"`
}

// Export returns every piece of data the lib + Stripe + app holds about
// a subject. Call from a GDPR "Subject Access Request" handler.
//
// Errors:
//   - if no Stripe customer is mapped → returns informative error
//     (treat as "we have no data on this subject")
//   - if Stripe call fails partially → returns the partial export +
//     the error (caller can choose to fulfil with what was retrieved)
func (o *Operations) Export(ctx context.Context, s SubjectID) (*Export, error) {
	stripeID, err := o.resolveStripeID(ctx, s)
	if err != nil {
		return nil, err
	}
	out := &Export{
		SubjectID:        s,
		StripeCustomerID: string(stripeID),
		GeneratedAt:      time.Now().UTC(),
	}
	out.Stripe, err = o.exportFromStripe(ctx, string(stripeID))
	if err != nil {
		return out, fmt.Errorf("customers.Export: stripe-side: %w", err)
	}
	if o.cfg.SubscriptionRepo != nil {
		subs, err := o.cfg.SubscriptionRepo.ListBySubject(ctx, subject.ID(s))
		if err != nil {
			return out, fmt.Errorf("customers.Export: subscription mirror: %w", err)
		}
		out.Subscriptions = subs
	}
	if o.cfg.CreditRepo != nil {
		cx, err := o.exportCredits(ctx, s)
		if err != nil {
			return out, fmt.Errorf("customers.Export: credits: %w", err)
		}
		out.Credits = cx
	}
	if o.cfg.AppDataExporter != nil {
		app, err := o.cfg.AppDataExporter(ctx, s)
		if err != nil {
			return out, fmt.Errorf("customers.Export: app-side: %w", err)
		}
		out.App = app
	}
	return out, nil
}

// exportFromStripe pulls Customer + Subscriptions + Invoices + Charges
// + TaxIDs + PaymentMethods for one customer. Each list is bounded to
// the most recent 100; apps with deep history should re-run periodically
// or extend this code with pagination cursors.
func (o *Operations) exportFromStripe(ctx context.Context, customerID string) (StripeCustomerExport, error) {
	out := StripeCustomerExport{}

	cust, err := o.cfg.StripeClient.V1Customers.Retrieve(ctx, customerID, nil)
	if err != nil {
		return out, fmt.Errorf("Customers.Retrieve(%s): %w", customerID, err)
	}
	out.Customer = projectCustomer(cust)

	// Subscriptions
	subListParams := &stripe.SubscriptionListParams{Customer: stripe.String(customerID)}
	subListParams.Filters.AddFilter("status", "", "all")
	subListParams.Filters.AddFilter("limit", "", "100")
	for s, err := range o.cfg.StripeClient.V1Subscriptions.List(ctx, subListParams) {
		if err != nil {
			return out, fmt.Errorf("Subscriptions.List: %w", err)
		}
		out.Subscriptions = append(out.Subscriptions, projectSubscriptionSnapshot(s))
	}

	// Invoices
	invListParams := &stripe.InvoiceListParams{Customer: stripe.String(customerID)}
	invListParams.Filters.AddFilter("limit", "", "100")
	for inv, err := range o.cfg.StripeClient.V1Invoices.List(ctx, invListParams) {
		if err != nil {
			return out, fmt.Errorf("Invoices.List: %w", err)
		}
		out.Invoices = append(out.Invoices, projectInvoiceSnapshot(inv))
	}

	// Charges
	chListParams := &stripe.ChargeListParams{Customer: stripe.String(customerID)}
	chListParams.Filters.AddFilter("limit", "", "100")
	for ch, err := range o.cfg.StripeClient.V1Charges.List(ctx, chListParams) {
		if err != nil {
			return out, fmt.Errorf("Charges.List: %w", err)
		}
		out.Charges = append(out.Charges, projectChargeSnapshot(ch))
	}

	// Tax IDs (one-shot list, no pagination — typical customers have ≤2)
	taxListParams := &stripe.TaxIDListParams{Customer: stripe.String(customerID)}
	for t, err := range o.cfg.StripeClient.V1TaxIDs.List(ctx, taxListParams) {
		if err != nil {
			return out, fmt.Errorf("TaxIDs.List: %w", err)
		}
		out.TaxIDs = append(out.TaxIDs, projectTaxIDSnapshot(t))
	}

	// Payment methods (card-only — extend by passing other types if needed)
	pmListParams := &stripe.PaymentMethodListParams{
		Customer: stripe.String(customerID),
		Type:     stripe.String("card"),
	}
	pmListParams.Filters.AddFilter("limit", "", "100")
	for pm, err := range o.cfg.StripeClient.V1PaymentMethods.List(ctx, pmListParams) {
		if err != nil {
			return out, fmt.Errorf("PaymentMethods.List: %w", err)
		}
		out.PaymentMethods = append(out.PaymentMethods, projectPaymentMethodSnapshot(pm))
	}

	return out, nil
}

func (o *Operations) exportCredits(ctx context.Context, s SubjectID) (*CreditsExport, error) {
	balances, err := o.cfg.CreditRepo.AllBalances(ctx, credits.SubjectID(s))
	if err != nil {
		return nil, err
	}
	grants, err := o.cfg.CreditRepo.ListBySubject(ctx, credits.SubjectID(s))
	if err != nil {
		return nil, err
	}
	bals := make(map[string]int64, len(balances))
	for bucket, b := range balances {
		bals[bucket] = b.Total
	}
	history, err := o.cfg.CreditRepo.History(ctx, credits.SubjectID(s), time.Time{})
	if err != nil {
		// History may be unsupported on some repos; treat as soft-fail.
		history = nil
	}
	return &CreditsExport{Balances: bals, Grants: grants, History: history}, nil
}

func projectCustomer(c *stripe.Customer) *CustomerSnapshot {
	if c == nil {
		return nil
	}
	out := &CustomerSnapshot{
		ID:          c.ID,
		Email:       c.Email,
		Name:        c.Name,
		Description: c.Description,
		Phone:       c.Phone,
		Currency:    string(c.Currency),
		Balance:     c.Balance,
		Created:     time.Unix(c.Created, 0).UTC(),
		Metadata:    c.Metadata,
	}
	if c.Address != nil {
		out.Address = &AddressSnapshot{
			Line1:      c.Address.Line1,
			Line2:      c.Address.Line2,
			City:       c.Address.City,
			State:      c.Address.State,
			PostalCode: c.Address.PostalCode,
			Country:    c.Address.Country,
		}
	}
	return out
}

func projectSubscriptionSnapshot(s *stripe.Subscription) *SubscriptionSnapshot {
	out := &SubscriptionSnapshot{
		ID:                s.ID,
		Status:            string(s.Status),
		CancelAtPeriodEnd: s.CancelAtPeriodEnd,
		Created:           time.Unix(s.Created, 0).UTC(),
	}
	if s.Items != nil {
		for _, item := range s.Items.Data {
			if item.CurrentPeriodEnd > 0 {
				out.CurrentPeriodEnd = time.Unix(item.CurrentPeriodEnd, 0).UTC()
				break
			}
		}
	}
	return out
}

func projectInvoiceSnapshot(i *stripe.Invoice) *InvoiceSnapshot {
	return &InvoiceSnapshot{
		ID:        i.ID,
		Number:    i.Number,
		Status:    string(i.Status),
		Total:     i.Total,
		Currency:  string(i.Currency),
		Created:   time.Unix(i.Created, 0).UTC(),
		HostedURL: i.HostedInvoiceURL,
		PDFURL:    i.InvoicePDF,
	}
}

func projectChargeSnapshot(c *stripe.Charge) *ChargeSnapshot {
	return &ChargeSnapshot{
		ID:       c.ID,
		Amount:   c.Amount,
		Currency: string(c.Currency),
		Status:   string(c.Status),
		Paid:     c.Paid,
		Refunded: c.Refunded,
		Created:  time.Unix(c.Created, 0).UTC(),
	}
}

func projectTaxIDSnapshot(t *stripe.TaxID) *TaxIDSnapshot {
	out := &TaxIDSnapshot{
		ID:    t.ID,
		Type:  string(t.Type),
		Value: t.Value,
	}
	if t.Country != "" {
		out.Country = t.Country
	}
	return out
}

func projectPaymentMethodSnapshot(p *stripe.PaymentMethod) *PaymentMethodSnapshot {
	out := &PaymentMethodSnapshot{
		ID:   p.ID,
		Type: string(p.Type),
	}
	if p.Card != nil {
		out.Last4 = p.Card.Last4
		out.Brand = string(p.Card.Brand)
		out.ExpMonth = p.Card.ExpMonth
		out.ExpYear = p.Card.ExpYear
	}
	return out
}
