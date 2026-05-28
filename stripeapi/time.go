package stripeapi

import "time"

// UnixToTime converts a Stripe Unix timestamp (seconds since epoch)
// into a UTC time.Time. Returns the zero time.Time when u == 0 —
// Stripe uses 0 to signal "unset" for many optional timestamp fields
// (TrialStart, CanceledAt, EndedAt, etc.), and the lib's mirror tracks
// "unset" via time.Time.IsZero() so the projection has to agree.
func UnixToTime(u int64) time.Time {
	if u == 0 {
		return time.Time{}
	}
	return time.Unix(u, 0).UTC()
}

// UnixToTimePtr is the *time.Time analogue of [UnixToTime] for
// projection helpers whose target field is *time.Time (typically
// "this happened at most once" fields like CanceledAt, EndedAt).
// Returns nil when u == 0.
func UnixToTimePtr(u int64) *time.Time {
	if u == 0 {
		return nil
	}
	t := time.Unix(u, 0).UTC()
	return &t
}

// unixOrZero / unixToPtr are the lowercase aliases for the dense
// stripeapi internal call sites in *_backend.go.
func unixOrZero(u int64) time.Time  { return UnixToTime(u) }
func unixToPtr(u int64) *time.Time  { return UnixToTimePtr(u) }
