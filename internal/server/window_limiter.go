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
	// live holds requests of this window that are still being served, or whose
	// response is a long-lived stream (Server-Sent Events). An entry with a
	// stream attached is never expired while the stream is open, however long
	// that is.
	live int
}

// takeLive is take for endpoints a connected client holds open (the live
// activity stream). A streamed response stays "in use" for its whole life, so
// charging it against a per-minute request budget would throttle a dashboard
// that is merely sitting there with a tab open. It is instead bounded as a
// concurrency limit: at most max streams per key at once.
//
// The first stream of a key reuses that key's existing window entry -- keeping
// the request counter it already holds from the request that opened the stream
// -- and pins it for the duration. Once the last stream closes, the entry
// resumes normal expiry.
func (l *windowLimiter) takeLive(key string) windowQuota {
	key = limiterKey(key)
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e, ok := l.entries[key]
	if !ok {
		if len(l.entries) >= maxLimiterEntries {
			l.purge(now)
			if len(l.entries) >= maxLimiterEntries {
				for k := range l.entries {
					delete(l.entries, k)
					break
				}
			}
		}
		e = &windowEntry{start: now, count: 1}
		l.entries[key] = e
	}
	q := windowQuota{Limit: l.max, Remaining: max(l.max-e.live, 0)}
	if e.count >= l.max && e.live == 0 {
		q.ResetSeconds = resetSeconds(now, e.start.Add(l.window))
		return q
	}
	if e.live >= l.max {
		q.ResetSeconds = 1
		return q
	}
	e.live++
	if e.live >= l.max {
		q.Remaining = 0
	}
	q.Allowed = true
	q.ResetSeconds = resetSeconds(now, e.start.Add(l.window))
	return q
}

// releaseLive closes a stream previously admitted by takeLive.
func (l *windowLimiter) releaseLive(key string) {
	key = limiterKey(key)
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok || e.live == 0 {
		return
	}
	e.live--
	if e.live == 0 && !l.now().Before(e.start.Add(l.window)) {
		delete(l.entries, key)
	}
}

// resetSeconds rounds the remaining window up to whole seconds, at least 1.
func resetSeconds(now, end time.Time) int {
	seconds := int((end.Sub(now) + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return seconds
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
	q := windowQuota{Limit: l.max, ResetSeconds: resetSeconds(now, e.start.Add(l.window))}
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
		if e.live > 0 {
			continue
		}
		if !now.Before(e.start.Add(l.window)) {
			delete(l.entries, k)
		}
	}
}
