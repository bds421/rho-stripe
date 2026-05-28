package webhooks

import (
	"sync"
	"testing"
)

func TestLogRateLimiter_AllowsFirstNPerSecond(t *testing.T) {
	var l logRateLimiter
	l.burstPerSec = 5
	for i := 0; i < 5; i++ {
		if got := l.recordAndShouldLog(); got != 0 {
			t.Fatalf("call %d should log with 0 dropped; got %d", i, got)
		}
	}
}

func TestLogRateLimiter_SuppressesAfterBurst(t *testing.T) {
	var l logRateLimiter
	l.burstPerSec = 3
	for i := 0; i < 3; i++ {
		_ = l.recordAndShouldLog()
	}
	// 4th call within the same second should suppress.
	if got := l.recordAndShouldLog(); got != -1 {
		t.Fatalf("4th call should suppress (return -1); got %d", got)
	}
	if got := l.recordAndShouldLog(); got != -1 {
		t.Fatalf("5th call should also suppress; got %d", got)
	}
}

func TestLogRateLimiter_DefaultBurstIs10(t *testing.T) {
	var l logRateLimiter // zero-value
	for i := 0; i < 10; i++ {
		if got := l.recordAndShouldLog(); got == -1 {
			t.Fatalf("call %d suppressed before reaching default burst; got -1", i)
		}
	}
	if got := l.recordAndShouldLog(); got != -1 {
		t.Fatalf("11th call should suppress; got %d", got)
	}
}

func TestLogRateLimiter_ConcurrentSafe(t *testing.T) {
	var l logRateLimiter
	l.burstPerSec = 100
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = l.recordAndShouldLog()
		}()
	}
	wg.Wait()
	// Total calls = 200; burst = 100 per second; some calls suppress,
	// some log. The exact split depends on timing — we just verify
	// the mutex didn't deadlock or panic.
}
