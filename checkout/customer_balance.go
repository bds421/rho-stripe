package checkout

import (
	"context"
	"errors"
	"time"
)

// CustomerBalance helpers wrap Stripe's customer_balance — a money
// balance attached to the Customer object, distinct from the lib's
// own credits ledger.
//
// When to use which:
//
//   - **credits package** (FIFO-by-expiry, multi-bucket, app-side
//     enforcement): for prepaid units consumed inside your app
//     (API calls, transcription minutes, AI completions). The library
//     owns the ledger; the app decides when to deduct.
//
//   - **customer_balance** (here): Stripe's built-in field on the
//     Customer used to apply credits / debits against future Stripe
//     invoices automatically. Typical use: B2B credit-on-account
//     (e.g. customer pre-paid €1000; subsequent invoices auto-debit
//     until exhausted), refund credit (refund applied as balance for
//     future use), or write-off adjustments.
//
// Conceptually: credits = your in-app currency; customer_balance =
// Stripe-managed cash credit toward Stripe invoices.
//
// API:
//
//   conn.Checkout.GetCustomerBalance(ctx, cus_id)
//   conn.Checkout.AdjustCustomerBalance(ctx, cus_id, amount, currency, description)
//   conn.Checkout.ListCustomerBalanceTransactions(ctx, cus_id, limit)
//
// Amounts are in the smallest currency unit. Positive amounts are
// CREDITS to the customer (reduce what they owe on next invoice).
// Negative amounts are DEBITS (increase what they owe).

// CustomerBalance is the projection of a Stripe Customer's balance
// field. Always denominated in the Customer's currency.
type CustomerBalance struct {
	StripeCustomerID StripeCustomerID
	Currency         string
	Balance          int64 // negative = customer owes; positive = credit toward future invoices
}

// CustomerBalanceTransaction is one row in the customer's balance history.
type CustomerBalanceTransaction struct {
	StripeID    string
	Type        string // "adjustment", "credit_note", "applied_to_invoice", …
	Amount      int64  // smallest unit
	Currency    string
	Description string
	InvoiceID   string // populated for applied_to_invoice / invoice_overpaid
	CreatedAt   time.Time
}

// AdjustCustomerBalance applies a credit (positive amount) or debit
// (negative amount) to the customer's balance. Stripe's API requires
// the currency to match the customer's settled currency.
//
// Typical use: a finance person grants a €100 goodwill credit:
//
//	conn.Checkout.AdjustCustomerBalance(ctx, "cus_x", -10000, "eur", "Goodwill credit")
//
// Stripe convention: balance is NEGATIVE when the customer has a
// credit (they owe less). The adjustment amount you pass should be
// the delta you want applied — Stripe inverts the sign internally,
// so use the convention "negative = credit them, positive = charge them"
// like in this library, OR use the helper signs from the constants below.
func (c *Checkout) AdjustCustomerBalance(ctx context.Context, customerID StripeCustomerID, amount int64, currency, description string) (CustomerBalanceTransaction, error) {
	if customerID == "" {
		return CustomerBalanceTransaction{}, errors.New("checkout.AdjustCustomerBalance: customerID is required")
	}
	if amount == 0 {
		return CustomerBalanceTransaction{}, errors.New("checkout.AdjustCustomerBalance: amount must be non-zero")
	}
	if currency == "" {
		return CustomerBalanceTransaction{}, errors.New("checkout.AdjustCustomerBalance: currency is required")
	}
	be, ok := c.cfg.Backend.(CustomerBalanceBackend)
	if !ok {
		return CustomerBalanceTransaction{}, errors.New("checkout: backend does not implement CustomerBalanceBackend (use stripeapi.NewCheckoutBackend or a fake that implements both)")
	}
	return be.AdjustCustomerBalance(ctx, customerID, amount, currency, description)
}

// GetCustomerBalance returns the customer's current balance.
func (c *Checkout) GetCustomerBalance(ctx context.Context, customerID StripeCustomerID) (CustomerBalance, error) {
	if customerID == "" {
		return CustomerBalance{}, errors.New("checkout.GetCustomerBalance: customerID is required")
	}
	be, ok := c.cfg.Backend.(CustomerBalanceBackend)
	if !ok {
		return CustomerBalance{}, errors.New("checkout: backend does not implement CustomerBalanceBackend")
	}
	return be.GetCustomerBalance(ctx, customerID)
}

// ListCustomerBalanceTransactions returns the customer's balance-
// transaction history, newest first.
func (c *Checkout) ListCustomerBalanceTransactions(ctx context.Context, customerID StripeCustomerID, limit int) ([]CustomerBalanceTransaction, error) {
	if customerID == "" {
		return nil, errors.New("checkout.ListCustomerBalanceTransactions: customerID is required")
	}
	be, ok := c.cfg.Backend.(CustomerBalanceBackend)
	if !ok {
		return nil, errors.New("checkout: backend does not implement CustomerBalanceBackend")
	}
	return be.ListCustomerBalanceTransactions(ctx, customerID, limit)
}

// CustomerBalanceBackend is an optional extension interface — backends
// implement it to enable the customer_balance helpers above. The
// stripeapi.CheckoutBackend implements it; bare-fake backends can opt
// out by not implementing.
type CustomerBalanceBackend interface {
	AdjustCustomerBalance(ctx context.Context, customerID StripeCustomerID, amount int64, currency, description string) (CustomerBalanceTransaction, error)
	GetCustomerBalance(ctx context.Context, customerID StripeCustomerID) (CustomerBalance, error)
	ListCustomerBalanceTransactions(ctx context.Context, customerID StripeCustomerID, limit int) ([]CustomerBalanceTransaction, error)
}
