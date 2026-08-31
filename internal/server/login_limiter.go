package server

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// loginLimiter is a simple in-memory brute-force guard for the admin login
// endpoint. It tracks consecutive failed attempts per client IP and locks the
// IP out for a window once a threshold is exceeded. It is intentionally simple:
// no external dependency, no token bucket, no persistence.
type loginLimiter struct {
	mu       sync.Mutex
	failures map[string]*loginFailure
	max      int           // consecutive failures before lockout
	window   time.Duration // counting window for consecutive failures
	lockout  time.Duration // how long a lockout lasts
}

type loginFailure struct {
	count int
	first time.Time
	until time.Time // lockout expiry; zero means not locked out
}

func newLoginLimiter(max int, window, lockout time.Duration) *loginLimiter {
	return &loginLimiter{
		failures: make(map[string]*loginFailure),
		max:      max,
		window:   window,
		lockout:  lockout,
	}
}

// locked reports whether the client is currently locked out. A lockout that has
// expired is cleared so the next attempt starts fresh. Entries that are merely
// counting failures (never locked out) are left untouched.
func (l *loginLimiter) locked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, ok := l.failures[key]
	if !ok {
		return false
	}
	if f.until.IsZero() {
		return false // counting failures, not locked out
	}
	if time.Now().Before(f.until) {
		return true
	}
	delete(l.failures, key) // lockout expired
	return false
}

// recordFailure registers a failed attempt and reports whether the client is
// now locked out (i.e. this failure crossed the threshold).
func (l *loginLimiter) recordFailure(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	f, ok := l.failures[key]
	if !ok {
		l.failures[key] = &loginFailure{count: 1, first: now}
		return false
	}
	if now.Before(f.until) {
		return true
	}
	// Reset the streak if the counting window has elapsed since the first failure.
	if now.Sub(f.first) > l.window {
		f.count = 1
		f.first = now
		return false
	}
	f.count++
	if f.count >= l.max {
		f.until = now.Add(l.lockout)
		return true
	}
	return false
}

// success clears the failure record for a successful login.
func (l *loginLimiter) success(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, key)
}

// peerIP returns the client IP used to key the limiter. It uses the direct
// peer address (RemoteAddr) rather than a forwarded header, so a spoofable
// header cannot be used to bypass or poison the lockout.
func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
