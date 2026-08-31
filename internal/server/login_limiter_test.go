package server

import (
	"context"
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
