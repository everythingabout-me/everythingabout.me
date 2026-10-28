// Package ratelimit is in-process, in-memory rate limiting: enough to stop password guessing
// and bulk registration. With multiple instances each one keeps its own count; strict
// limiting would need Redis.
package ratelimit

import (
	"fmt"
	"sync"
	"time"

	"everythingabout.me/backend/internal/httpx"
)

type bucket struct {
	count   int
	resetAt time.Time
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
func (l *Limiter) Allow(key string, limit int, window time.Duration) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	entry, exists := l.buckets[key]
	if !exists || !entry.resetAt.After(now) {
		if len(l.buckets) >= l.maxKeys {
			for candidate, value := range l.buckets {
				if !value.resetAt.After(now) {
					delete(l.buckets, candidate)
				}
			}
			if len(l.buckets) >= l.maxKeys {
				l.buckets = make(map[string]bucket)
			}
		}
		l.buckets[key] = bucket{count: 1, resetAt: now.Add(window)}
		return true, 0
	}
	if entry.count >= limit {
		retry := int((entry.resetAt.Sub(now) + time.Second - 1) / time.Second)
		if retry < 1 {
			retry = 1
		}
		return false, retry
	}
	entry.count++
	l.buckets[key] = entry
	return true, 0
}

// Enforce returns a 429 once the limit is exceeded.
func (l *Limiter) Enforce(key string, limit int, window time.Duration) error {
	ok, retryAfter := l.Allow(key, limit, window)
	if !ok {
		return httpx.TooManyRequests(fmt.Sprintf("too many requests; retry in %d seconds", retryAfter))
	}
	return nil
}
