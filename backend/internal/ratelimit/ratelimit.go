// Package ratelimit is in-process, in-memory rate limiting: enough to stop password guessing
// and to notice a client that loops. With multiple instances each one keeps its own count;
// strict limiting would need Redis.
package ratelimit

import (
	"fmt"
	"math"
	"sync"
	"time"

	"everythingabout.me/backend/internal/httpx"
)

// bucket is a token bucket, not a fixed window: a window lets a client spend the whole
// allowance in one burst and then wait out the rest of the minute, which is how a 23k-file
// import ended up stalling for ~45 seconds between bursts. A bucket refills continuously, so
// a client that keeps asking settles at the configured rate instead of sawtoothing, and the
// burst is still bounded by the same number.
type bucket struct {
	tokens  float64
	updated time.Time
}

type Limiter struct {
	mu      sync.Mutex
	buckets map[string]bucket
	maxKeys int
	now     func() time.Time
}

func New() *Limiter {
	return &Limiter{
		buckets: make(map[string]bucket),
		maxKeys: 10_000,
		now:     time.Now,
	}
}

// Allow reports whether the request passes, along with the suggested retry delay in seconds.
//
// limit is both the burst and the per-window allowance, so a caller keeps its old meaning:
// limit requests may pass back to back, and after that the bucket admits one every
// window/limit. That is the number clients are told about, and it is now a steady rate
// rather than a cliff.
func (l *Limiter) Allow(key string, limit int, window time.Duration) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if limit < 1 || window <= 0 {
		return false, 1
	}
	now := l.now()
	rate := float64(limit) / window.Seconds()
	entry, exists := l.buckets[key]
	if !exists {
		// Pruning here (not on every request) keeps the common path to one map lookup.
		if len(l.buckets) >= l.maxKeys {
			l.makeRoom(now)
		}
		entry = bucket{tokens: float64(limit), updated: now}
	} else {
		entry.tokens = math.Min(float64(limit), entry.tokens+now.Sub(entry.updated).Seconds()*rate)
		entry.updated = now
	}
	if entry.tokens < 1 {
		wait := time.Duration((1 - entry.tokens) / rate * float64(time.Second))
		retry := int(math.Ceil(wait.Seconds()))
		if retry < 1 {
			retry = 1
		}
		l.buckets[key] = entry
		return false, retry
	}
	entry.tokens--
	l.buckets[key] = entry
	return true, 0
}

// makeRoom bounds memory without wiping the table. A full bucket carries no state worth
// keeping — it is indistinguishable from a fresh key — so those go first; if every key is
// still partly spent, the one that has been idle longest goes. Dropping a spent bucket
// hands that one key a fresh allowance; it never hands every key one, which is what the
// previous "clear the whole map" did and why the limit could vanish for a moment.
func (l *Limiter) makeRoom(now time.Time) {
	for key, entry := range l.buckets {
		windowless := entry.updated.IsZero()
		if windowless || entry.tokens >= 1 {
			delete(l.buckets, key)
		}
	}
	if len(l.buckets) < l.maxKeys {
		return
	}
	oldest, oldestAt := "", now
	for key, entry := range l.buckets {
		if oldest == "" || entry.updated.Before(oldestAt) {
			oldest, oldestAt = key, entry.updated
		}
	}
	if oldest != "" {
		delete(l.buckets, oldest)
	}
}

// Enforce returns a 429 once the limit is exceeded.
func (l *Limiter) Enforce(key string, limit int, window time.Duration) error {
	ok, retryAfter := l.Allow(key, limit, window)
	if !ok {
		return httpx.TooManyRequests(fmt.Sprintf("too many requests; retry in %d seconds", retryAfter))
	}
	return nil
}
