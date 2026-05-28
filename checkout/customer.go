package checkout

import "github.com/bds421/rho-stripe/subject"

// StripeCustomerID is a typed string holding a "cus_..." Stripe id.
// Aliases [subject.StripeCustomerID] so callers can pass values across
// package boundaries without conversion. The canonical definition lives
// in `subject` (alongside [SubjectID] / [CustomerRepo]).
type StripeCustomerID = subject.StripeCustomerID

// CustomerRepo persists the SubjectID → StripeCustomerID mapping.
// Alias of [subject.CustomerRepo]; the canonical definition lives in
// `subject` so packages don't need to import `checkout` just to satisfy
// the contract.
type CustomerRepo = subject.CustomerRepo

// NewMemoryCustomerRepo returns an in-memory CustomerRepo. Thin
// re-export of [subject.NewMemoryCustomerRepo] so existing call sites
// don't need to update their imports.
func NewMemoryCustomerRepo() CustomerRepo { return subject.NewMemoryCustomerRepo() }
