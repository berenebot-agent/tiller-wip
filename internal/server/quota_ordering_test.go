package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/providers/oauth"
)

// quotaProbeInterval spaces the test's quota reads. The dashboard limiter allows
// 60 management reads per minute per account, so a tight poll loop would trip it
// before the check it is waiting on completes.
// waitFor polls cond until it holds or a bounded deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestQuotaPollBeforeOAuthConnectIsNotAFailure is the regression for the
// provider-add sequence: creating an OAuth provider polls its quota before the
// user can possibly have finished signing in. That pre-connection poll is a
// pre-condition, not a failed check, so the card must not report a failure (it
// previously stayed on "Check failed" for the whole cadence), and the poll that
// follows the connection must run immediately rather than waiting out the
// interval the pre-connection attempt stamped.
func TestQuotaPollBeforeOAuthConnectIsNotAFailure(t *testing.T) {
	var usageCalls int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{map[string]any{"slug": "gpt-5.6-sol", "display_name": "gpt-5.6-sol", "supported_in_api": true}}})
		case "/wham/usage", "/codex/usage", "/backend-api/wham/usage":
			atomic.AddInt64(&usageCalls, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"plan_type": "plus",
				"rate_limit": map[string]any{
					"primary_window": map[string]any{"used_percent": 21.0, "limit_window_seconds": 18000},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// The session's account is the local operator account; resolve it rather than
	// assuming a constant, because the store's local account id is an
	// implementation detail.
	accountID := database.LocalAccountID

	app := newTestServer(t, config.Config{TillerUser: "admin", TillerUserPassword: "correct horse", DataDir: t.TempDir(), ListenAddr: ":8080"}, db)
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)

	jar, _ := cookiejar.New(nil)
	api := &testAPI{t: t, base: router.URL, client: &http.Client{Jar: jar}, server: app}
	status, payload, _ := api.request("POST", "/api/admin/session", map[string]any{"username": "admin", "password": "correct horse"})
	if status != 200 {
		t.Fatalf("login: %d %v", status, payload)
	}
	api.csrf = payload["csrf_token"].(string)

	status, payload, _ = api.request("POST", "/api/admin/providers", map[string]any{"name": "codex-mock", "type": "codex-subscription", "base_url": upstream.URL, "protocols": []any{"responses"}})
	if status != 201 {
		t.Fatalf("create provider: %d %v", status, payload)
	}
	providerID := payload["id"].(string)
	// Creation must not run discovery for an OAuth provider: there is no
	// credential, so it could only record a refresh error the sign-in has to
	// clear again.
	if refreshError, _ := payload["refresh_error"].(string); refreshError != "" {
		t.Fatalf("create reported a discovery failure for an unconnected OAuth provider: %q", refreshError)
	}

	// The UI polls quota immediately after creating the provider. The check runs
	// asynchronously; read the poller directly rather than through the endpoint,
	// which is rate-limited per account.
	status, payload, _ = api.request("GET", "/api/admin/quota", nil)
	if status != 200 {
		t.Fatalf("quota: %d %v", status, payload)
	}
	// The trigger is asynchronous: wait until the check has run and finished.
	waitFor(t, "pre-connection quota check to settle", func() bool {
		state := app.quota.PollStatesForAccount(accountID)[providerID]
		return state.Attempts > 0 && !state.Refreshing
	})
	pre := app.quota.PollStatesForAccount(accountID)[providerID]
	if pre.Failed {
		t.Fatalf("a provider with no OAuth token was recorded as a failed quota check: %+v", pre)
	}
	if !pre.NotConnected {
		t.Fatalf("pre-connection poll status = %+v; want not_connected", pre)
	}
	if pre.LastAttemptAt != nil {
		t.Fatalf("a pre-connection poll stamped an attempt, which would gate the next check: %+v", pre)
	}
	if snap, ok := app.quota.Snapshot(accountID, providerID); ok && snap.Available {
		t.Fatalf("a pre-connection poll reported available quota: %+v", snap)
	}

	// The sign-in completes: the token is stored and the connection hook polls
	// quota straight away, ignoring the interval the pre-connection attempt ran
	// inside.
	future := time.Now().Add(time.Hour)
	putOAuthToken(t, db, oauth.TokenRecord{
		ProviderID:   providerID,
		AccessToken:  "live-token",
		RefreshToken: "refresh-token",
		TokenType:    "Bearer",
		ExpiresAt:    &future,
		AuthState:    oauth.AuthConnected,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	})
	if !app.refreshQuotaAfterConnect(accountID, providerID) {
		t.Fatal("the post-connection hook did not start a quota check")
	}
	waitFor(t, "post-connection quota check to settle", func() bool {
		state := app.quota.PollStatesForAccount(accountID)[providerID]
		return state.Attempts > 1 && !state.Refreshing
	})
	snap, ok := app.quota.Snapshot(accountID, providerID)
	if !ok || !snap.Available {
		t.Fatalf("post-connection snapshot = %+v (present=%v); want available quota", snap, ok)
	}
	if snap.Plan != "plus" {
		t.Fatalf("post-connection snapshot plan = %q; want the value from the quota endpoint", snap.Plan)
	}
	if got := atomic.LoadInt64(&usageCalls); got == 0 {
		t.Fatal("the post-connection poll never reached the quota endpoint")
	}
}
