package credentialstore

import (
	"errors"
	"sync"
	"time"
)

// Default ingress rate limit per remote IP.
const (
	DefaultRatePerMinute = 10
	DefaultRateBurst     = 5
)

// maxBuckets bounds the limiter's memory. Full buckets are evicted first; if the table is still
// full every remaining address is active, and a new address is refused rather than growing the
// table without bound.
const maxBuckets = 100_000

// rateLimiter is a per-key token bucket driven by the injected clock, implemented as GCRA: each
// key keeps the theoretical arrival time of its next request, which keeps the arithmetic exact in
// time.Duration instead of accumulating floating-point refills.
type rateLimiter struct {
	interval  time.Duration // time to earn one request
	tolerance time.Duration // how far ahead of now a key may run: (burst-1) intervals
	now       func() time.Time
	mu        sync.Mutex
	arrivals  map[string]time.Time
}

func newRateLimiter(perMinute, burst int, now func() time.Time) (*rateLimiter, error) {
	if perMinute <= 0 || burst <= 0 {
		return nil, errors.New("credential store ingress rate and burst must be positive")
	}
	interval := time.Minute / time.Duration(perMinute)
	return &rateLimiter{
		interval: interval, tolerance: interval * time.Duration(burst-1), now: now,
		arrivals: make(map[string]time.Time),
	}, nil
}

// allow admits one request for key if its budget has room and charges one interval for it.
func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	tat, ok := l.arrivals[key]
	if !ok && len(l.arrivals) >= maxBuckets {
		l.evictIdle(now)
		if len(l.arrivals) >= maxBuckets {
			return false
		}
	}
	if tat.Before(now) {
		tat = now
	}
	if tat.Sub(now) > l.tolerance {
		return false
	}
	l.arrivals[key] = tat.Add(l.interval)
	return true
}

// evictIdle drops keys whose budget is full again; forgetting them changes no decision.
func (l *rateLimiter) evictIdle(now time.Time) {
	for key, tat := range l.arrivals {
		if !tat.After(now) {
			delete(l.arrivals, key)
		}
	}
}
