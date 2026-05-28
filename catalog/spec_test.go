package catalog_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/bds421/rho-kit/core/v2/apperror"
	"github.com/bds421/rho-stripe/catalog"
)

// validSpec returns a minimal but legal spec used as the baseline for
// "modify one thing and expect a specific failure" tests.
func validSpec() catalog.Spec {
	return catalog.Spec{
		Namespace: "demo",
		Products: map[string]catalog.Product{
			"pro_plan": {
				Name:        "Pro Plan",
				TaxCategory: catalog.TaxCategorySaaS,
				Prices: map[string]catalog.Price{
					"monthly_eur": {
						Amount:   4900,
						Currency: "eur",
						Type:     catalog.PriceTypeRecurring,
						Interval: catalog.IntervalMonth,
					},
				},
			},
		},
	}
}

func TestMustSpec_Valid(t *testing.T) {
	spec := validSpec()
	got := catalog.MustSpec(spec)
	if got == nil {
		t.Fatal("MustSpec returned nil for a valid spec")
	}
	if got.Namespace != "demo" {
		t.Errorf("Namespace = %q, want %q", got.Namespace, "demo")
	}
}

func TestNewSpec_Valid(t *testing.T) {
	got, err := catalog.NewSpec(validSpec())
	if err != nil {
		t.Fatalf("NewSpec returned error: %v", err)
	}
	if got == nil {
		t.Fatal("NewSpec returned nil for a valid spec")
	}
}

func TestNewSpec_NamespaceRequired(t *testing.T) {
	spec := validSpec()
	spec.Namespace = ""
	assertValidationField(t, spec, "namespace")
}

func TestNewSpec_NamespaceFormat(t *testing.T) {
	for _, ns := range []string{"App1", "1demo", "demo-app", "demo.app", "demo app", ""} {
		t.Run(ns, func(t *testing.T) {
			spec := validSpec()
			spec.Namespace = ns
			assertValidationField(t, spec, "namespace")
		})
	}
}

func TestNewSpec_ProductsRequired(t *testing.T) {
	spec := validSpec()
	spec.Products = nil
	assertValidationField(t, spec, "products")
}

func TestNewSpec_ProductKeyFormat(t *testing.T) {
	spec := validSpec()
	prod := spec.Products["pro_plan"]
	delete(spec.Products, "pro_plan")
	spec.Products["Pro.Plan"] = prod // illegal key
	assertValidationField(t, spec, "products")
}

func TestNewSpec_ProductNameRequired(t *testing.T) {
	spec := validSpec()
	p := spec.Products["pro_plan"]
	p.Name = ""
	spec.Products["pro_plan"] = p
	assertValidationField(t, spec, "name")
}

func TestNewSpec_ProductTaxCategoryRequired(t *testing.T) {
	spec := validSpec()
	p := spec.Products["pro_plan"]
	p.TaxCategory = ""
	spec.Products["pro_plan"] = p
	assertValidationField(t, spec, "tax_category")
}

func TestNewSpec_ProductPricesRequired(t *testing.T) {
	spec := validSpec()
	p := spec.Products["pro_plan"]
	p.Prices = nil
	spec.Products["pro_plan"] = p
	assertValidationField(t, spec, "prices")
}

func TestNewSpec_PriceAmountAllowsZeroFree(t *testing.T) {
	spec := validSpec()
	p := spec.Products["pro_plan"]
	price := p.Prices["monthly_eur"]
	price.Amount = 0 // free tier — must be accepted
	p.Prices["monthly_eur"] = price
	spec.Products["pro_plan"] = p
	if _, err := catalog.NewSpec(spec); err != nil {
		t.Errorf("Amount=0 (free tier) should be allowed, got %v", err)
	}
}

func TestNewSpec_PriceAmountRejectsNegative(t *testing.T) {
	spec := validSpec()
	p := spec.Products["pro_plan"]
	price := p.Prices["monthly_eur"]
	price.Amount = -1
	p.Prices["monthly_eur"] = price
	spec.Products["pro_plan"] = p
	assertValidationField(t, spec, "amount")
}

func TestNewSpec_PriceCurrencyRequired(t *testing.T) {
	spec := validSpec()
	p := spec.Products["pro_plan"]
	price := p.Prices["monthly_eur"]
	price.Currency = ""
	p.Prices["monthly_eur"] = price
	spec.Products["pro_plan"] = p
	assertValidationField(t, spec, "currency")
}

func TestNewSpec_PriceCurrencyFormat(t *testing.T) {
	spec := validSpec()
	p := spec.Products["pro_plan"]
	price := p.Prices["monthly_eur"]
	price.Currency = "EUR" // must be lowercase
	p.Prices["monthly_eur"] = price
	spec.Products["pro_plan"] = p
	assertValidationField(t, spec, "currency")
}

func TestNewSpec_RecurringRequiresInterval(t *testing.T) {
	spec := validSpec()
	p := spec.Products["pro_plan"]
	price := p.Prices["monthly_eur"]
	price.Interval = ""
	p.Prices["monthly_eur"] = price
	spec.Products["pro_plan"] = p
	assertValidationField(t, spec, "interval")
}

func TestNewSpec_OneTimeRejectsInterval(t *testing.T) {
	spec := validSpec()
	p := spec.Products["pro_plan"]
	price := p.Prices["monthly_eur"]
	price.Type = catalog.PriceTypeOneTime
	// price.Interval still set to month — should fail
	p.Prices["monthly_eur"] = price
	spec.Products["pro_plan"] = p
	assertValidationField(t, spec, "interval")
}

func TestNewSpec_CreditGrantRequiresOneTime(t *testing.T) {
	spec := validSpec()
	p := spec.Products["pro_plan"]
	p.CreditGrant = &catalog.CreditGrant{Bucket: "ai", Amount: 100, ValidDays: 30}
	spec.Products["pro_plan"] = p
	assertValidationField(t, spec, "credit_grant")
}

func TestNewSpec_CreditGrantValid(t *testing.T) {
	spec := validSpec()
	creditProduct := catalog.Product{
		Name:        "1000 AI Credits",
		TaxCategory: catalog.TaxCategorySaaS,
		Prices: map[string]catalog.Price{
			"default": {
				Amount:   1000,
				Currency: "eur",
				Type:     catalog.PriceTypeOneTime,
			},
		},
		CreditGrant: &catalog.CreditGrant{Bucket: "ai", Amount: 1000, ValidDays: 90},
	}
	spec.Products["credit_pack"] = creditProduct
	if _, err := catalog.NewSpec(spec); err != nil {
		t.Errorf("expected valid spec, got error: %v", err)
	}
}

func TestNewSpec_CreditGrantBucketRequired(t *testing.T) {
	spec := validSpec()
	spec.Products["credit_pack"] = catalog.Product{
		Name:        "Credits",
		TaxCategory: catalog.TaxCategorySaaS,
		Prices: map[string]catalog.Price{
			"default": {Amount: 1000, Currency: "eur", Type: catalog.PriceTypeOneTime},
		},
		CreditGrant: &catalog.CreditGrant{Bucket: "", Amount: 100, ValidDays: 30},
	}
	assertValidationField(t, spec, "bucket")
}

func TestNewSpec_CouponExclusiveDiscount(t *testing.T) {
	for name, c := range map[string]catalog.Coupon{
		"both set": {
			PercentOff: 10, AmountOff: 100, Currency: "eur",
			Duration: catalog.CouponDurationOnce,
		},
		"neither set": {
			Duration: catalog.CouponDurationOnce,
		},
	} {
		t.Run(name, func(t *testing.T) {
			spec := validSpec()
			spec.Coupons = map[string]catalog.Coupon{"SAVE": c}
			assertValidationField(t, spec, "coupons")
		})
	}
}

func TestNewSpec_CouponAmountOffRequiresCurrency(t *testing.T) {
	spec := validSpec()
	spec.Coupons = map[string]catalog.Coupon{
		"FIXED": {
			AmountOff: 500,
			Currency:  "",
			Duration:  catalog.CouponDurationOnce,
		},
	}
	assertValidationField(t, spec, "currency")
}

func TestNewSpec_CouponRepeatingRequiresMonths(t *testing.T) {
	spec := validSpec()
	spec.Coupons = map[string]catalog.Coupon{
		"REPEAT": {
			PercentOff:     10,
			Duration:       catalog.CouponDurationRepeating,
			DurationMonths: 0,
		},
	}
	assertValidationField(t, spec, "duration_months")
}

func TestNewSpec_CouponValid(t *testing.T) {
	spec := validSpec()
	spec.Coupons = map[string]catalog.Coupon{
		"SAVE20": {PercentOff: 20, Duration: catalog.CouponDurationOnce},
		"FIXED": {
			AmountOff: 500, Currency: "eur",
			Duration: catalog.CouponDurationOnce,
		},
		"YEARLY": {
			PercentOff:     10,
			Duration:       catalog.CouponDurationRepeating,
			DurationMonths: 12,
		},
	}
	if _, err := catalog.NewSpec(spec); err != nil {
		t.Errorf("expected valid spec, got error: %v", err)
	}
}

func TestNewSpec_PercentOffRange(t *testing.T) {
	for _, pct := range []float64{-1, 0, 100.5, 101} {
		t.Run("", func(t *testing.T) {
			spec := validSpec()
			spec.Coupons = map[string]catalog.Coupon{
				"BAD": {PercentOff: pct, Duration: catalog.CouponDurationOnce},
			}
			assertValidationField(t, spec, "percent_off")
		})
	}
}

func TestMustSpec_PanicsOnInvalid(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("MustSpec did not panic on invalid spec")
		}
	}()
	spec := validSpec()
	spec.Namespace = ""
	_ = catalog.MustSpec(spec)
}

func TestSpec_NamespacedPriceKey(t *testing.T) {
	spec := catalog.MustSpec(validSpec())
	got := spec.NamespacedPriceKey("pro_plan", "monthly_eur")
	want := "demo.pro_plan.monthly_eur"
	if got != want {
		t.Errorf("NamespacedPriceKey = %q, want %q", got, want)
	}
}

func TestSpec_NamespacedProductID(t *testing.T) {
	spec := catalog.MustSpec(validSpec())
	got := spec.NamespacedProductID("pro_plan")
	want := "prod_demo_pro_plan"
	if got != want {
		t.Errorf("NamespacedProductID = %q, want %q", got, want)
	}
}

func TestSpec_NamespacedCouponID(t *testing.T) {
	spec := validSpec()
	spec.Coupons = map[string]catalog.Coupon{
		"SAVE20": {PercentOff: 20, Duration: catalog.CouponDurationOnce},
	}
	got := catalog.MustSpec(spec).NamespacedCouponID("SAVE20")
	want := "SAVE20_demo"
	if got != want {
		t.Errorf("NamespacedCouponID = %q, want %q", got, want)
	}
}

// assertValidationField runs NewSpec on spec and asserts that the
// returned error is a *apperror.ValidationError whose Fields contain
// substr in either a Field or Message.
func assertValidationField(t *testing.T, spec catalog.Spec, substr string) {
	t.Helper()
	_, err := catalog.NewSpec(spec)
	if err == nil {
		t.Fatalf("expected validation error mentioning %q, got nil", substr)
	}
	var ve *apperror.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *apperror.ValidationError, got %T: %v", err, err)
	}
	for _, f := range ve.Fields {
		if strings.Contains(strings.ToLower(f.Field), substr) ||
			strings.Contains(strings.ToLower(f.Message), substr) {
			return
		}
	}
	if strings.Contains(strings.ToLower(ve.Message), substr) {
		return
	}
	t.Fatalf("ValidationError did not mention %q: %v", substr, ve)
}
