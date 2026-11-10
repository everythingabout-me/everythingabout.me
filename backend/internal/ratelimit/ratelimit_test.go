package ratelimit

import (
	"testing"
	"time"
)

func newTestLimiter() (*Limiter, *time.Time) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	l := New()
	l.now = func() time.Time { return now }
	return l, &now
}

// A window let the whole allowance go out in one burst and then refused everything until
// the minute turned over; a bucket keeps admitting at the configured rate.
func TestAllowSteadiesAtTheConfiguredRate(t *testing.T) {
	l, now := newTestLimiter()
	for i := 0; i < 60; i++ {
		if ok, _ := l.Allow("k", 60, time.Minute); !ok {
			t.Fatalf("request %d inside the burst was refused", i+1)
		}
	}
	if ok, retry := l.Allow("k", 60, time.Minute); ok || retry < 1 {
		t.Fatalf("burst overflow admitted with retry %d", retry)
	}
	// One second of refill is exactly one request at 60/min.
	*now = now.Add(time.Second)
	if ok, _ := l.Allow("k", 60, time.Minute); !ok {
		t.Fatal("a refilled token was still refused")
	}
	if ok, _ := l.Allow("k", 60, time.Minute); ok {
		t.Fatal("a second request was admitted from one refilled token")
	}
	// Half a token later the wait is under a second and still rounds up to one.
	*now = now.Add(500 * time.Millisecond)
	if ok, retry := l.Allow("k", 60, time.Minute); ok || retry != 1 {
		t.Fatalf("half-token wait: ok=%v retry=%d", ok, retry)
	}
}

func TestKeysDoNotShareABucket(t *testing.T) {
	l, _ := newTestLimiter()
	for i := 0; i < 5; i++ {
		l.Allow("a", 5, time.Hour)
	}
	if ok, _ := l.Allow("a", 5, time.Hour); ok {
		t.Fatal("bucket a over its limit")
	}
	if ok, _ := l.Allow("b", 5, time.Hour); !ok {
		t.Fatal("bucket b inherited a's spending")
	}
}

// Bounding memory must not wipe every tracked key: the previous fallback cleared the whole
// map, which handed every client a fresh allowance at once.
func TestFullTableDoesNotWipeEveryBucket(t *testing.T) {
	l, _ := newTestLimiter()
	for i := 0; i < l.maxKeys; i++ {
		l.Allow(string(rune('a'+i%26))+string(rune('a'+i/26)), 1, time.Hour)
	}
	// Spend one key dry, then force the table full with fresh keys.
	spent := "aa"
	for {
		if ok, _ := l.Allow(spent, 1, time.Hour); !ok {
			break
		}
	}
	for i := 0; i < 100; i++ {
		l.Allow("fresh-"+string(rune('a'+i)), 1, time.Hour)
	}
	if len(l.buckets) > l.maxKeys {
		t.Fatalf("table grew past its bound: %d", len(l.buckets))
	}
	// The spent key may have been the one evicted, but it must not have been reset to a
	// full allowance while still tracked with its spending.
	if entry, ok := l.buckets[spent]; ok && entry.tokens >= 1 {
		t.Fatal("a tracked spent bucket was refilled by the eviction")
	}
}
