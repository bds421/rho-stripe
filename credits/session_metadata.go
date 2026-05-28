package credits

import (
	"encoding/json"
	"errors"
	"fmt"
)

// SessionMetadataKey is the Stripe Checkout Session metadata key under
// which the lib stores the credit-grant intent at session creation
// time. The webhook auto-handler reads it on checkout.session.completed
// and creates ledger entries without needing a separate Stripe lookup.
const SessionMetadataKey = "credit_grants"

// PendingGrant is one credit grant intent encoded into a session's
// metadata. Multiple grants in one session (e.g. customer bought two
// different credit packs) are stored as a JSON array.
type PendingGrant struct {
	Bucket     string `json:"bucket"`
	Amount     int64  `json:"amount"`
	ValidDays  int    `json:"valid_days,omitempty"`
	ProductKey string `json:"product_key,omitempty"`
	Quantity   int    `json:"quantity,omitempty"`
}

// EncodeSessionMetadata serializes grants for the Stripe Session
// metadata value. Returns ("", nil) when grants is empty so callers
// can detect "no metadata to set."
func EncodeSessionMetadata(grants []PendingGrant) (string, error) {
	if len(grants) == 0 {
		return "", nil
	}
	b, err := json.Marshal(grants)
	if err != nil {
		return "", fmt.Errorf("credits: encode session metadata: %w", err)
	}
	// Stripe limits metadata values to 500 bytes.
	if len(b) > 500 {
		return "", errors.New("credits: encoded credit_grants metadata exceeds Stripe's 500-byte per-value limit")
	}
	return string(b), nil
}

// DecodeSessionMetadata parses the metadata value back into grants.
// Returns nil, nil when value is empty (no credit grants on this session).
func DecodeSessionMetadata(value string) ([]PendingGrant, error) {
	if value == "" {
		return nil, nil
	}
	var grants []PendingGrant
	if err := json.Unmarshal([]byte(value), &grants); err != nil {
		return nil, fmt.Errorf("credits: decode session metadata: %w", err)
	}
	return grants, nil
}
