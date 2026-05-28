package catalog

import (
	"context"
	"fmt"
)

// Apply executes plan against backend in dependency-safe order:
//  1. CREATE products (so prices have a parent to attach to).
//  2. UPDATE products (metadata-only; safe at any point but grouped here).
//  3. CREATE prices.
//  4. REPLACE prices (create-new-and-transfer-lookup-key, then archive old).
//  5. ARCHIVE prices (so the parent product becomes archivable).
//  6. ARCHIVE products.
//
// DRIFT items are skipped (reported only). The method is idempotent:
// re-running after a successful apply produces no changes when the
// backend's state matches the spec, because Diff will yield an empty
// plan.
//
// On the first error, Apply stops and returns it. Partially-applied
// state is acceptable: re-running picks up where it left off (Stripe
// API uses idempotency keys downstream when the stripeapi backend is
// in use, so retried creates don't duplicate).
func Apply(ctx context.Context, backend Backend, plan Plan) error {
	for _, it := range itemsByOpKind(plan, OpCreate, KindProduct) {
		if _, err := backend.CreateProduct(ctx, *it.NewProduct); err != nil {
			return fmt.Errorf("catalog.Apply: create product %q: %w", it.Key, err)
		}
	}

	for _, it := range itemsByOpKind(plan, OpUnarchive, KindProduct) {
		if err := backend.UpdateProductActive(ctx, it.ExistingProduct.ID, true); err != nil {
			return fmt.Errorf("catalog.Apply: unarchive product %q: %w", it.Key, err)
		}
	}

	for _, it := range itemsByOpKind(plan, OpUpdate, KindProduct) {
		update := ProductUpdate{
			Name:        it.NewProduct.Name,
			Description: it.NewProduct.Description,
			Metadata:    it.NewProduct.Metadata,
		}
		if err := backend.UpdateProduct(ctx, it.ExistingProduct.ID, update); err != nil {
			return fmt.Errorf("catalog.Apply: update product %q: %w", it.Key, err)
		}
	}

	for _, it := range itemsByOpKind(plan, OpCreate, KindPrice) {
		if _, err := backend.CreatePrice(ctx, *it.NewPrice); err != nil {
			return fmt.Errorf("catalog.Apply: create price %q: %w", it.Key, err)
		}
	}

	// REPLACE = create new (with transfer_lookup_key=true) + archive old.
	// The transfer is atomic on Stripe's side, so a partial failure
	// between create and archive leaves a stale orphan that the next
	// run will detect (its lookup_key has moved to the new price, so
	// it appears as drift) and archive on a later sync.
	for _, it := range itemsByOpKind(plan, OpReplace, KindPrice) {
		if _, err := backend.CreatePrice(ctx, *it.NewPrice); err != nil {
			return fmt.Errorf("catalog.Apply: replace (create new) price %q: %w", it.Key, err)
		}
		if err := backend.UpdatePriceActive(ctx, it.ExistingPrice.ID, false); err != nil {
			return fmt.Errorf("catalog.Apply: replace (archive old) price %q: %w", it.Key, err)
		}
	}

	for _, it := range itemsByOpKind(plan, OpArchive, KindPrice) {
		if err := backend.UpdatePriceActive(ctx, it.ExistingPrice.ID, false); err != nil {
			return fmt.Errorf("catalog.Apply: archive price %q: %w", it.Key, err)
		}
	}

	for _, it := range itemsByOpKind(plan, OpArchive, KindProduct) {
		if err := backend.UpdateProductActive(ctx, it.ExistingProduct.ID, false); err != nil {
			return fmt.Errorf("catalog.Apply: archive product %q: %w", it.Key, err)
		}
	}

	for _, it := range itemsByOpKind(plan, OpCreate, KindMeter) {
		if _, err := backend.CreateMeter(ctx, *it.NewMeter); err != nil {
			return fmt.Errorf("catalog.Apply: create meter %q: %w", it.Key, err)
		}
	}
	for _, it := range itemsByOpKind(plan, OpArchive, KindMeter) {
		if err := backend.ArchiveMeter(ctx, it.ExistingMeter.ID); err != nil {
			return fmt.Errorf("catalog.Apply: archive meter %q: %w", it.Key, err)
		}
	}

	return nil
}

func itemsByOpKind(plan Plan, op PlanOp, kind PlanKind) []PlanItem {
	var out []PlanItem
	for _, it := range plan.Items {
		if it.Op == op && it.Kind == kind {
			out = append(out, it)
		}
	}
	return out
}
