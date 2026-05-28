// Package plans encapsulates the "what can this customer do" question
// every SaaS app re-implements otherwise. It composes:
//
//   - subscriptions mirror (current plan + status + period bounds)
//   - catalog spec (limits + features as metadata, RecurringGrant amounts)
//   - credits ledger (current remaining-this-cycle for credit buckets)
//
// And exposes a single typed Snapshot apps gate decisions on.
//
// # Convention: declare plan limits in catalog Product.Metadata
//
// The library reads two metadata key prefixes:
//
//	"limit.<name>"   — numeric cap. Value is either a positive
//	                    integer ("10") or the literal string "unlimited"
//	                    (which Snapshot.IntLimit returns as
//	                    (math.MaxInt64, hasLimit=false) so apps don't
//	                    need a special branch).
//
//	"feature.<name>" — boolean feature flag. Value is "true"/"false"
//	                    (or any non-empty for true; absence = false).
//
// Apps declare what they want their plans to encode:
//
//	"limit.max_seats":          "10"
//	"limit.max_api_calls_mo":   "50000"
//	"limit.included_storage_gb":"50"
//	"feature.sso":              "true"
//	"feature.audit_log":        "true"
//
// And read uniformly:
//
//	snap, _ := conn.Plans.SnapshotFor(ctx, subject)
//	if !snap.IsActive() { return ErrInactive }
//	if !snap.HasFeature("sso") { return ErrUpgrade }
//	maxSeats, has := snap.IntLimit("max_seats")
//	if has && currentSeats >= maxSeats { return ErrLimitReached }
//
// # Aggregation across multiple subscriptions
//
// B2B customers often have multiple active subs (base plan + addons).
// Snapshot aggregates:
//
//   - IntLimit       → MAX across all active subscriptions' plans
//     (highest tier wins).
//   - HasFeature     → OR (any plan enabling it grants access).
//   - CreditsRemaining → SUM of all non-expired grants in the bucket.
//
// # Dunning / past-due handling
//
// `Status==past_due` means the latest invoice failed but Stripe is
// retrying ("Smart Retries", typically over a 3-week window). The
// customer keeps access during this period; Snapshot encodes it as
// `InGracePeriod==true` so apps can show a banner without blocking
// access:
//
//	if snap.InGracePeriod() {
//	    banner = "Your payment failed — we'll retry; please update your card"
//	}
//	if at := snap.NextPaymentRetryAt(); at != nil {
//	    banner += " Next retry: " + at.Format("…")
//	}
package plans

import (
	"context"
	"math"
	"strconv"
	"time"

	"github.com/bds421/rho-kit/core/v2/clock"
	"github.com/bds421/rho-stripe/catalog"
	"github.com/bds421/rho-stripe/credits"
	"github.com/bds421/rho-stripe/subject"
	"github.com/bds421/rho-stripe/subscriptions"
)

// SubjectID re-exports [subject.ID] for the plans API. Same underlying
// type — assignment-compatible with checkout.SubjectID,
// SubjectID, credits.SubjectID, etc.
type SubjectID = subject.ID

// Operations is the public surface. Construct via New; the connector
// facade exposes it as conn.Plans.
type Operations struct {
	subRepo    subscriptions.SubscriptionRepo
	creditRepo credits.CreditRepo // optional (nil → credits-remaining lookups return 0)
	spec       *catalog.Spec
	now        clock.Func // defaults to clock.System() (time.Now)
}

// Config wires the package's dependencies (lib-wide convention).
type Config struct {
	// Spec is the catalog whose Product metadata declares limits /
	// features. Required.
	Spec *catalog.Spec
	// SubRepo is the local subscription mirror used to determine
	// which plans the subject currently has. Required.
	SubRepo subscriptions.SubscriptionRepo
	// CreditRepo is the local credit ledger queried for
	// CreditsRemaining. Optional — when nil, CreditsRemaining always
	// returns 0; numeric limits + feature flags still work.
	CreditRepo credits.CreditRepo
}

// New constructs Operations. cfg.CreditRepo may be nil for apps that
// don't use the credits ledger; numeric limits + feature flags still
// work, only CreditsRemaining is degraded.
func New(cfg Config) *Operations {
	if cfg.Spec == nil {
		panic("plans.New: Spec is required")
	}
	if cfg.SubRepo == nil {
		panic("plans.New: SubRepo is required")
	}
	return &Operations{
		subRepo:    cfg.SubRepo,
		creditRepo: cfg.CreditRepo,
		spec:       cfg.Spec,
		now:        clock.System(),
	}
}

// SetClock replaces the now-function the snapshot computation uses.
// Default is clock.System() (wall-clock time.Now). Tests pass a
// clock.NewStub(...).Func() to drive trial-end / past_due transitions
// deterministically.
func (o *Operations) SetClock(fn clock.Func) { o.now = clock.OrSystem(fn) }

// SnapshotFor returns a typed view of "what can `subject` do right now",
// aggregating across every active or trialing subscription they have.
//
// Returns a Snapshot with IsActive()==false when the subject has no
// access-granting subscription. Apps usually short-circuit on that.
func (o *Operations) SnapshotFor(ctx context.Context, subject SubjectID) (Snapshot, error) {
	subs, err := o.subRepo.ListBySubject(ctx, subject)
	if err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{
		SubjectID: subject,
		credits:   o.creditRepo,
		ctx:       ctx,
		features:  map[string]bool{},
		limits:    map[string]int64{},
		adds:      map[string]int64{},
		hasLimit:  map[string]bool{},
	}
	now := o.now()
	for _, sub := range subs {
		if !sub.Status.IsAccessGranting() {
			continue
		}
		snap.anyActive = true
		if sub.Status == subscriptions.StatusPastDue {
			snap.inGracePeriod = true
		}
		if sub.Status == subscriptions.StatusTrialing {
			snap.isTrialing = true
		}
		if !sub.CurrentPeriodEnd.IsZero() && (snap.PeriodEnd.IsZero() || sub.CurrentPeriodEnd.After(snap.PeriodEnd)) {
			snap.PeriodEnd = sub.CurrentPeriodEnd
		}
		if sub.TrialEnd != nil && sub.TrialEnd.After(now) && (snap.TrialEndsAt == nil || sub.TrialEnd.After(*snap.TrialEndsAt)) {
			t := *sub.TrialEnd
			snap.TrialEndsAt = &t
		}
		for _, item := range sub.Items {
			pk, _ := stripNamespace(o.spec.Namespace, item.PriceKey)
			productKey, _ := splitPriceKey(pk)
			if productKey == "" {
				continue
			}
			product, ok := o.spec.Products[productKey]
			if !ok {
				continue
			}
			snap.planKeys = append(snap.planKeys, productKey)
			mergeMetadata(&snap, product.Metadata)
		}
	}
	return snap, nil
}

// Snapshot is the typed read-side view of a subject's current
// entitlements, computed at SnapshotFor() time. Treat as a snapshot —
// it doesn't live-refresh.
type Snapshot struct {
	SubjectID     SubjectID
	PeriodEnd     time.Time
	TrialEndsAt   *time.Time
	anyActive     bool
	inGracePeriod bool
	isTrialing    bool
	planKeys      []string
	features      map[string]bool

	// limits holds the MAX across all `limit.<name>` declarations
	// (tier-upgrade semantics — higher tier wins).
	limits map[string]int64
	// adds holds the SUM across all `limit.<name>.add` declarations
	// (additive-addon semantics — seat packs, storage packs, etc.).
	// IntLimit() returns limits[name] + adds[name] so the two aggregation
	// modes compose: a base tier sets the floor, addons stack on top.
	adds     map[string]int64
	hasLimit map[string]bool

	// For deferred CreditsRemaining lookups.
	credits credits.CreditRepo
	ctx     context.Context
}

// IsActive reports whether the subject has any access-granting
// subscription (active / trialing / past_due).
func (s Snapshot) IsActive() bool { return s.anyActive }

// IsTrialing reports whether at least one of the subject's
// subscriptions is in its trial period.
func (s Snapshot) IsTrialing() bool { return s.isTrialing }

// InGracePeriod reports whether ANY subscription is past_due. Apps
// usually keep access on but show a "payment failed, will retry"
// banner to drive payment-method updates.
func (s Snapshot) InGracePeriod() bool { return s.inGracePeriod }

// HasFeature returns true if any active plan enables the named
// feature via `feature.<key>="true"` metadata.
func (s Snapshot) HasFeature(key string) bool { return s.features[key] }

// IntLimit returns the numeric cap for the named limit, plus a
// flag indicating whether the caller has any cap at all on it.
//
//	value, has := snap.IntLimit("max_seats")
//	switch {
//	case !has:                       // limit not declared in plan
//	case value == math.MaxInt64:     // declared as "unlimited"
//	default:                          // enforce
//	}
//
// When the limit is declared as "unlimited", returns
// (math.MaxInt64, true) so apps can write `currentSeats >= limit`
// uniformly without a special branch (it'll be false for any
// realistic currentSeats).
//
// # Aggregation across multiple plans
//
//   - `limit.<name>` declarations across plans are MAX-aggregated
//     (higher tier wins — Pro and Enterprise on the same subject
//     yields the Enterprise cap).
//   - `limit.<name>.add` declarations are SUM-aggregated (additive
//     addons — a base plan with `limit.storage_gb=100` plus two
//     `limit.storage_gb.add=50` storage-pack addons yields 200).
//   - The returned value is MAX(base) + SUM(adds), saturated when
//     base is "unlimited" (math.MaxInt64).
func (s Snapshot) IntLimit(key string) (value int64, has bool) {
	if !s.hasLimit[key] {
		return 0, false
	}
	base := s.limits[key]
	add := s.adds[key]
	if base == math.MaxInt64 {
		// Unlimited base saturates any finite add.
		return math.MaxInt64, true
	}
	// Guard against overflow when add is huge or close to MaxInt64.
	if add > math.MaxInt64-base {
		return math.MaxInt64, true
	}
	return base + add, true
}

// CreditsRemaining returns the subject's current balance in the
// named credit bucket. Returns 0 when no credit repo is wired or
// the bucket has no grants.
func (s Snapshot) CreditsRemaining(bucket string) int64 {
	if s.credits == nil {
		return 0
	}
	bal, err := s.credits.Balance(s.ctx, s.SubjectID, bucket)
	if err != nil {
		return 0
	}
	return bal.Total
}

// PlanKeys returns every active product key contributing to this
// snapshot — useful for analytics / debugging ("user is on pro + addon_xyz").
func (s Snapshot) PlanKeys() []string {
	out := make([]string, 0, len(s.planKeys))
	seen := map[string]bool{}
	for _, k := range s.planKeys {
		if !seen[k] {
			out = append(out, k)
			seen[k] = true
		}
	}
	return out
}

// --- helpers ---

func mergeMetadata(snap *Snapshot, meta map[string]string) {
	const (
		featurePrefix = "feature."
		limitPrefix   = "limit."
		addSuffix     = ".add"
	)
	for k, v := range meta {
		switch {
		case len(k) > len(featurePrefix) && k[:len(featurePrefix)] == featurePrefix:
			name := k[len(featurePrefix):]
			if v != "" && v != "false" && v != "0" {
				snap.features[name] = true
			}
		case len(k) > len(limitPrefix) && k[:len(limitPrefix)] == limitPrefix:
			name := k[len(limitPrefix):]
			// Distinguish `limit.X.add` (additive addon, SUM-aggregated)
			// from `limit.X` (base limit, MAX-aggregated). Strip the
			// `.add` suffix once detected so both contribute to the same
			// logical limit name in the snapshot.
			additive := false
			if len(name) > len(addSuffix) && name[len(name)-len(addSuffix):] == addSuffix {
				additive = true
				name = name[:len(name)-len(addSuffix)]
			}
			if v == "unlimited" {
				// "unlimited" is only meaningful as a base; treating it
				// as an additive contribution would silently make every
				// addon-namespaced cap MaxInt64. Reject silently.
				if additive {
					continue
				}
				if cur, ok := snap.limits[name]; !ok || math.MaxInt64 > cur {
					snap.limits[name] = math.MaxInt64
				}
				snap.hasLimit[name] = true
				continue
			}
			parsed, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				continue
			}
			if additive {
				snap.adds[name] += parsed
			} else if cur, ok := snap.limits[name]; !ok || parsed > cur {
				snap.limits[name] = parsed
			}
			snap.hasLimit[name] = true
		}
	}
}

func stripNamespace(ns, key string) (string, bool) {
	prefix := ns + "."
	if len(key) > len(prefix) && key[:len(prefix)] == prefix {
		return key[len(prefix):], true
	}
	return key, false
}

func splitPriceKey(key string) (product, price string) {
	for i := 0; i < len(key); i++ {
		if key[i] == '.' {
			return key[:i], key[i+1:]
		}
	}
	return "", ""
}
