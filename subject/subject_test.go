package subject_test

import (
	"errors"
	"testing"

	"github.com/bds421/rho-kit/core/v2/tenant"
	"github.com/bds421/rho-stripe/checkout"
	"github.com/bds421/rho-stripe/credits"
	"github.com/bds421/rho-stripe/metering"
	"github.com/bds421/rho-stripe/subject"
	"github.com/bds421/rho-stripe/subscriptions"
)

func TestAliasesAreCompatible(t *testing.T) {
	// All four package SubjectID types are aliases of subject.ID, so
	// they are assignment-compatible without conversion. Explicit
	// type declarations are intentional documentation of the alias
	// relationship — staticcheck's ST1023 would prefer inference.
	//lint:file-ignore ST1023 explicit types document alias compatibility
	var s subject.ID = "org_acme"
	var c checkout.SubjectID = s
	var cr credits.SubjectID = s
	var sub subscriptions.SubjectID = s
	var m metering.SubjectID = s
	if string(c) != "org_acme" || string(cr) != "org_acme" || string(sub) != "org_acme" || string(m) != "org_acme" {
		t.Errorf("alias assignment lost the value")
	}
}

func TestStringAndIsZero(t *testing.T) {
	var z subject.ID
	if !z.IsZero() {
		t.Error("zero subject.ID should be IsZero")
	}
	id := subject.ID("acme")
	if id.IsZero() || id.String() != "acme" {
		t.Errorf("non-zero broke: %q IsZero=%v", id.String(), id.IsZero())
	}
}

func TestFromTenant(t *testing.T) {
	tid := tenant.MustNewID("acme-prod")
	sid := subject.FromTenant(tid)
	if string(sid) != "acme-prod" {
		t.Errorf("FromTenant lost value: %q", sid)
	}
}

func TestToTenant_OK(t *testing.T) {
	tid, err := subject.ToTenant(subject.ID("acme-prod"))
	if err != nil {
		t.Fatalf("ToTenant: %v", err)
	}
	if tid.String() != "acme-prod" {
		t.Errorf("ToTenant lost value: %q", tid)
	}
}

func TestToTenant_RejectsForbiddenChars(t *testing.T) {
	// tenant.ID forbids ':' (would collide with cache key separators).
	_, err := subject.ToTenant(subject.ID("ten:ant"))
	if !errors.Is(err, tenant.ErrInvalid) {
		t.Errorf("want tenant.ErrInvalid, got %v", err)
	}

	// And whitespace.
	_, err = subject.ToTenant(subject.ID("ten ant"))
	if !errors.Is(err, tenant.ErrInvalid) {
		t.Errorf("want tenant.ErrInvalid for whitespace, got %v", err)
	}
}
