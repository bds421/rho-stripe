// Package meta holds wire-format constants + small helpers that the
// lib's writers and readers agree on. It's a leaf package (no imports
// of any other lib package) so every layer can depend on it without
// risking import cycles.
//
// Today the package holds:
//
//   - MetadataKeyNamespace + StampNamespace: the "app_namespace"
//     Stripe-metadata key that the webhooks dispatcher filters on
//     (per ADR-0007) and that every lib writer stamps on objects it
//     creates.
//
// New constants belong here when they meet all of:
//   - shared between writer + reader inside the lib
//   - small (just constants + tiny helpers)
//   - no Stripe-go or upper-layer dependency
package meta

// MetadataKeyNamespace is the Stripe metadata key under which the
// lib's app namespace is stamped. Webhook dispatch filters by this key;
// changing it requires a coordinated rollout (every writer + the
// dispatcher must agree).
const MetadataKeyNamespace = "app_namespace"

// StampNamespace returns a copy of `in` with MetadataKeyNamespace set
// to ns. When ns is empty, `in` is returned unchanged (single-app
// configurations don't need the stamp). Always returns a fresh map
// when ns is non-empty to preserve the caller-supplied map's identity.
func StampNamespace(in map[string]string, ns string) map[string]string {
	if ns == "" {
		return in
	}
	out := make(map[string]string, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	out[MetadataKeyNamespace] = ns
	return out
}
