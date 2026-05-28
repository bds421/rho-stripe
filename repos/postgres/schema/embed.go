// Package schema embeds the reference Postgres DDL so consumers can
// apply it programmatically (via their migration tool of choice).
// The lib itself never runs migrations at runtime.
package schema

import _ "embed"

//go:embed 0001_initial.sql
var Initial string

//go:embed 0002_subscriptions.sql
var Subscriptions string

//go:embed 0003_credit_deductions.sql
var CreditDeductions string

//go:embed 0004_usage.sql
var Usage string

//go:embed 0005_invoice_numbers.sql
var InvoiceNumbers string

//go:embed 0006_webhook_queue.sql
var WebhookQueue string

// All returns every migration file in apply-order across phases:
// Initial (0) → Subscriptions (1) → CreditDeductions (3) → Usage (5) →
// InvoiceNumbers (6) → WebhookQueue (7).
func All() []string {
	return []string{Initial, Subscriptions, CreditDeductions, Usage, InvoiceNumbers, WebhookQueue}
}
