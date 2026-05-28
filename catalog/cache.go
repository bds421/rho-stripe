package catalog

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// PriceLister abstracts the Stripe price-listing API the Cache depends
// on. The default implementation backed by stripe-go lives in the
// stripeapi package; tests inject fakes.
type PriceLister interface {
	// ListByLookupKeys returns every Stripe Price whose lookup_key is
	// in keys. Implementations may batch (Stripe accepts up to 10 keys
	// per list call); the Cache treats results as a single set.
	// Inactive (archived) prices may or may not be returned; the Cache
	// filters them out via ResolvedPrice.Active.
	ListByLookupKeys(ctx context.Context, keys []string) ([]ResolvedPrice, error)
}

// ResolvedPrice is the minimal projection of a Stripe Price the Cache
// needs. Implementations populate it from stripe-go's Price struct.
type ResolvedPrice struct {
	LookupKey string
	PriceID   string
	ProductID string
	Active    bool
}

// Cache maps namespaced lookup_keys to Stripe price IDs. It is warmed
// eagerly at connector startup (see adr-0003) and treats post-warmup
// cache misses as configuration errors rather than silently
// re-resolving — that surfaces "forgot to run sync" at startup instead
// of at first checkout.
type Cache struct {
	lister PriceLister
	mu     sync.RWMutex
	byKey  map[string]string
	warmed bool
}

// NewCache returns an empty Cache. Call Warm before Lookup.
func NewCache(lister PriceLister) *Cache {
	return &Cache{
		lister: lister,
		byKey:  make(map[string]string),
	}
}

// Warm resolves every lookup_key in spec against Stripe and populates
// the cache. It returns an error if any declared key is missing in
// Stripe (typically because sync has not been run, or a key was
// renamed without re-syncing).
func (c *Cache) Warm(ctx context.Context, spec *Spec) error {
	keys := spec.AllLookupKeys()
	if len(keys) == 0 {
		c.mu.Lock()
		c.warmed = true
		c.mu.Unlock()
		return nil
	}

	resolved, err := c.lister.ListByLookupKeys(ctx, keys)
	if err != nil {
		return fmt.Errorf("catalog: warm cache: %w", err)
	}

	next := make(map[string]string, len(resolved))
	for _, p := range resolved {
		if p.Active {
			next[p.LookupKey] = p.PriceID
		}
	}

	var missing []string
	for _, k := range keys {
		if _, ok := next[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"catalog: warm cache: %d lookup_key(s) missing in Stripe (run sync?): %s",
			len(missing), strings.Join(missing, ", "),
		)
	}

	c.mu.Lock()
	c.byKey = next
	c.warmed = true
	c.mu.Unlock()
	return nil
}

// Lookup returns the Stripe price_id for the namespaced lookup_key, or
// (zero, false) if the key is unknown. Per adr-0003, a miss after the
// cache is warmed indicates a configuration error in the caller (e.g.
// a typo, or a new key added without re-syncing); the cache does NOT
// silently re-resolve.
func (c *Cache) Lookup(namespacedKey string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	id, ok := c.byKey[namespacedKey]
	return id, ok
}

// Refresh re-runs Warm against the current spec; used after a runtime
// sync changes Stripe state.
//
// Concurrency: Refresh is atomic from the Lookup side — the rebuild
// happens against a private map outside the lock, then a single
// write-locked field swap publishes the new map. Concurrent Lookups
// see EITHER the old map OR the new map, never a partial state.
// A Refresh failure leaves the previous map intact (no torn state).
func (c *Cache) Refresh(ctx context.Context, spec *Spec) error {
	return c.Warm(ctx, spec)
}

// Warmed reports whether Warm has completed successfully at least once.
func (c *Cache) Warmed() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.warmed
}

// PriceKeyByStripeID is the reverse-lookup direction: given a Stripe
// price id, return the namespaced lookup_key from the catalog. Used
// by the subscriptions mirror to translate event payloads back to
// app-level keys.
//
// Returns ("", false) if no catalog entry maps to the id — usually
// because the price was created out-of-band (manually in the
// dashboard) or because the cache hasn't been refreshed since a sync.
func (c *Cache) PriceKeyByStripeID(stripeID string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for lookupKey, id := range c.byKey {
		if id == stripeID {
			return lookupKey, true
		}
	}
	return "", false
}
