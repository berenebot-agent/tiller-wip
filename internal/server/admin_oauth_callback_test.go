package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
)

// callbackTransport routes the Codex token exchange (auth.openai.com) to a mock
// while letting all other requests pass through.
type callbackTransport struct {
	oauthServer *httptest.Server
}

func (t *callbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.Host, "auth.openai.com") {
		req.URL.Scheme = "http"
		req.URL.Host = strings.TrimPrefix(t.oauthServer.URL, "http://")
		return http.DefaultTransport.RoundTrip(req)
	}
	return http.DefaultTransport.RoundTrip(req)
}

// newHostedCallbackHarness builds a hosted server with one codex provider seeded
// directly, so the unauthenticated redirect callback can be exercised without a
// hosted customer session.
func newHostedCallbackHarness(t *testing.T) (*Server, *database.DB, string) {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	app := newTestServer(t, config.Config{
		Mode:                        config.ModeHosted,
		TillerPlatformAdminUser:     "platform-admin",
		TillerPlatformAdminPassword: "correct horse",
		PublicURL:                   "https://tiller.example.com",
		TrustedProxy:                netip.MustParsePrefix("127.0.0.1/32"),
		DataDir:                     t.TempDir(),
	}, db)
	now := database.Now()
	if _, err := db.SQL.Exec(`INSERT INTO namespaces(name,kind,entity_id) VALUES('codex-mock','real','provider-cb')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO providers(id,name,type,base_url,enabled,protocols,created_at,updated_at) VALUES('provider-cb','codex-mock','codex-subscription','https://chatgpt.com/backend-api/codex',1,'["responses"]',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	return app, db, "provider-cb"
}

// beginCallbackFlow seeds an active flow and returns its state plus generation.
func beginCallbackFlow(t *testing.T, app *Server, providerID string) (string, int64) {
	t.Helper()
	ctx := context.Background()
	generation, err := app.scopeFor(database.LocalAccountID).OAuthGeneration(ctx, providerID)
	if err != nil {
		t.Fatal(err)
	}
	flow, err := app.oauthFlows.BeginWithGeneration(database.LocalAccountID, providerID, "https://tiller.example.com/auth/callback", generation)
	if err != nil {
		t.Fatal(err)
	}
	return flow.PKCE.State, generation
}

func TestHostedOAuthRedirectCallbackCompletesCodexFlow(t *testing.T) {
	app, db, providerID := newHostedCallbackHarness(t)
	var grants atomic.Int32
	oauthServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("code") != "auth-code" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		grants.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "cb-access", "refresh_token": "cb-refresh", "token_type": "Bearer", "expires_in": 3600, "scope": "openid profile email offline_access"})
	}))
	t.Cleanup(oauthServer.Close)
	app.providers.Registry().SetHTTPClient(&http.Client{Transport: &callbackTransport{oauthServer: oauthServer}})

	state, _ := beginCallbackFlow(t, app, providerID)
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)

	resp, err := http.Get(router.URL + "/auth/callback?code=auth-code&state=" + url.QueryEscape(state))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Connected") {
		t.Fatalf("callback = %d %q, want 200 Connected", resp.StatusCode, string(body))
	}
	if grants.Load() != 1 {
		t.Fatalf("authorization_code grants = %d, want 1", grants.Load())
	}
	record := getOAuthToken(t, db, providerID)
	if record.AccessToken != "cb-access" || record.RefreshToken != "cb-refresh" {
		t.Fatalf("stored token = %+v", record)
	}

	// The state is single-use: replaying the callback must fail, not re-exchange.
	resp, err = http.Get(router.URL + "/auth/callback?code=auth-code&state=" + url.QueryEscape(state))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("replayed callback = %d, want 400", resp.StatusCode)
	}
	if grants.Load() != 1 {
		t.Fatalf("authorization_code grants after replay = %d, want 1", grants.Load())
	}
}

func TestHostedOAuthRedirectCallbackRejectsBadState(t *testing.T) {
	app, _, _ := newHostedCallbackHarness(t)
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)

	resp, err := http.Get(router.URL + "/auth/callback?code=auth-code&state=unknown")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad state callback = %d, want 400", resp.StatusCode)
	}
}

func TestHostedOAuthRedirectCallbackWithoutStateGivesPasteHelp(t *testing.T) {
	app, _, _ := newHostedCallbackHarness(t)
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)

	resp, err := http.Get(router.URL + "/auth/callback")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "copy this page") {
		t.Fatalf("no-state callback = %d %q, want 200 paste help", resp.StatusCode, string(body))
	}
}

func TestHostedOAuthRedirectCallbackProviderError(t *testing.T) {
	app, _, providerID := newHostedCallbackHarness(t)
	state, _ := beginCallbackFlow(t, app, providerID)
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)

	resp, err := http.Get(router.URL + "/auth/callback?error=access_denied&state=" + url.QueryEscape(state))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "declined") {
		t.Fatalf("provider-error callback = %d %q, want 200 declined", resp.StatusCode, string(body))
	}
}

func TestHostedOAuthRedirectCallbackDisconnectRace(t *testing.T) {
	app, _, providerID := newHostedCallbackHarness(t)
	oauthServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "late-access", "refresh_token": "late-refresh", "token_type": "Bearer", "expires_in": 3600, "scope": "openid profile email offline_access"})
	}))
	t.Cleanup(oauthServer.Close)
	app.providers.Registry().SetHTTPClient(&http.Client{Transport: &callbackTransport{oauthServer: oauthServer}})

	state, _ := beginCallbackFlow(t, app, providerID)
	// A disconnect advances the generation after the flow began, so the token
	// write must be rejected rather than resurrecting the connection.
	if _, err := app.scopeFor(database.LocalAccountID).AdvanceOAuthGeneration(context.Background(), providerID); err != nil {
		t.Fatal(err)
	}
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)

	resp, err := http.Get(router.URL + "/auth/callback?code=auth-code&state=" + url.QueryEscape(state))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "disconnected") {
		t.Fatalf("disconnect-race callback = %d %q, want 409 disconnected", resp.StatusCode, string(body))
	}
}
