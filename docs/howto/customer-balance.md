# Customer balance

Stripe's built-in `customer.balance` — a money amount on the Customer
object that auto-applies to future invoices. Distinct from this lib's
`credits` ledger.

## When to use which

| Need | Use |
|---|---|
| Per-unit consumable (API calls, voice minutes, AI tokens) | `credits` package |
| Credit-on-account that pays down NEXT Stripe invoice automatically | `customer_balance` (this) |
| Goodwill credit ("here's €100 toward your next bill") | `customer_balance` |
| Bank transfer / wire payments that pre-fund the customer | `customer_balance` (Stripe auto-applies) |

## API

```go
// Credit the customer €100 (negative amount = credit = "we owe them"):
tx, err := conn.Checkout.AdjustCustomerBalance(ctx, "cus_xyz", -10000, "eur", "Goodwill credit")

// Read current balance:
bal, err := conn.Checkout.GetCustomerBalance(ctx, "cus_xyz")
// bal.Balance is negative when the customer has credit
fmt.Printf("balance: %d %s\n", bal.Balance, bal.Currency)

// History:
txs, err := conn.Checkout.ListCustomerBalanceTransactions(ctx, "cus_xyz", 50)
```

## Sign convention

Stripe's convention (preserved by the lib):
- **Negative balance** = customer has CREDIT (will pay less on next invoice)
- **Positive balance** = customer OWES (will pay more on next invoice)

To grant a €100 credit, pass `Amount: -10000` (€100 = 10000 cents,
negative = credit).

## What Stripe does with the balance

On the customer's next Stripe invoice, Stripe automatically applies
the balance:
- Credit balance → reduces the invoice total (down to zero)
- Debit balance → adds to the invoice total

No app code involved — Stripe owns the application logic.

## Webhook events

- `customer.updated` fires when the balance changes via `Update`.
- `customer_balance_transaction.created` fires for each adjustment.

Both go to `OnOtherEvent` today (no typed handler — let me know if
you need one).

## Common patterns

**Goodwill credit:**
```go
conn.Checkout.AdjustCustomerBalance(ctx, custID, -2500, "eur", "Apology — outage on 2026-03-15")
```

**Pre-payment of an enterprise contract:**
```go
conn.Checkout.AdjustCustomerBalance(ctx, custID, -100000, "eur", "Q1 2026 prepayment per contract")
// Stripe auto-applies to each monthly invoice until depleted.
```

**Bank-transfer top-ups** (when payment_method=customer_balance):
Stripe credits the customer balance automatically when the wire
arrives — no app code needed.
