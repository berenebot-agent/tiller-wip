package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
)

func TestLoginLimiterLockoutAndReset(t *testing.T) {
	l := newLoginLimiter(3, time.Minute, time.Minute)

	// Two failures: not locked out.
	if l.recordFailure("1.2.3.4") {
		t.Fatal("first failure should not lock out")
	}
	if l.recordFailure("1.2.3.4") {
		t.Fatal("second failure should not lock out")
	}
	// Third failure crosses the threshold.
	if !l.recordFailure("1.2.3.4") {
		t.Fatal("third failure should lock out")
	}
	if !l.locked("1.2.3.4") {
		t.Fatal("client should be locked out")
	}
	// A different IP is unaffected.
	if l.locked("5.6.7.8") {
		t.Fatal("unrelated IP should not be locked out")
	}

	// Success clears the record.
	l.success("1.2.3.4")
	if l.locked("1.2.3.4") {
		t.Fatal("success should clear the lockout")
	}
}

func TestLoginLimiterWindowReset(t *testing.T) {
	l := newLoginLimiter(3, 10*time.Millisecond, time.Minute)
	l.recordFailure("1.2.3.4")
	l.recordFailure("1.2.3.4")
	// Let the counting window elapse; the next failure restarts the streak.
	time.Sleep(20 * time.Millisecond)
	if l.recordFailure("1.2.3.4") {
		t.Fatal("failure after window elapse should restart the streak, not lock out")
	}
}

func TestLoginLimiterLockoutExpiry(t *testing.T) {
	l := newLoginLimiter(2, time.Minute, 10*time.Millisecond)
	l.recordFailure("1.2.3.4")
	l.recordFailure("1.2.3.4") // locks out
	if !l.locked("1.2.3.4") {
		t.Fatal("should be locked out")
	}
	time.Sleep(20 * time.Millisecond)
	if l.locked("1.2.3.4") {
		t.Fatal("lockout should expire after the window")
	}
}

func TestLoginLimiterSuccessfulRequestsDoNotCountAsFailures(t *testing.T) {
	l := newLoginLimiter(10, time.Minute, time.Minute)
	for i := 0; i < 100; i++ {
		if l.locked("1.2.3.4") {
			t.Fatal("successful request was locked out")
		}
		l.success("1.2.3.4")
	}
}

func TestLoginLimiterSuccessClearsFailures(t *testing.T) {
	l := newLoginLimiter(3, time.Minute, time.Minute)
	l.recordFailure("1.2.3.4")
	l.recordFailure("1.2.3.4")
	l.success("1.2.3.4")
	if l.recordFailure("1.2.3.4") {
		t.Fatal("success did not clear failures")
	}
}

// TestLoginLimiterHardBoundRefusesEntryWhenFull verifies the 4096-entry map
// is a true hard bound: with every slot holding an active lockout (nothing
// safely evictable), a new IP is let through without growing the map, and
// every existing lockout still holds.
func TestLoginLimiterHardBoundRefusesEntryWhenFull(t *testing.T) {
	l := newLoginLimiter(1, time.Hour, time.Hour)
	for i := 0; i < maxLimiterEntries; i++ {
		ip := fmt.Sprintf("10.%d.%d.%d", (i>>16)&0xff, (i>>8)&0xff, i&0xff)
		l.recordFailure(ip) // first failure starts the streak
		if !l.recordFailure(ip) {
			t.Fatalf("filling lockout %d: expected lockout on second failure with max=1", i)
		}
	}
	l.mu.Lock()
	if got := len(l.failures); got != maxLimiterEntries {
		l.mu.Unlock()
		t.Fatalf("limiter holds %d entries, want %d", got, maxLimiterEntries)
	}
	l.mu.Unlock()
	// One more IP: must not grow the map and must not be locked out.
	if l.recordFailure("192.0.2.99") {
		t.Fatal("new IP should be let through when the limiter is full of active lockouts")
	}
	if l.locked("192.0.2.99") {
		t.Fatal("refused entry must not leave the new IP locked out")
	}
	l.mu.Lock()
	if got := len(l.failures); got != maxLimiterEntries {
		l.mu.Unlock()
		t.Fatalf("limiter grew to %d entries, want hard bound %d", got, maxLimiterEntries)
	}
	l.mu.Unlock()
	// Spot-check that pre-existing lockouts still hold.
	if !l.locked("10.0.0.0") || !l.locked("10.0.15.255") {
		t.Fatal("existing lockouts must still hold after a refused insert")
	}
}

func TestOAuthRateLimitCheckDoesNotRecordSuccess(t *testing.T) {
	l := newLoginLimiter(2, time.Minute, time.Minute)
	app := &Server{config: config.Config{}, oauthCallbackLimiter: l}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "1.2.3.4:1234"
	w := httptest.NewRecorder()
	if app.oauthRateLimited(w, req, l) {
		t.Fatal("first request should not be limited")
	}
	if app.oauthRateLimited(w, req, l) {
		t.Fatal("second request should not be limited")
	}
	if l.locked("1.2.3.4") {
		t.Fatal("rate-limit check recorded successful requests as failures")
	}
}

// TestAdminLoginRateLimit verifies that repeated failed logins lock the client
// out and that a correct credential is then rejected with 429.
func TestAdminLoginRateLimit(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	app, err := New(config.Config{AdminUsername: "admin", AdminPassword: "correct horse", DataDir: t.TempDir(), ListenAddr: ":8080"}, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)

	post := func(payload map[string]any) int {
		t.Helper()
		status, _, _ := (&testAPI{t: t, base: router.URL, client: router.Client()}).request("POST", "/api/admin/session", payload)
		return status
	}

	// The limiter allows 5 failures; the 5th crosses the threshold.
	for i := 0; i < 4; i++ {
		if status := post(map[string]any{"username": "admin", "password": "wrong"}); status != http.StatusUnauthorized {
			t.Fatalf("attempt %d: expected 401, got %d", i+1, status)
		}
	}
	if status := post(map[string]any{"username": "admin", "password": "wrong"}); status != http.StatusTooManyRequests {
		t.Fatalf("5th failure: expected 429, got %d", status)
	}
	// Correct credential is now rejected while locked out.
	if status := post(map[string]any{"username": "admin", "password": "correct horse"}); status != http.StatusTooManyRequests {
		t.Fatalf("correct credential while locked out: expected 429, got %d", status)
	}
}
