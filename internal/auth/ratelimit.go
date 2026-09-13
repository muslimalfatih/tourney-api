package auth

import (
	"sync"
	"time"
)

// OTP request limits. Deliberately per-address AND per-IP: the first stops one
// mailbox being flooded, the second stops one client walking a list of
// addresses to discover which are invited.
const (
	OTPPerEmailLimit  = 3
	OTPPerEmailWindow = 10 * time.Minute
	OTPPerIPLimit     = 10
	OTPPerIPWindow    = time.Hour
)

// Limiter is a fixed-set sliding-window counter held in process.
//
// ponytail: in-process, single machine. This is coherent today only because
// the app is pinned to one Fly machine — the realtime hub keeps SSE
// subscribers in memory, so `--ha=false` is already a hard deployment
// constraint. A second machine would give each its own counters and multiply
// every limit by the machine count. Move this to Redis at the same time as the
// SSE hub, not before: two coordinated stores is the cost either way.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string][]time.Time

	// maxKeys bounds memory against an attacker cycling addresses. When
	// exceeded, expired buckets are swept; if that is not enough the map is
	// dropped entirely. Dropping resets everyone's counters, which briefly
	// loosens the limit — the alternative is unbounded growth, and a limiter
	// that OOMs the machine protects nothing.
	maxKeys int
}

func NewLimiter() *Limiter {
	return &Limiter{buckets: make(map[string][]time.Time), maxKeys: 10_000}
}

// Allow records an attempt and reports whether it is within the limit.
//
// The attempt is recorded even when denied, so hammering the endpoint keeps
// the window full rather than letting a caller slip through the moment the
// oldest entry ages out.
func (l *Limiter) Allow(key string, limit int, window time.Duration) bool {
	now := time.Now()
	cutoff := now.Add(-window)

	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.buckets) > l.maxKeys {
		l.sweepLocked(now)
		if len(l.buckets) > l.maxKeys {
			l.buckets = make(map[string][]time.Time)
		}
	}

	kept := l.buckets[key][:0]
	for _, t := range l.buckets[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	allowed := len(kept) < limit
	l.buckets[key] = append(kept, now)
	return allowed
}

// RetryAfter reports how long until the oldest entry in a full window ages
// out, so callers can tell the user when to try again instead of "later".
func (l *Limiter) RetryAfter(key string, window time.Duration) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	stamps := l.buckets[key]
	if len(stamps) == 0 {
		return 0
	}
	if d := time.Until(stamps[0].Add(window)); d > 0 {
		return d
	}
	return 0
}

// Reset clears one key. Used by tests and after a successful sign-in, where
// continuing to count a completed flow only punishes the legitimate user.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.buckets, key)
}

// sweepLocked drops buckets whose newest entry is older than the longest
// window in use. Caller must hold the mutex.
func (l *Limiter) sweepLocked(now time.Time) {
	cutoff := now.Add(-OTPPerIPWindow)
	for k, stamps := range l.buckets {
		if len(stamps) == 0 || stamps[len(stamps)-1].Before(cutoff) {
			delete(l.buckets, k)
		}
	}
}
