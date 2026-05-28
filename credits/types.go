// Package credits implements the prepaid-credit ledger primitives.
// Phase-0 scope: the GRANT side only — credits get added when a
// catalog-declared CreditGrant product is purchased via checkout.
// The deduction side (TryDeduct / Balance / FIFO-by-expiry expiry)
// ships in phase 3 per docs/design/credits-ledger.md.
//
// Stripe has no native concept of "credits"; this package owns the
// ledger entirely. Stripe is the cashier (one-time payment), the
// CreditRepo here is the ledger book.
package credits

import (
	"time"

	"github.com/bds421/rho-stripe/subject"
)

// SubjectID identifies who owns the credit balance.
//
// As of slice 40 this is a type alias of [subject.ID] so values flow
// freely between the connector packages without conversion.
type SubjectID = subject.ID

// Source describes where a Grant came from. Used in audit reports.
type Source string

const (
	SourceStripePayment Source = "stripe_payment"
	SourceAdminGrant    Source = "admin_grant"
	SourceSignupBonus   Source = "signup_bonus"
	SourceRefund        Source = "refund"
)

// Grant is a single credit-issuance record persisted in the ledger.
type Grant struct {
	ID              string
	SubjectID         SubjectID
	Bucket          string
	AmountInitial   int64
	AmountRemaining int64
	GrantedAt       time.Time
	ExpiresAt       *time.Time // nil = never expires
	Source          Source
	SourceRef       string // e.g. "pi_NXxxx" for SourceStripePayment
	Metadata        map[string]string
}

// GrantInput is the parameter shape for CreditRepo.Grant.
type GrantInput struct {
	SubjectID   SubjectID
	Bucket    string
	Amount    int64
	ValidDays int // 0 = never expires
	Source    Source
	SourceRef string
	Metadata  map[string]string
}

// DeductInput is the parameter shape for CreditRepo.TryDeduct.
type DeductInput struct {
	SubjectID   SubjectID
	Bucket    string
	Amount    int64
	Reason    string // e.g. "api_call", "ai_completion"
	RequestID string // idempotency: same (Subject, RequestID) deducts once
	Metadata  map[string]string
}

// Deduction is one ledger debit row.
type Deduction struct {
	ID        string
	SubjectID   SubjectID
	Bucket    string
	GrantID   string
	Amount    int64
	Reason    string
	RequestID string
	CreatedAt time.Time
	Metadata  map[string]string
}

// Balance is the result of CreditRepo.Balance — total remaining in a
// subject/bucket plus useful aggregates for app UX.
type Balance struct {
	SubjectID    SubjectID
	Bucket     string
	Total      int64      // sum of amount_remaining across active grants
	NextExpiry *time.Time // earliest expires_at among active grants
	GrantCount int
}

// LedgerEntry is one row in History — either a Grant or a Deduction.
// Combined into a single sorted stream so apps can render a unified
// "credit activity" view.
type LedgerEntry struct {
	Type      EntryType
	Timestamp time.Time
	Bucket    string
	Amount    int64
	Reason    string // for deductions
	Source    Source // for grants
	SourceRef string // for grants
	GrantID   string // for grants; or for deductions, the grant they came from
}

// EntryType discriminates LedgerEntry.
type EntryType string

const (
	EntryTypeGrant     EntryType = "grant"
	EntryTypeDeduction EntryType = "deduction"
)
