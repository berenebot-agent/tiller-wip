package server

import (
	"sync"
	"time"
)

// windowLimiter is a fixed-window request counter keyed by string. Unlike
// loginLimiter it never extends a block: once the budget is spent, requests are
// refused until the window that began at the key's first request ends, and the
// caller learns exactly how many seconds remain. It backs the hosted
// per-account management API budget, whose responses advertise the quota.
type windowLimiter struct {
	mu      sync.Mutex
	entries map[string]*windowEntry
	max     int
	window  time.Duration
	now     func() time.Time
}

type windowEntry struct {
	count int
	start time.Time
}

// windowQuota is the state after charging one request.
type windowQuota struct {
	Limit     int
	Remaining int
	// ResetSeconds is the whole seconds (rounded up, at least 1) until the
	// window ends and the budget refills.
	ResetSeconds int
	Allowed      bool
}

func newWindowLimiter(max int, window time.Duration) *windowLimiter {
	return &windowLimiter{entries: make(map[string]*windowEntry), max: max, window: window, now: time.Now}
}

// take charges one request against key and reports the resulting quota. A
// refused request is not counted.
func (l *windowLimiter) take(key string) windowQuota {
	key = limiterKey(key)
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e, ok := l.entries[key]
	if ok && !now.Before(e.start.Add(l.window)) {
		delete(l.entries, key)
		ok = false
	}
	if !ok {
		if len(l.entries) >= maxLimiterEntries {
			l.purge(now)
			if len(l.entries) >= maxLimiterEntries {
				// Every entry is a live window: drop one so the table stays
				// bounded. The evicted account merely gets a fresh window.
				for k := range l.entries {
					delete(l.entries, k)
					break
				}
			}
		}
		e = &windowEntry{start: now}
		l.entries[key] = e
	}
	reset := int((e.start.Add(l.window).Sub(now) + time.Second - 1) / time.Second)
	if reset < 1 {
		reset = 1
	}
	q := windowQuota{Limit: l.max, ResetSeconds: reset}
	if e.count >= l.max {
		return q
	}
	e.count++
	q.Allowed = true
	q.Remaining = l.max - e.count
	return q
}

func (l *windowLimiter) purge(now time.Time) {
	for k, e := range l.entries {
		if !now.Before(e.start.Add(l.window)) {
			delete(l.entries, k)
		}
	}
}
