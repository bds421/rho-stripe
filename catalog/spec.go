package catalog

import (
	"fmt"
	"sort"

	"github.com/bds421/rho-kit/core/v2/apperror"
)

// MustSpec validates s and returns a normalized *Spec. It panics on
// invalid input — catalog specs are constructed at process start, so
// any error is a programmer mistake that should fail loudly.
func MustSpec(s Spec) *Spec {
	out, err := NewSpec(s)
	if err != nil {
		panic(fmt.Sprintf("catalog.MustSpec: %v", err))
	}
	return out
}

// NewSpec validates s and returns a normalized *Spec, or an
// *apperror.ValidationError listing every problem found. Apps that
// load catalogs dynamically (rare) use this in place of MustSpec.
func NewSpec(s Spec) (*Spec, error) {
	var fields []apperror.FieldError

	if !namespaceRegex.MatchString(s.Namespace) {
		fields = append(fields, apperror.FieldError{
			Field:   "namespace",
			Message: "must match " + namespaceRegex.String(),
		})
	}

	if len(s.Products) == 0 {
		fields = append(fields, apperror.FieldError{
			Field:   "products",
			Message: "at least one product is required",
		})
	}

	normalizedProducts := make(map[string]Product, len(s.Products))
	for key, p := range s.Products {
		if !keyRegex.MatchString(key) {
			fields = append(fields, apperror.FieldError{
				Field:   "products." + key,
				Message: "key must match " + keyRegex.String(),
			})
			continue
		}
		normalized, productFields := validateProduct(key, p)
		fields = append(fields, productFields...)
		normalizedProducts[key] = normalized
	}

	normalizedCoupons := make(map[string]Coupon, len(s.Coupons))
	for key, c := range s.Coupons {
		if !couponKeyRegex.MatchString(key) {
			fields = append(fields, apperror.FieldError{
				Field:   "coupons." + key,
				Message: "key must match " + couponKeyRegex.String(),
			})
			continue
		}
		couponFields := validateCoupon(key, c)
		fields = append(fields, couponFields...)
		normalizedCoupons[key] = c
	}

	normalizedMeters := make(map[string]Meter, len(s.Meters))
	for key, m := range s.Meters {
		if !keyRegex.MatchString(key) {
			fields = append(fields, apperror.FieldError{
				Field: "meters." + key, Message: "key must match " + keyRegex.String(),
			})
			continue
		}
		if m.EventName == "" {
			m.EventName = key
		}
		if m.AggregateBy == "" {
			m.AggregateBy = "sum"
		}
		if m.DisplayName == "" {
			m.DisplayName = key
		}
		normalizedMeters[key] = m
	}

	// MeterRef on Prices must reference a declared Meter + only on recurring.
	for productKey, prod := range normalizedProducts {
		for priceKey, price := range prod.Prices {
			if price.MeterRef == "" {
				continue
			}
			if _, ok := normalizedMeters[price.MeterRef]; !ok {
				fields = append(fields, apperror.FieldError{
					Field:   fmt.Sprintf("products.%s.prices.%s.meter_ref", productKey, priceKey),
					Message: fmt.Sprintf("references undeclared Meter %q", price.MeterRef),
				})
			}
			if price.Type != PriceTypeRecurring && price.Type != "" {
				fields = append(fields, apperror.FieldError{
					Field:   fmt.Sprintf("products.%s.prices.%s.meter_ref", productKey, priceKey),
					Message: "meter_ref requires recurring price type",
				})
			}
		}
	}

	if len(fields) > 0 {
		return nil, &apperror.ValidationError{
			Message: "invalid catalog spec",
			Fields:  fields,
		}
	}

	return &Spec{
		Namespace: s.Namespace,
		Products:  normalizedProducts,
		Coupons:   normalizedCoupons,
		Meters:    normalizedMeters,
	}, nil
}

// NamespacedPriceKey returns the lookup_key the lib uses for a Price
// in Stripe (e.g. "demo.pro_plan.monthly_eur").
func (s *Spec) NamespacedPriceKey(productKey, priceKey string) string {
	return s.Namespace + "." + productKey + "." + priceKey
}

// AllLookupKeys returns every namespaced price lookup_key in the spec,
// sorted for determinism.
func (s *Spec) AllLookupKeys() []string {
	var out []string
	for pk, p := range s.Products {
		for prk := range p.Prices {
			out = append(out, s.NamespacedPriceKey(pk, prk))
		}
	}
	sort.Strings(out)
	return out
}

// NamespacedProductID returns the Stripe Product id the lib assigns
// (e.g. "prod_demo_pro_plan"). Stripe accepts custom Product ids.
func (s *Spec) NamespacedProductID(productKey string) string {
	return "prod_" + s.Namespace + "_" + productKey
}

// NamespacedCouponID returns the Stripe Coupon id (e.g. "SAVE20_demo").
func (s *Spec) NamespacedCouponID(couponKey string) string {
	return couponKey + "_" + s.Namespace
}

func validateProduct(key string, p Product) (Product, []apperror.FieldError) {
	prefix := "products." + key + "."
	var fields []apperror.FieldError

	if p.Name == "" {
		fields = append(fields, apperror.FieldError{
			Field: prefix + "name", Message: "required",
		})
	}
	if p.TaxCategory == "" {
		fields = append(fields, apperror.FieldError{
			Field: prefix + "tax_category", Message: "required",
		})
	}
	if len(p.Prices) == 0 {
		fields = append(fields, apperror.FieldError{
			Field: prefix + "prices", Message: "at least one price is required",
		})
	}

	normalizedPrices := make(map[string]Price, len(p.Prices))
	hasRecurring := false
	for priceKey, price := range p.Prices {
		if !keyRegex.MatchString(priceKey) {
			fields = append(fields, apperror.FieldError{
				Field:   prefix + "prices." + priceKey,
				Message: "key must match " + keyRegex.String(),
			})
			continue
		}
		normalized, priceFields := validatePrice(prefix+"prices."+priceKey+".", price)
		fields = append(fields, priceFields...)
		normalizedPrices[priceKey] = normalized
		if normalized.Type == PriceTypeRecurring {
			hasRecurring = true
		}
	}

	if p.RecurringGrant != nil {
		if !hasRecurring {
			fields = append(fields, apperror.FieldError{
				Field:   prefix + "recurring_grant",
				Message: "recurring_grant is only valid on products with recurring prices",
			})
		}
		if p.RecurringGrant.Bucket == "" {
			fields = append(fields, apperror.FieldError{Field: prefix + "recurring_grant.bucket", Message: "required"})
		}
		if p.RecurringGrant.Amount <= 0 {
			fields = append(fields, apperror.FieldError{Field: prefix + "recurring_grant.amount", Message: "must be positive"})
		}
		if p.RecurringGrant.ValidDaysFromGrant < 0 {
			fields = append(fields, apperror.FieldError{Field: prefix + "recurring_grant.valid_days_from_grant", Message: "must be non-negative"})
		}
	}

	if p.CreditGrant != nil {
		if hasRecurring {
			fields = append(fields, apperror.FieldError{
				Field:   prefix + "credit_grant",
				Message: "credit_grant is only valid on products with one_time prices",
			})
		}
		if p.CreditGrant.Bucket == "" {
			fields = append(fields, apperror.FieldError{
				Field: prefix + "credit_grant.bucket", Message: "required",
			})
		}
		if p.CreditGrant.Amount <= 0 {
			fields = append(fields, apperror.FieldError{
				Field: prefix + "credit_grant.amount", Message: "must be positive",
			})
		}
		if p.CreditGrant.ValidDays < 0 {
			fields = append(fields, apperror.FieldError{
				Field:   prefix + "credit_grant.valid_days",
				Message: "must be non-negative (0 = never expires)",
			})
		}
	}

	p.Prices = normalizedPrices
	return p, fields
}

func validatePrice(prefix string, p Price) (Price, []apperror.FieldError) {
	var fields []apperror.FieldError

	if p.Type == "" {
		p.Type = PriceTypeRecurring
	}
	if p.Type != PriceTypeRecurring && p.Type != PriceTypeOneTime {
		fields = append(fields, apperror.FieldError{
			Field:   prefix + "type",
			Message: `must be "recurring" or "one_time"`,
		})
	}

	if p.Amount < 0 {
		fields = append(fields, apperror.FieldError{
			Field: prefix + "amount", Message: "must be non-negative (0 = free tier)",
		})
	}
	if p.Currency == "" {
		fields = append(fields, apperror.FieldError{
			Field: prefix + "currency", Message: "required",
		})
	} else if !currencyRegex.MatchString(p.Currency) {
		fields = append(fields, apperror.FieldError{
			Field:   prefix + "currency",
			Message: "must be a 3-letter lowercase ISO 4217 code",
		})
	}

	switch p.Type {
	case PriceTypeRecurring:
		if p.Interval == "" {
			fields = append(fields, apperror.FieldError{
				Field:   prefix + "interval",
				Message: `required for recurring prices ("month"|"year"|"week"|"day")`,
			})
		}
		if p.IntervalCount == 0 {
			p.IntervalCount = 1
		}
	case PriceTypeOneTime:
		if p.Interval != "" {
			fields = append(fields, apperror.FieldError{
				Field:   prefix + "interval",
				Message: "must be empty for one_time prices",
			})
		}
		if p.IntervalCount != 0 {
			fields = append(fields, apperror.FieldError{
				Field:   prefix + "interval_count",
				Message: "must be zero for one_time prices",
			})
		}
	}

	return p, fields
}

func validateCoupon(key string, c Coupon) []apperror.FieldError {
	prefix := "coupons." + key + "."
	var fields []apperror.FieldError

	hasPercent := c.PercentOff != 0
	hasAmount := c.AmountOff != 0
	if hasPercent == hasAmount { // both or neither
		fields = append(fields, apperror.FieldError{
			Field:   "coupons." + key,
			Message: "exactly one of percent_off or amount_off must be set",
		})
	}

	if hasPercent && (c.PercentOff <= 0 || c.PercentOff > 100) {
		fields = append(fields, apperror.FieldError{
			Field:   prefix + "percent_off",
			Message: "must be in (0, 100]",
		})
	}

	if hasAmount {
		if c.AmountOff < 0 {
			fields = append(fields, apperror.FieldError{
				Field: prefix + "amount_off", Message: "must be positive",
			})
		}
		if c.Currency == "" {
			fields = append(fields, apperror.FieldError{
				Field:   prefix + "currency",
				Message: "required when amount_off is set",
			})
		} else if !currencyRegex.MatchString(c.Currency) {
			fields = append(fields, apperror.FieldError{
				Field:   prefix + "currency",
				Message: "must be a 3-letter lowercase ISO 4217 code",
			})
		}
	}

	switch c.Duration {
	case CouponDurationOnce, CouponDurationForever:
		// no extra requirements
	case CouponDurationRepeating:
		if c.DurationMonths <= 0 {
			fields = append(fields, apperror.FieldError{
				Field:   prefix + "duration_months",
				Message: "must be positive when duration is repeating",
			})
		}
	case "":
		fields = append(fields, apperror.FieldError{
			Field: prefix + "duration", Message: "required",
		})
	default:
		fields = append(fields, apperror.FieldError{
			Field:   prefix + "duration",
			Message: `must be "once" | "repeating" | "forever"`,
		})
	}

	return fields
}
