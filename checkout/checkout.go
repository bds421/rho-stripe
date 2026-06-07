package checkout

import (
	"context"
	"errors"
	"fmt"

	"github.com/bds421/rho-stripe/catalog"
	"github.com/bds421/rho-stripe/credits"
	"github.com/bds421/rho-stripe/meta"
)

// PriceResolver maps namespaced lookup_keys to Stripe price ids. The
// production implementation is *catalog.Cache.
type PriceResolver interface {
	Lookup(namespacedKey string) (priceID string, ok bool)
}

// Config configures Checkout.
type Config struct {
	// Namespace stamps every Stripe object created by this package via
	// metadata.app_namespace. Must match the catalog Spec's Namespace.
	Namespace string

	// Spec is the catalog spec; used to look up Price types (one_time
	// vs recurring) for Mode inference. We don't need the full Cache
	// for that — just the Spec — but we'll separately accept a
	// PriceResolver for lookup_key → stripe id resolution.
	Spec *catalog.Spec

	// Resolver maps lookup_key → stripe price id. Production: *catalog.Cache.
	Resolver PriceResolver

	// Customers persists subject ↔ StripeCustomerID across sessions.
	Customers CustomerRepo

	// Backend is the Stripe-side adapter (stripeapi.CheckoutBackend
	// in production; a fake in tests).
	Backend Backend
}

// Checkout is the public API of the package.
type Checkout struct {
	cfg Config
}

// New constructs a Checkout. Panics on missing required config.
func New(cfg Config) *Checkout {
	if cfg.Namespace == "" {
		panic("checkout.New: Namespace is required")
	}
	if cfg.Spec == nil {
		panic("checkout.New: Spec is required")
	}
	if cfg.Resolver == nil {
		panic("checkout.New: Resolver is required")
	}
	if cfg.Customers == nil {
		panic("checkout.New: Customers is required")
	}
	if cfg.Backend == nil {
		panic("checkout.New: Backend is required")
	}
	return &Checkout{cfg: cfg}
}

// Errors surfaced from CreateSession.
var (
	ErrSubjectMissing       = errors.New("checkout: Input.SubjectID is required")
	ErrNoLineItems          = errors.New("checkout: at least one LineItem is required")
	ErrPriceKeyNotFound     = errors.New("checkout: price key not resolvable (cache warmed? sync run?)")
	ErrMixedItemTypes       = errors.New("checkout: line items must be all recurring or all one-time")
	ErrMissingSuccessURL    = errors.New("checkout: SuccessURL is required")
	ErrMissingCancelURL     = errors.New("checkout: CancelURL is required")
	ErrUnknownPriceInSpec   = errors.New("checkout: price key not declared in catalog spec")
	ErrMissingReturnURL     = errors.New("checkout: ReturnURL is required when UIMode is embedded")
	ErrTrialDaysOutOfRange  = errors.New("checkout: TrialDays must be between 1 and 730 (Stripe's hard cap)")
	ErrTrialOnPayment       = errors.New("checkout: TrialDays is only valid for subscription-mode sessions (got one-time line items)")
	ErrCustomAmountAndPrice = errors.New("checkout: LineItem cannot have both PriceKey and CustomAmount set")
	ErrCustomAmountInvalid  = errors.New("checkout: CustomAmount requires Currency + Name + Min>=1 + Max>=Min")
)

// CreateSession resolves logical price keys to Stripe ids, resolves
// (or creates) the subject's Stripe Customer, and creates a Checkout
// Session. Returns the hosted URL the app redirects the customer to.
func (c *Checkout) CreateSession(ctx context.Context, in Input) (Session, error) {
	if err := validateInput(in); err != nil {
		return Session{}, err
	}

	resolved, mode, err := c.resolveLineItems(in.LineItems)
	if err != nil {
		return Session{}, err
	}

	if in.TrialDays != 0 {
		if in.TrialDays < 1 || in.TrialDays > 730 {
			return Session{}, ErrTrialDaysOutOfRange
		}
		if mode != ModeSubscription {
			return Session{}, ErrTrialOnPayment
		}
	}

	customerID, err := c.resolveCustomer(ctx, in.SubjectID, in.CustomerEmail)
	if err != nil {
		return Session{}, err
	}

	metadata, err := c.sessionMetadata(in)
	if err != nil {
		return Session{}, err
	}

	params := SessionCreate{
		StripeCustomerID: customerID,
		Mode:             mode,
		LineItems:        resolved,
		SuccessURL:       in.SuccessURL,
		CancelURL:        in.CancelURL,
		ClientReference:  string(in.Actor),
		PromoCode:        in.PromoCode,
		PromoCodeID:      in.PromoCodeID,
		Metadata:         metadata,
		PaymentMethods:   in.PaymentMethods,
		UIMode:           in.UIMode,
		ReturnURL:        in.ReturnURL,
		Defaults:         in.Defaults,
		TrialDays:        in.TrialDays,
		Locale:           in.Locale,
	}

	return c.cfg.Backend.CreateCheckoutSession(ctx, params)
}

func (c *Checkout) resolveLineItems(items []LineItem) ([]SessionLineItem, Mode, error) {
	out := make([]SessionLineItem, 0, len(items))
	var sawRecurring, sawOneTime bool

	for _, item := range items {
		// Pay-what-you-want path: bypass catalog resolution.
		if item.CustomAmount != nil {
			if item.PriceKey != "" {
				return nil, "", ErrCustomAmountAndPrice
			}
			if err := validateCustomAmount(item.CustomAmount); err != nil {
				return nil, "", err
			}
			qty := item.Quantity
			if qty == 0 {
				qty = 1
			}
			out = append(out, SessionLineItem{CustomAmount: item.CustomAmount, Quantity: qty})
			// Custom amounts are always one-time (Stripe doesn't allow
			// custom_unit_amount on recurring prices).
			sawOneTime = true
			continue
		}
		productKey, priceKey, ok := splitPriceKey(item.PriceKey)
		if !ok {
			return nil, "", fmt.Errorf("%w: %q is not in the form product.price", ErrPriceKeyNotFound, item.PriceKey)
		}
		product, productOK := c.cfg.Spec.Products[productKey]
		if !productOK {
			return nil, "", fmt.Errorf("%w: product %q", ErrUnknownPriceInSpec, productKey)
		}
		specPrice, priceOK := product.Prices[priceKey]
		if !priceOK {
			return nil, "", fmt.Errorf("%w: price %q on product %q", ErrUnknownPriceInSpec, priceKey, productKey)
		}

		namespaced := c.cfg.Spec.NamespacedPriceKey(productKey, priceKey)
		stripeID, ok := c.cfg.Resolver.Lookup(namespaced)
		if !ok {
			return nil, "", fmt.Errorf("%w: %q (run sync first)", ErrPriceKeyNotFound, namespaced)
		}

		qty := item.Quantity
		if qty == 0 {
			qty = 1
		}
		out = append(out, SessionLineItem{StripePriceID: stripeID, Quantity: qty})

		switch specPrice.Type {
		case catalog.PriceTypeRecurring, "":
			sawRecurring = true
		case catalog.PriceTypeOneTime:
			sawOneTime = true
		}
	}

	if sawRecurring && sawOneTime {
		return nil, "", ErrMixedItemTypes
	}
	mode := ModeSubscription
	if sawOneTime {
		mode = ModePayment
	}
	return out, mode, nil
}

// resolveCustomer returns the existing Stripe Customer id for subject,
// creating one (and persisting the mapping) if none exists.
func (c *Checkout) resolveCustomer(ctx context.Context, subject SubjectID, email string) (StripeCustomerID, error) {
	if id, ok, err := c.cfg.Customers.Get(ctx, subject); err != nil {
		return "", fmt.Errorf("customers.Get: %w", err)
	} else if ok {
		return id, nil
	}

	created, err := c.cfg.Backend.CreateCustomer(ctx, CustomerCreate{
		SubjectID: subject,
		Email:     email,
		Metadata: map[string]string{
			meta.MetadataKeyNamespace: c.cfg.Namespace,
			"subject_id":    string(subject),
		},
	})
	if err != nil {
		return "", fmt.Errorf("backend.CreateCustomer: %w", err)
	}
	if err := c.cfg.Customers.Upsert(ctx, subject, created); err != nil {
		// Stripe Customer exists but we failed to persist the mapping;
		// subsequent calls will see no mapping and create another
		// Stripe Customer. Better to surface the error than to lose
		// the link silently.
		return "", fmt.Errorf("customers.Upsert: %w", err)
	}
	return created, nil
}

func (c *Checkout) sessionMetadata(in Input) (map[string]string, error) {
	out := make(map[string]string, len(in.Metadata)+4)
	for k, v := range in.Metadata {
		out[k] = v
	}
	out[meta.MetadataKeyNamespace] = c.cfg.Namespace
	out["subject_id"] = string(in.SubjectID)
	if in.Actor != "" {
		out["actor_id"] = string(in.Actor)
	}

	// Stamp the credit-grant intent so the webhook auto-handler can
	// apply grants on checkout.session.completed without a follow-up
	// Stripe lookup. Only applies to one-time line items whose Product
	// declares a CreditGrant.
	pending := c.collectPendingGrants(in.LineItems)
	if len(pending) > 0 {
		encoded, err := credits.EncodeSessionMetadata(pending)
		if err != nil {
			return nil, err
		}
		out[credits.SessionMetadataKey] = encoded
	}
	return out, nil
}

func (c *Checkout) collectPendingGrants(items []LineItem) []credits.PendingGrant {
	var out []credits.PendingGrant
	for _, item := range items {
		productKey, _, ok := splitPriceKey(item.PriceKey)
		if !ok {
			continue
		}
		product, ok := c.cfg.Spec.Products[productKey]
		if !ok || product.CreditGrant == nil {
			continue
		}
		qty := item.Quantity
		if qty == 0 {
			qty = 1
		}
		out = append(out, credits.PendingGrant{
			Bucket:     product.CreditGrant.Bucket,
			Amount:     product.CreditGrant.Amount,
			ValidDays:  product.CreditGrant.ValidDays,
			ProductKey: productKey,
			Quantity:   qty,
		})
	}
	return out
}

func validateInput(in Input) error {
	if in.SubjectID == "" {
		return ErrSubjectMissing
	}
	if len(in.LineItems) == 0 {
		return ErrNoLineItems
	}
	if in.UIMode == UIModeEmbedded {
		// Embedded mode uses ReturnURL instead of SuccessURL/CancelURL.
		if in.ReturnURL == "" {
			return ErrMissingReturnURL
		}
		return nil
	}
	if in.SuccessURL == "" {
		return ErrMissingSuccessURL
	}
	if in.CancelURL == "" {
		return ErrMissingCancelURL
	}
	return nil
}

func validateCustomAmount(ca *CustomAmount) error {
	if ca.Currency == "" || ca.Name == "" || ca.Min < 1 || ca.Max < ca.Min {
		return ErrCustomAmountInvalid
	}
	if ca.Preset != 0 && (ca.Preset < ca.Min || ca.Preset > ca.Max) {
		return ErrCustomAmountInvalid
	}
	return nil
}

// splitPriceKey parses "product.price" into its parts. Returns
// ok=false if the input doesn't contain exactly one dot.
func splitPriceKey(key string) (product, price string, ok bool) {
	dot := -1
	for i := 0; i < len(key); i++ {
		if key[i] == '.' {
			if dot >= 0 {
				return "", "", false
			}
			dot = i
		}
	}
	if dot <= 0 || dot >= len(key)-1 {
		return "", "", false
	}
	return key[:dot], key[dot+1:], true
}
