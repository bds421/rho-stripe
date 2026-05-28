package webhooks

import (
	"encoding/json"

	"github.com/bds421/rho-stripe/meta"
	stripe "github.com/stripe/stripe-go/v82"
)

// extractAppNamespace reads metadata.app_namespace from the event's
// inner object. Returns ("", false) when the event lacks a stamp
// (e.g. account.updated events, or events from before the lib started
// stamping a given object type).
//
// The lookup decodes event.Data.Raw into a map only as deep as needed
// to read .metadata.app_namespace, which is constant-cost.
func extractAppNamespace(evt *stripe.Event) (string, bool) {
	if evt == nil || evt.Data == nil || len(evt.Data.Raw) == 0 {
		return "", false
	}
	var shape struct {
		Metadata map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal(evt.Data.Raw, &shape); err != nil {
		return "", false
	}
	ns, ok := shape.Metadata[meta.MetadataKeyNamespace]
	if !ok || ns == "" {
		return "", false
	}
	return ns, true
}

// filterDecision reports what the dispatcher should do with an event
// based on namespace-matching against this app's configured namespace.
type filterDecision int

const (
	filterDispatch filterDecision = iota // route to the typed handler
	filterSkip                           // namespace mismatch; mark processed silently
	filterUnrouted                       // no namespace stamp; route to OnOtherEvent only
)

// classifyNamespace decides what to do with an incoming event given
// this Webhooks instance's configured namespace.
//
//   - Namespace == "" (single-app setup): always dispatch.
//   - Event has matching namespace: dispatch.
//   - Event has a different namespace: skip silently.
//   - Event has no namespace stamp: unrouted — apps that opt into
//     OnOtherEvent see it; typed handlers do not.
func (w *Webhooks) classifyNamespace(evt *stripe.Event) filterDecision {
	if w.namespace == "" {
		return filterDispatch
	}
	ns, ok := extractAppNamespace(evt)
	if !ok {
		return filterUnrouted
	}
	if ns != w.namespace {
		return filterSkip
	}
	return filterDispatch
}
