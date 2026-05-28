package stripeapi_test

// This is a static-source-code regression test that scans every Go
// source file in the stripeapi package and asserts that any function
// which calls *.AddMetadata also references an app_namespace value
// (either directly or transitively via a metadata map passed in).
//
// The motivation: the high-level "namespace stamping auto-test" in
// connectortest only covers Operations facades. New stripeapi backend
// methods that create Stripe objects with AddMetadata but forget to
// propagate app_namespace would slip past that test. This test reads
// the package's own source, so it catches forgotten stamps at the
// AddMetadata call site itself.
//
// Heuristic:
//   - Find every file in stripeapi/.
//   - For each function that contains "AddMetadata" or sets a
//     `Metadata` map literal:
//       - Either it must reference `app_namespace` literally, OR
//       - It must be in the allowlist (functions that pass through a
//         caller-supplied map and rely on the Operations layer to
//         stamp before calling them).
//
// Update the allowlist when adding a new pass-through method; that
// edit is the signal that you considered (and explicitly opted out
// of) backend-side stamping.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// passThroughFuncs lists stripeapi functions that accept caller-
// supplied metadata and rely on the Operations layer to have stamped
// it. Adding a function here is an explicit "we know this doesn't
// stamp; the Operations facade above us does."
var passThroughFuncs = map[string]bool{
	// CheckoutBackend: stamping happens in checkout.sessionMetadata
	// before SessionCreate.Metadata is populated.
	"CheckoutBackend.CreateCustomer":        true,
	"CheckoutBackend.CreateCheckoutSession": true,

	// SubscriptionBackend.CreateSubscriptionSchedule: stamping happens
	// in subscriptions.Operations.stampNamespace.
	"SubscriptionBackend.CreateSubscriptionSchedule": true,
	"SubscriptionBackend.CreateInvoiced":             true,

	// CouponBackend.CreatePromoCode: stamping happens in coupons.
	// Operations.CreatePromoCode via the stampNamespace helper.
	"CouponBackend.CreatePromoCode": true,

	// InvoiceBackend.CreateDraft: stamping happens in invoices.
	// Operations.CreateDraft via stampNamespace.
	"InvoiceBackend.CreateDraft": true,

	// RefundBackend.Refund: stamping happens in payments.Operations.Refund.
	"RefundBackend.Refund": true,

	// CheckoutBackend.createAdHocCustomPrice: receives sessionMeta from
	// the parent CreateCheckoutSession call which itself receives
	// already-stamped metadata from checkout.sessionMetadata. The
	// stamp flows through the ProductData / Price params verbatim.
	"CheckoutBackend.createAdHocCustomPrice": true,

	// SubscriptionBackend.buildAddInvoiceItemParams: receives sessionMeta
	// from the parent CreateInvoiced call which itself receives
	// already-stamped metadata via subscriptions.Operations.stampNamespace.
	"SubscriptionBackend.buildAddInvoiceItemParams": true,

	// Backend (catalog): CreateProduct/CreatePrice/UpdateProduct all
	// receive metadata that catalog/diff.go's namespacedMetadata helper
	// has already stamped at PlanItem construction time, so the
	// stripeapi backend just forwards the map verbatim.
	"Backend.CreateProduct": true,
	"Backend.CreatePrice":   true,
	"Backend.UpdateProduct": true,
}

func TestStripeAPI_EveryMetadataSettingFuncStampsOrIsAllowlisted(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no Go files found in stripeapi/")
	}

	fset := token.NewFileSet()
	violations := []string{}

	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		f, err := parser.ParseFile(fset, path, src, parser.AllErrors)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}

		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := funcFullName(fn)
			start := fset.Position(fn.Body.Pos()).Offset
			end := fset.Position(fn.Body.End()).Offset
			if start < 0 || end > len(src) || start >= end {
				continue
			}
			bodyStr := string(src[start:end])

			// Heuristic: does the body call AddMetadata on a Stripe
			// param object? That's how stripeapi writes metadata to
			// Stripe-bound requests. "Metadata:" in a struct literal
			// is too broad (matches projections that READ a Stripe
			// object's metadata into our types).
			if !strings.Contains(bodyStr, "AddMetadata(") {
				continue
			}
			// Allowed if either the body stamps the namespace literal
			// OR the function is in the pass-through allowlist.
			stampsLiterally := strings.Contains(bodyStr, "app_namespace")
			if stampsLiterally || passThroughFuncs[name] {
				continue
			}
			violations = append(violations, name+" (in "+path+")")
		}
	}

	if len(violations) > 0 {
		t.Errorf(`stripeapi functions that touch metadata but neither stamp app_namespace nor appear in passThroughFuncs:

  %s

Either (a) stamp app_namespace inside the function, OR (b) add the function name
to passThroughFuncs in this file with a comment explaining which Operations
facade above it does the stamping. The latter is the right choice when a
caller is expected to have already stamped the metadata map.`, strings.Join(violations, "\n  "))
	}
}

// funcFullName returns "Receiver.Method" for methods, or just the
// function name for plain functions.
func funcFullName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	recv := fn.Recv.List[0].Type
	// Strip the leading * if present.
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	ident, ok := recv.(*ast.Ident)
	if !ok {
		return fn.Name.Name
	}
	return ident.Name + "." + fn.Name.Name
}
