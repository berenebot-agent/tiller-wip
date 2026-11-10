package server

import (
	"net/http"
	"testing"
	"time"
)

// TestReauthenticationLockoutIsSharedAcrossScreens proves one failure streak
// covers every sensitive operation: a stolen session cannot buy extra password
// guesses by moving between account settings screens. It also
// pins that a lockout is a rate limit, not a wrong-password answer.
func TestReauthenticationLockoutIsSharedAcrossScreens(t *testing.T) {
	t.Parallel()
	_, api, _ := hostedAccountServer(t)

	password := func() (int, map[string]any, http.Header) {
		return api.request("POST", "/api/auth/account/password", map[string]any{
			"current_password": "definitely not the password",
			"new_password":     "a replacement password phrase",
		})
	}
	email := func() (int, map[string]any, http.Header) {
		return api.request("POST", "/api/auth/account/email", map[string]any{
			"new_email": "elsewhere@example.com", "password": "definitely not the password",
		})
	}

	// Spend the streak on one screen.
	for i := 0; i < reauthMaxFailures; i++ {
		if status, payload, _ := password(); status != 401 || payload["error"].(map[string]any)["code"] != "reauth_required" {
			t.Fatalf("attempt %d: %d %v, want 401 reauth_required", i, status, payload)
		}
	}

	// The other screen is locked too, and says so with a retry hint rather than
	// claiming the password was wrong.
	status, payload, header := email()
	if status != http.StatusTooManyRequests || payload["error"].(map[string]any)["code"] != "authentication_rate_limited" {
		t.Fatalf("email screen after lockout: %d %v, want 429 authentication_rate_limited", status, payload)
	}
	if header.Get("Retry-After") == "" {
		t.Fatal("lockout response must carry Retry-After")
	}

	// A correct password is also refused while locked: the lockout is not
	// bypassable by finally guessing right.
	status, payload, _ = api.request("POST", "/api/auth/account/password", map[string]any{
		"current_password": hostedTestPassword,
		"new_password":     "a replacement password phrase",
	})
	if status != http.StatusTooManyRequests || payload["error"].(map[string]any)["code"] != "authentication_rate_limited" {
		t.Fatalf("correct password during lockout: %d %v, want 429", status, payload)
	}
}

// TestReauthenticationLockoutIsATimer proves the lockout is a timeout, not a
// permanent state: once its window lapses the same user can confirm again.
func TestReauthenticationLockoutIsATimer(t *testing.T) {
	t.Parallel()
	app, api, _ := hostedAccountServer(t)
	// A lockout with a lapsed window behaves like an expired entry.
	app.reauthLimiter = newLoginLimiter(reauthMaxFailures, time.Nanosecond, time.Nanosecond)

	for i := 0; i < reauthMaxFailures; i++ {
		api.request("POST", "/api/auth/account/password", map[string]any{
			"current_password": "wrong", "new_password": "a replacement password phrase",
		})
	}
	// The streak is spent but its lockout has already expired, so a correct
	// password is admitted again.
	status, payload, _ := api.request("POST", "/api/auth/account/password", map[string]any{
		"current_password": hostedTestPassword, "new_password": "a replacement password phrase",
	})
	if status != http.StatusOK {
		t.Fatalf("password change after the lockout lapsed: %d %v, want 200", status, payload)
	}
}

// TestReauthenticationSuccessClearsStreak proves a correct password resets the
// failure count, so occasional typos never accumulate into a lockout.
func TestReauthenticationSuccessClearsStreak(t *testing.T) {
	t.Parallel()
	app, api, _ := hostedAccountServer(t)
	var userID string
	if err := app.db.SQL.QueryRow(`SELECT id FROM users WHERE email=?`, "owner@example.com").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < reauthMaxFailures-1; i++ {
		api.request("POST", "/api/auth/account/password", map[string]any{
			"current_password": "wrong", "new_password": "a replacement password phrase",
		})
	}
	if app.reauthLimiter.locked(userID) {
		t.Fatal("must not lock out one attempt short of the threshold")
	}
	// One more wrong attempt crosses the threshold.
	if status, _, _ := api.request("POST", "/api/auth/account/password", map[string]any{
		"current_password": "wrong", "new_password": "a replacement password phrase",
	}); status != http.StatusUnauthorized {
		t.Fatalf("threshold attempt = %d, want 401 (the request that trips it still reports the mismatch)", status)
	}
	if !app.reauthLimiter.locked(userID) {
		t.Fatal("expected a lockout after the threshold attempt")
	}
	app.reauthLimiter.success(userID)
	if app.reauthLimiter.locked(userID) {
		t.Fatal("success must clear the failure streak")
	}
}

// rawUserSessionTokenFrom returns the session token the test API's cookie jar
// holds, so a test can mint a re-auth grant for it.
func rawUserSessionTokenFrom(t *testing.T, api *testAPI) string {
	t.Helper()
	req, err := http.NewRequest("GET", api.base+"/api/auth/session", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range api.client.Jar.Cookies(req.URL) {
		if cookie.Name == userSessionCookie {
			return cookie.Value
		}
	}
	t.Fatal("no user session cookie found")
	return ""
}
