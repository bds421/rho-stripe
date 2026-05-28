package payments_test

import (
	"slices"
	"testing"

	"github.com/bds421/rho-stripe/payments"
)

func TestMethodsFor_KnownIntents(t *testing.T) {
	for _, tc := range []struct {
		intent payments.Intent
		want   []string
	}{
		{payments.IntentSubscription, []string{"card", "sepa_debit", "apple_pay", "google_pay"}},
		{payments.IntentOneTime, []string{"card", "sepa_debit", "apple_pay", "google_pay"}},
		{payments.IntentCreditTopUp, []string{"card", "apple_pay", "google_pay"}},
		{payments.IntentInvoice, []string{"customer_balance", "card"}},
	} {
		t.Run(string(tc.intent), func(t *testing.T) {
			got := payments.MethodsFor(tc.intent)
			if !slices.Equal(got, tc.want) {
				t.Errorf("MethodsFor(%q) = %v, want %v", tc.intent, got, tc.want)
			}
		})
	}
}

func TestMethodsFor_UnknownReturnsNil(t *testing.T) {
	if got := payments.MethodsFor("nonsense"); got != nil {
		t.Errorf("MethodsFor unknown = %v, want nil", got)
	}
}

func TestMethodsFor_ReturnsCopy(t *testing.T) {
	a := payments.MethodsFor(payments.IntentSubscription)
	a[0] = "MUTATED"
	b := payments.MethodsFor(payments.IntentSubscription)
	if b[0] == "MUTATED" {
		t.Error("MethodsFor returned a shared backing array; callers can corrupt the preset")
	}
}

func TestExportedPresets_NonEmpty(t *testing.T) {
	for name, p := range map[string][]string{
		"PresetSubscription": payments.PresetSubscription,
		"PresetOneTime":      payments.PresetOneTime,
		"PresetCreditTopUp":  payments.PresetCreditTopUp,
		"PresetInvoice":      payments.PresetInvoice,
	} {
		if len(p) == 0 {
			t.Errorf("%s is empty", name)
		}
	}
}
