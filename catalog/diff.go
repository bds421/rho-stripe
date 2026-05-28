package catalog

import (
	"fmt"
	"sort"
	"strings"

	"github.com/bds421/rho-stripe/meta"
)

// PlanOp is the kind of mutation a PlanItem describes.
type PlanOp string

const (
	OpCreate    PlanOp = "create"
	OpUpdate    PlanOp = "update"  // metadata-only update on a product
	OpReplace   PlanOp = "replace" // price values changed: create-new + archive-old + transfer lookup_key
	OpArchive   PlanOp = "archive"
	OpUnarchive PlanOp = "unarchive" // archived product re-added to spec → reactivate
	OpDrift     PlanOp = "drift"     // reported only; never applied
)

// PlanKind narrows a PlanItem to the Stripe object kind it targets.
type PlanKind string

const (
	KindProduct PlanKind = "product"
	KindPrice   PlanKind = "price"
	KindMeter   PlanKind = "meter"
)

// Plan is the result of Diff: an ordered list of mutations sync will
// perform on Apply, plus any DRIFT items it found and reported.
type Plan struct {
	Namespace string
	Items     []PlanItem
}

// PlanItem is one operation in a Plan.
type PlanItem struct {
	Op     PlanOp
	Kind   PlanKind
	Key    string // logical key (e.g. "pro_plan" or "pro_plan.monthly_eur")
	Reason string // human-readable
	// Product carries the values for Op=create on a product, OR the
	// existing values for Op=archive/drift on a product.
	NewProduct      *NewProduct
	ExistingProduct *ExistingProduct
	// Price equivalents for kind=price items.
	NewPrice      *NewPrice
	ExistingPrice *ExistingPrice
	// Meter equivalents for kind=meter items.
	NewMeter      *NewMeter
	ExistingMeter *ExistingMeter
}

// DiffWithMeters extends Diff to also reconcile meters. The two
// inputs (currentProducts, currentMeters) are fetched separately
// because Stripe lists them via different APIs.
func DiffWithMeters(spec *Spec, currentProducts []ExistingProduct, currentMeters []ExistingMeter) Plan {
	plan := Diff(spec, currentProducts)

	existingByEvent := make(map[string]ExistingMeter, len(currentMeters))
	for _, m := range currentMeters {
		existingByEvent[m.EventName] = m
	}
	for _, meterKey := range sortedKeys(spec.Meters) {
		m := spec.Meters[meterKey]
		namespacedEvent := spec.Namespace + "." + m.EventName
		if _, exists := existingByEvent[namespacedEvent]; !exists {
			plan.Items = append(plan.Items, PlanItem{
				Op:     OpCreate,
				Kind:   KindMeter,
				Key:    meterKey,
				Reason: "meter not present in Stripe",
				NewMeter: &NewMeter{
					EventName:   namespacedEvent,
					DisplayName: m.DisplayName,
					AggregateBy: m.AggregateBy,
				},
			})
		}
	}
	// Drift: meters with our namespace prefix not in spec — archive.
	for _, m := range currentMeters {
		prefix := spec.Namespace + "."
		if len(m.EventName) < len(prefix) || m.EventName[:len(prefix)] != prefix {
			continue
		}
		shortName := m.EventName[len(prefix):]
		declared := false
		for _, sm := range spec.Meters {
			if sm.EventName == shortName {
				declared = true
				break
			}
		}
		if !declared && m.Active {
			cm := m
			plan.Items = append(plan.Items, PlanItem{
				Op:            OpArchive,
				Kind:          KindMeter,
				Key:           shortName,
				Reason:        "meter no longer in spec",
				ExistingMeter: &cm,
			})
		}
	}
	return plan
}

// Diff produces a Plan that, when applied, brings the Stripe state
// matching the spec. Inputs:
//
//   - spec: the declared catalog (source of truth).
//   - current: every Stripe Product (with its Prices) currently in the
//     namespace, as returned by Backend.ListProductsByNamespace.
//
// Diff covers CREATE for missing products/prices, ARCHIVE for active
// items not in the spec, and DRIFT (reported, not applied) for any
// Stripe-side state that doesn't match a declared key. For Meter
// reconciliation use DiffWithMeters.
func Diff(spec *Spec, current []ExistingProduct) Plan {
	plan := Plan{Namespace: spec.Namespace}

	// Index current state by ID for quick lookup.
	currentByID := make(map[string]ExistingProduct, len(current))
	currentPriceByLookupKey := make(map[string]ExistingPrice)
	for _, p := range current {
		currentByID[p.ID] = p
		for _, price := range p.Prices {
			if price.LookupKey != "" {
				currentPriceByLookupKey[price.LookupKey] = price
			}
		}
	}

	// 1. Walk the spec — CREATE missing products/prices.
	for _, productKey := range sortedKeys(spec.Products) {
		product := spec.Products[productKey]
		stripeID := spec.NamespacedProductID(productKey)

		existing, exists := currentByID[stripeID]
		if !exists {
			plan.Items = append(plan.Items, PlanItem{
				Op:     OpCreate,
				Kind:   KindProduct,
				Key:    productKey,
				Reason: "product not present in Stripe",
				NewProduct: &NewProduct{
					ID:          stripeID,
					Name:        product.Name,
					TaxCode:     string(product.TaxCategory),
					Description: product.Description,
					Metadata:    namespacedMetadata(spec.Namespace, product.Metadata),
				},
			})
		} else if existing.Active && productNeedsUpdate(product, existing) {
			plan.Items = append(plan.Items, PlanItem{
				Op:     OpUpdate,
				Kind:   KindProduct,
				Key:    productKey,
				Reason: productUpdateReason(product, existing),
				NewProduct: &NewProduct{
					ID:          stripeID,
					Name:        product.Name,
					TaxCode:     string(product.TaxCategory),
					Description: product.Description,
					Metadata:    namespacedMetadata(spec.Namespace, product.Metadata),
				},
				ExistingProduct: &existing,
			})
		} else if !existing.Active {
			// Product was archived but is back in the spec; reactivate.
			plan.Items = append(plan.Items, PlanItem{
				Op:              OpUnarchive,
				Kind:            KindProduct,
				Key:             productKey,
				Reason:          "product is in spec but archived in Stripe; reactivating",
				ExistingProduct: &existing,
			})
		}

		// Walk this product's prices.
		for _, priceKey := range sortedKeys(product.Prices) {
			price := product.Prices[priceKey]
			lookupKey := spec.NamespacedPriceKey(productKey, priceKey)
			itemKey := productKey + "." + priceKey
			existingPrice, priceExists := currentPriceByLookupKey[lookupKey]

			switch {
			case !priceExists:
				plan.Items = append(plan.Items, PlanItem{
					Op:     OpCreate,
					Kind:   KindPrice,
					Key:    itemKey,
					Reason: "price not present in Stripe",
					NewPrice: &NewPrice{
						ProductID:        stripeID,
						LookupKey:        lookupKey,
						Amount:           price.Amount,
						Currency:         price.Currency,
						Type:             price.Type,
						Interval:         price.Interval,
						IntervalCount:    price.IntervalCount,
						Metadata:         namespacedMetadata(spec.Namespace, nil),
						TaxBehavior:      price.TaxBehavior,
						TaxRateOverrides: price.TaxRateOverrides,
					},
				})
			case priceNeedsReplace(price, existingPrice):
				plan.Items = append(plan.Items, PlanItem{
					Op:     OpReplace,
					Kind:   KindPrice,
					Key:    itemKey,
					Reason: priceReplaceReason(price, existingPrice),
					NewPrice: &NewPrice{
						ProductID:         stripeID,
						LookupKey:         lookupKey,
						Amount:            price.Amount,
						Currency:          price.Currency,
						Type:              price.Type,
						Interval:          price.Interval,
						IntervalCount:     price.IntervalCount,
						Metadata:          namespacedMetadata(spec.Namespace, nil),
						TransferLookupKey: true,
						TaxBehavior:       price.TaxBehavior,
						TaxRateOverrides:  price.TaxRateOverrides,
					},
					ExistingPrice: &existingPrice,
				})
			}
		}
	}

	// 2. Walk current Stripe state — ARCHIVE/DRIFT items not in spec.
	for _, productKey := range sortedProductKeys(current) {
		// Find the current product by its namespaced ID's suffix.
		// (Iterating in stable order over current.)
		var existing ExistingProduct
		for _, p := range current {
			if existingProductKey(p, spec.Namespace) == productKey {
				existing = p
				break
			}
		}

		_, inSpec := spec.Products[productKey]

		if !inSpec && existing.Active {
			// Product no longer declared — archive it (and any active
			// prices on it; ARCHIVE-price plan items are emitted below).
			plan.Items = append(plan.Items, PlanItem{
				Op:              OpArchive,
				Kind:            KindProduct,
				Key:             productKey,
				Reason:          "product no longer in spec",
				ExistingProduct: &existing,
			})
		}

		for _, ep := range existing.Prices {
			priceKeyInSpec := lookupKeyInSpec(spec, ep.LookupKey)
			if priceKeyInSpec == "" && ep.Active {
				// Active price whose lookup_key isn't declared.
				plan.Items = append(plan.Items, PlanItem{
					Op:            OpArchive,
					Kind:          KindPrice,
					Key:           keyFromLookupKey(spec.Namespace, ep.LookupKey),
					Reason:        "price no longer in spec",
					ExistingPrice: &ep,
				})
			}
			// Note: an archived price with no spec match is silently
			// skipped (expected after a previous sync that archived
			// it) — no else-branch needed.
		}
	}

	return plan
}

// Empty reports whether the plan has no actionable items. DRIFT items
// are reported only and never executed, so a drift-only plan is still
// "empty" for "apply did nothing" purposes.
func (p Plan) Empty() bool {
	for _, it := range p.Items {
		if it.Op != OpDrift {
			return false
		}
	}
	return true
}

// String renders the plan as a human-readable report suitable for the
// sync CLI's stdout.
func (p Plan) String() string {
	if len(p.Items) == 0 {
		return fmt.Sprintf("Sync plan for namespace %q: no changes.\n", p.Namespace)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Sync plan for namespace %q:\n", p.Namespace)
	for _, it := range p.Items {
		fmt.Fprintf(&b, "  %-8s %-7s %s\n", strings.ToUpper(string(it.Op)), it.Kind, it.Key)
		if it.Reason != "" {
			fmt.Fprintf(&b, "           reason: %s\n", it.Reason)
		}
	}
	return b.String()
}

// --- helpers ---

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sortedProductKeys extracts logical product keys from current Stripe
// products in stable order.
func sortedProductKeys(current []ExistingProduct) []string {
	seen := make(map[string]struct{}, len(current))
	for _, p := range current {
		// The trailing component after "prod_<namespace>_" is the key.
		// If the product doesn't match this pattern we skip it (drift
		// is detected by other means).
		k := stripeProductSuffix(p.ID)
		if k != "" {
			seen[k] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func existingProductKey(p ExistingProduct, namespace string) string {
	prefix := "prod_" + namespace + "_"
	if strings.HasPrefix(p.ID, prefix) {
		return strings.TrimPrefix(p.ID, prefix)
	}
	return ""
}

func stripeProductSuffix(id string) string {
	// Crude: strip "prod_<anything>_" prefix. Real namespace-aware
	// trimming happens via existingProductKey when we know the
	// namespace.
	idx := strings.Index(id[len("prod_"):], "_")
	if idx < 0 {
		return ""
	}
	return id[len("prod_")+idx+1:]
}

// lookupKeyInSpec returns the price key within the spec for which
// lookupKey would be the namespaced key, or "" if no match.
func lookupKeyInSpec(spec *Spec, lookupKey string) string {
	if !strings.HasPrefix(lookupKey, spec.Namespace+".") {
		return ""
	}
	rest := strings.TrimPrefix(lookupKey, spec.Namespace+".")
	parts := strings.SplitN(rest, ".", 2)
	if len(parts) != 2 {
		return ""
	}
	productKey, priceKey := parts[0], parts[1]
	if product, ok := spec.Products[productKey]; ok {
		if _, ok := product.Prices[priceKey]; ok {
			return productKey + "." + priceKey
		}
	}
	return ""
}

func keyFromLookupKey(namespace, lookupKey string) string {
	return strings.TrimPrefix(lookupKey, namespace+".")
}

func namespacedMetadata(namespace string, base map[string]string) map[string]string {
	out := make(map[string]string, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	out[meta.MetadataKeyNamespace] = namespace
	return out
}

// priceNeedsReplace reports whether a spec Price differs from its
// existing Stripe Price in a way that requires REPLACE (Stripe Prices
// are immutable on these fields).
func priceNeedsReplace(spec Price, existing ExistingPrice) bool {
	if !existing.Active {
		// An archived price with our lookup_key shouldn't normally
		// happen (transfer would have moved the key), but if it does
		// we treat it as needs-replace.
		return true
	}
	if spec.Amount != existing.Amount {
		return true
	}
	if spec.Currency != existing.Currency {
		return true
	}
	if spec.Type != existing.Type {
		return true
	}
	if spec.Type == PriceTypeRecurring {
		if spec.Interval != existing.Interval {
			return true
		}
		if spec.IntervalCount != existing.IntervalCount {
			return true
		}
	}
	return false
}

func priceReplaceReason(spec Price, existing ExistingPrice) string {
	switch {
	case spec.Amount != existing.Amount:
		return fmt.Sprintf("amount changed: %d → %d", existing.Amount, spec.Amount)
	case spec.Currency != existing.Currency:
		return fmt.Sprintf("currency changed: %s → %s", existing.Currency, spec.Currency)
	case spec.Type != existing.Type:
		return fmt.Sprintf("type changed: %s → %s", existing.Type, spec.Type)
	case spec.Interval != existing.Interval:
		return fmt.Sprintf("interval changed: %s → %s", existing.Interval, spec.Interval)
	case spec.IntervalCount != existing.IntervalCount:
		return fmt.Sprintf("interval_count changed: %d → %d", existing.IntervalCount, spec.IntervalCount)
	default:
		return "price values changed"
	}
}

// productNeedsUpdate reports whether the spec Product differs from
// the existing Stripe Product in fields the lib UPDATE can change
// (Name, Description). TaxCode is intentionally not updateable —
// Stripe restricts changes to tax_code on existing products.
func productNeedsUpdate(spec Product, existing ExistingProduct) bool {
	return spec.Name != existing.Name || spec.Description != existing.Description
}

func productUpdateReason(spec Product, existing ExistingProduct) string {
	switch {
	case spec.Name != existing.Name:
		return fmt.Sprintf("name changed: %q → %q", existing.Name, spec.Name)
	case spec.Description != existing.Description:
		return "description changed"
	default:
		return "product values changed"
	}
}
