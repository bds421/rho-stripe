// Package subject defines the canonical billing-subject identifier
// the connector uses across packages (checkout, subscriptions,
// credits, metering).
//
// Per ADR-0002 the connector is neutral on what a subject is: for B2B
// apps it's the customer org, for B2C apps it's the end user. The
// connector treats it as an opaque, app-chosen string.
//
// Historically each package declared its own `SubjectID string`. As of
// slice 40 those declarations become type *aliases* of [ID], so the
// types are identical (and assignment-compatible) without forcing a
// callsite-wide rename.
//
// # Relationship to rho-kit tenant.ID
//
// rho-kit's [tenant.ID] is a similar opaque-string identifier but with
// tighter validation (rejects ':', '/', whitespace, NUL). It exists
// for cache-key safety and budget scoping. Apps that already use
// tenant.ID can convert with [FromTenant] / [ToTenant]; absent that,
// subject.ID accepts any string the app cares to use as a subject.
package subject

import (
	"github.com/bds421/rho-kit/core/v2/tenant"
)

// ID is the canonical opaque billing-subject identifier.
type ID string

// String returns the underlying string form (satisfies fmt.Stringer).
func (s ID) String() string { return string(s) }

// IsZero reports whether the ID is unset.
func (s ID) IsZero() bool { return s == "" }

// FromTenant converts a rho-kit tenant.ID into a subject.ID. The
// conversion is lossless and always succeeds; tenant.ID's validation
// is a strict subset of what we accept here.
func FromTenant(t tenant.ID) ID { return ID(t.String()) }

// ToTenant converts a subject.ID into a rho-kit tenant.ID. Returns
// an error wrapping tenant.ErrInvalid if the subject value contains
// any byte tenant.ID forbids (':', '/', whitespace, NUL, control
// chars). Apps that want guaranteed-safe interop should pick their
// subject scheme to be tenant-compatible.
func ToTenant(s ID) (tenant.ID, error) {
	return tenant.NewID(string(s))
}
