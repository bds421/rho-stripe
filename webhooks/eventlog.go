package webhooks

import (
	"context"
	"sync"
	"time"
)

// EventLog persists per-event metadata + payload alongside the
// idempotency.Store dedup record. Optional — apps that don't need
// replay/debug capability leave Config.EventLog nil and lose nothing
// functional. When set, the dispatcher records every event it sees
// (status, attempt count, last error, full payload).
//
// MemoryLog is for tests; apps in production use a Postgres-backed
// implementation (see repos/postgres if available).
type EventLog interface {
	Record(ctx context.Context, e LoggedEvent) error
	Get(ctx context.Context, eventID string) (LoggedEvent, bool, error)
	StuckEvents(ctx context.Context, minAttempts int) ([]LoggedEvent, error)
	PruneOlderThan(ctx context.Context, before time.Time) (int, error)
}

// LoggedEvent is one row in the EventLog.
type LoggedEvent struct {
	EventID      string
	EventType    string
	Status       string // "skipped" | "processed" | "failed"
	LastError    string
	AttemptCount int
	ReceivedAt   time.Time
	ProcessedAt  *time.Time
	Payload      []byte // raw event JSON for replay
	Namespace    string // app_namespace stamp resolved at receive time
}

// NewMemoryLog returns an in-memory EventLog suitable for tests.
func NewMemoryLog() EventLog {
	return &memoryLog{events: map[string]LoggedEvent{}}
}

type memoryLog struct {
	mu     sync.RWMutex
	events map[string]LoggedEvent
}

func (m *memoryLog) Record(_ context.Context, e LoggedEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.events[e.EventID]; ok {
		// Preserve receive time + attempt count if we're updating.
		if e.ReceivedAt.IsZero() {
			e.ReceivedAt = existing.ReceivedAt
		}
		if e.AttemptCount == 0 {
			e.AttemptCount = existing.AttemptCount
		}
	}
	if e.ReceivedAt.IsZero() {
		e.ReceivedAt = time.Now()
	}
	m.events[e.EventID] = e
	return nil
}

func (m *memoryLog) Get(_ context.Context, eventID string) (LoggedEvent, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.events[eventID]
	return e, ok, nil
}

func (m *memoryLog) StuckEvents(_ context.Context, minAttempts int) ([]LoggedEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []LoggedEvent
	for _, e := range m.events {
		if e.Status != "processed" && e.AttemptCount >= minAttempts {
			out = append(out, e)
		}
	}
	return out, nil
}

func (m *memoryLog) PruneOlderThan(_ context.Context, before time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, e := range m.events {
		if e.ReceivedAt.Before(before) {
			delete(m.events, id)
			n++
		}
	}
	return n, nil
}
