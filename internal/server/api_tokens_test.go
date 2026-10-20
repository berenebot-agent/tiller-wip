package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/auth"
	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/testutil/fastsecret"
)

func newManagementHarness(t *testing.T) (*testAPI, *database.DB, *Server) {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	app := newTenantServer(t, db)
	api := loginTestAPI(t, app)
	return api, db, app
}

func createManagementToken(t *testing.T, api *testAPI, name string, scopes map[string]int, expires string) (id, token string) {
	t.Helper()
	body := map[string]any{"name": name, "description": "test", "scopes": scopes}
	if expires != "" {
		body["expires_at"] = expires
	}
	status, payload, _ := api.request("POST", "/api/admin/api-tokens", body)
	if status != 201 {
		t.Fatalf("create management token: %d %v", status, payload)
	}
	return payload["id"].(string), payload["token"].(string)
}

// bearerRequest issues a request with no cookie jar and the given bearer token.
func bearerRequest(t *testing.T, base, token, method, path string, body map[string]any) (int, map[string]any, http.Header) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	payload := map[string]any{}
	if strings.Contains(resp.Header.Get("Content-Type"), "json") {
		_ = json.NewDecoder(resp.Body).Decode(&payload)
	}
	return resp.StatusCode, payload, resp.Header.Clone()
}

// requestAuth issues a request through the cookie-jar client but with an
// explicit Authorization header (or none) and never a CSRF token, so tests can
// prove which authentication path decided the outcome.
func (a *testAPI) requestAuth(method, path string, body any, authorization string) (int, map[string]any) {
	a.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			a.t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, a.base+path, reader)
	if err != nil {
		a.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	payload := map[string]any{}
	if strings.Contains(resp.Header.Get("Content-Type"), "json") {
		_ = json.NewDecoder(resp.Body).Decode(&payload)
	}
	return resp.StatusCode, payload
}

func TestManagementTokenLifecycle(t *testing.T) {
	t.Parallel()
	api, _, _ := newManagementHarness(t)
	id, token := createManagementToken(t, api, "hermes", map[string]int{"providers": auth.ScopeRead}, "")
	if !strings.HasPrefix(token, "mt-tr-") {
		t.Fatalf("token prefix = %s", token)
	}

	status, payload, _ := api.request("GET", "/api/admin/api-tokens", nil)
	if status != 200 {
		t.Fatalf("list tokens: %d %v", status, payload)
	}
	data := payload["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("token count = %d", len(data))
	}
	row := data[0].(map[string]any)
	if row["name"] != "hermes" {
		t.Fatalf("row name = %v", row["name"])
	}
	if _, leaked := row["token"]; leaked {
		t.Fatal("list response leaked a plaintext token")
	}
	if _, leaked := row["secret"]; leaked {
		t.Fatal("list response leaked a secret field")
	}

	// The bearer path works with no cookie and no CSRF token.
	status, _, _ = bearerRequest(t, api.base, token, "GET", "/api/admin/providers", nil)
	if status != 200 {
		t.Fatalf("bearer read providers = %d", status)
	}

	// Rename and re-scope.
	status, payload, _ = api.request("PATCH", "/api/admin/api-tokens/"+id, map[string]any{"name": "renamed", "scopes": map[string]int{"virtual": auth.ScopeWrite}})
	if status != 200 {
		t.Fatalf("patch token: %d %v", status, payload)
	}
	status, _, _ = bearerRequest(t, api.base, token, "GET", "/api/admin/providers", nil)
	if status != 403 {
		t.Fatalf("stale scope still allowed providers read: %d", status)
	}
	status, _, _ = bearerRequest(t, api.base, token, "GET", "/api/admin/virtual-models", nil)
	if status != 200 {
		t.Fatalf("new scope denied virtual read: %d", status)
	}

	// Rotate: the old plaintext is immediately dead.
	status, payload, _ = api.request("POST", "/api/admin/api-tokens/"+id+"/rotate", nil)
	if status != 200 {
		t.Fatalf("rotate: %d %v", status, payload)
	}
	rotated := payload["token"].(string)
	if rotated == token || !strings.HasPrefix(rotated, "mt-tr-") {
		t.Fatal("rotation did not mint a new token")
	}
	status, _, _ = bearerRequest(t, api.base, token, "GET", "/api/admin/virtual-models", nil)
	if status != 401 {
		t.Fatalf("old token after rotate = %d", status)
	}
	status, _, _ = bearerRequest(t, api.base, rotated, "GET", "/api/admin/virtual-models", nil)
	if status != 200 {
		t.Fatalf("rotated token = %d", status)
	}

	// Delete.
	status, _, _ = api.request("DELETE", "/api/admin/api-tokens/"+id, nil)
	if status != 204 {
		t.Fatalf("delete: %d", status)
	}
	status, _, _ = bearerRequest(t, api.base, rotated, "GET", "/api/admin/virtual-models", nil)
	if status != 401 {
		t.Fatalf("deleted token = %d", status)
	}
}

func TestManagementTokenScopeCeilings(t *testing.T) {
	t.Parallel()
	api, _, _ := newManagementHarness(t)
	readID, readToken := createManagementToken(t, api, "reader", map[string]int{"providers": auth.ScopeRead}, "")
	writeID, writeToken := createManagementToken(t, api, "writer", map[string]int{"providers": auth.ScopeWrite}, "")
	_, deleteToken := createManagementToken(t, api, "deleter", map[string]int{"providers": auth.ScopeDelete}, "")
	_, monitorToken := createManagementToken(t, api, "monitor", map[string]int{"monitoring": auth.ScopeRead}, "")
	_ = readID
	_ = writeID

	// Read ceiling: GET yes, mutations no.
	status, _, _ := bearerRequest(t, api.base, readToken, "GET", "/api/admin/providers", nil)
	if status != 200 {
		t.Fatalf("read GET = %d", status)
	}
	status, payload, _ := bearerRequest(t, api.base, readToken, "POST", "/api/admin/providers", map[string]any{"name": "denied", "type": "generic-openai", "base_url": "http://127.0.0.1:9/v1"})
	if status != 403 || errorCode(payload) != "insufficient_scope" {
		t.Fatalf("read POST = %d %v", status, payload)
	}
	if payload["error"].(map[string]any)["required"] != "providers:write" {
		t.Fatalf("required scope = %v", payload["error"])
	}
	status, _, _ = bearerRequest(t, api.base, readToken, "DELETE", "/api/admin/providers/none", nil)
	if status != 403 {
		t.Fatalf("read DELETE = %d", status)
	}
	status, _, _ = bearerRequest(t, api.base, readToken, "GET", "/api/admin/settings", nil)
	if status != 403 {
		t.Fatalf("read token crossed areas: %d", status)
	}

	// Write ceiling: create yes, delete no.
	status, payload, _ = bearerRequest(t, api.base, writeToken, "POST", "/api/admin/providers", map[string]any{"name": "from-bearer", "type": "generic-openai", "base_url": "http://127.0.0.1:9/v1"})
	if status != 201 {
		t.Fatalf("write POST = %d %v", status, payload)
	}
	providerID := payload["id"].(string)
	status, _, _ = bearerRequest(t, api.base, writeToken, "DELETE", "/api/admin/providers/"+providerID, nil)
	if status != 403 {
		t.Fatalf("write DELETE = %d", status)
	}

	// Delete ceiling covers write and read.
	status, payload, _ = bearerRequest(t, api.base, deleteToken, "POST", "/api/admin/providers", map[string]any{"name": "delete-tier-create", "type": "generic-openai", "base_url": "http://127.0.0.1:9/v1"})
	if status != 201 {
		t.Fatalf("delete-tier POST = %d %v", status, payload)
	}
	status, _, _ = bearerRequest(t, api.base, deleteToken, "DELETE", "/api/admin/providers/"+providerID, nil)
	if status != 204 {
		t.Fatalf("delete-tier DELETE = %d", status)
	}

	// Monitoring: reads allowed, clears denied.
	status, _, _ = bearerRequest(t, api.base, monitorToken, "GET", "/api/admin/activity", nil)
	if status != 200 {
		t.Fatalf("monitoring read = %d", status)
	}
	status, _, _ = bearerRequest(t, api.base, monitorToken, "DELETE", "/api/admin/cooldown", nil)
	if status != 403 {
		t.Fatalf("monitoring clear = %d", status)
	}
}

func TestManagementTokenBlockedRoutes(t *testing.T) {
	t.Parallel()
	api, _, _ := newManagementHarness(t)
	_, token := createManagementToken(t, api, "full-power", map[string]int{
		"providers": auth.ScopeDelete, "virtual": auth.ScopeDelete, "client_keys": auth.ScopeDelete,
		"settings": auth.ScopeDelete, "monitoring": auth.ScopeDelete,
	}, "")
	for _, path := range []string{
		"/api/admin/backup/export",
		"/api/admin/account",
		"/api/admin/api-tokens",
		"/api/admin/live",
		"/api/admin/session",
	} {
		status, _, _ := bearerRequest(t, api.base, token, "GET", path, nil)
		if status != 401 {
			t.Fatalf("token reached blocked route %s: %d", path, status)
		}
	}
	// A management token never authenticates inference.
	status, _, _ := bearerRequest(t, api.base, token, "POST", "/v1/chat/completions", map[string]any{"model": "main", "messages": []any{}})
	if status != 401 {
		t.Fatalf("management token on /v1 = %d", status)
	}

	// A client key never authenticates the management surface.
	status, payload, _ := api.request("POST", "/api/admin/client-keys", map[string]any{"name": "inference client"})
	if status != 201 {
		t.Fatalf("create client key: %d %v", status, payload)
	}
	clientSecret := payload["secret"].(string)
	status, payload, _ = bearerRequest(t, api.base, clientSecret, "GET", "/api/admin/providers", nil)
	if status != 401 || errorCode(payload) != "invalid_api_token" {
		t.Fatalf("client key on management surface = %d %v", status, payload)
	}
}

func TestManagementTokenCSRFBoundary(t *testing.T) {
	t.Parallel()
	api, _, _ := newManagementHarness(t)
	_, writeToken := createManagementToken(t, api, "csrf-writer", map[string]int{"client_keys": auth.ScopeWrite}, "")

	// A bearer request mutates without any CSRF token.
	status, _, _ := bearerRequest(t, api.base, writeToken, "POST", "/api/admin/client-keys", map[string]any{"name": "via-bearer"})
	if status != 201 {
		t.Fatalf("bearer mutation with no CSRF = %d", status)
	}

	// The cookie path without CSRF is still rejected.
	status, payload := api.requestAuth("POST", "/api/admin/client-keys", map[string]any{"name": "via-cookie-no-csrf"}, "")
	if status != 403 || errorCode(payload) != "csrf_failed" {
		t.Fatalf("cookie POST without CSRF = %d %v", status, payload)
	}

	// An invalid bearer is terminal even when a valid admin cookie is present.
	status, payload = api.requestAuth("GET", "/api/admin/providers", nil, "Bearer mt-tr-AAAAAAAAAAAA.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	if status != 401 || errorCode(payload) != "invalid_api_token" {
		t.Fatalf("invalid bearer with valid cookie = %d %v", status, payload)
	}

	// Any Authorization header — even a non-Bearer one — selects the token
	// path; it must never downgrade to cookie authentication.
	status, payload = api.requestAuth("GET", "/api/admin/providers", nil, "Basic Zm9vOmJhcg==")
	if status != 401 || errorCode(payload) != "invalid_api_token" {
		t.Fatalf("non-bearer Authorization with valid cookie = %d %v", status, payload)
	}

	// A valid but out-of-scope bearer also wins over the cookie: the token
	// path decides, the cookie is never consulted.
	status, payload = api.requestAuth("GET", "/api/admin/providers", nil, "Bearer "+writeToken)
	if status != 403 || errorCode(payload) != "insufficient_scope" {
		t.Fatalf("out-of-scope bearer with valid cookie = %d %v", status, payload)
	}
}

func TestManagementTokenSelf(t *testing.T) {
	t.Parallel()
	api, _, _ := newManagementHarness(t)
	_, token := createManagementToken(t, api, "introspect", map[string]int{"monitoring": auth.ScopeRead}, "")

	status, payload, _ := bearerRequest(t, api.base, token, "GET", "/api/admin/api-tokens/self", nil)
	if status != 200 {
		t.Fatalf("self: %d %v", status, payload)
	}
	if payload["name"] != "introspect" {
		t.Fatalf("self name = %v", payload["name"])
	}
	scopes, _ := payload["scopes"].(map[string]any)
	if scopes["monitoring"] != float64(auth.ScopeRead) {
		t.Fatalf("self scopes = %v", payload["scopes"])
	}
	for _, leaked := range []string{"token", "secret", "secret_hash"} {
		if _, ok := payload[leaked]; ok {
			t.Fatalf("self leaked %q", leaked)
		}
	}

	// A browser session has no token principal.
	status, _ = api.requestAuth("GET", "/api/admin/api-tokens/self", nil, "")
	if status != 404 {
		t.Fatalf("cookie caller on self = %d", status)
	}
}

func TestManagementTokenExpiryAndDisable(t *testing.T) {
	t.Parallel()
	api, db, _ := newManagementHarness(t)
	id, token := createManagementToken(t, api, "expiring", map[string]int{"providers": auth.ScopeRead}, "")

	// Warm the cache, then disable: the change must take effect immediately.
	status, _, _ := bearerRequest(t, api.base, token, "GET", "/api/admin/providers", nil)
	if status != 200 {
		t.Fatalf("warm read = %d", status)
	}
	status, payload, _ := api.request("PATCH", "/api/admin/api-tokens/"+id, map[string]any{"enabled": false})
	if status != 200 {
		t.Fatalf("disable: %d %v", status, payload)
	}
	status, payload, _ = bearerRequest(t, api.base, token, "GET", "/api/admin/providers", nil)
	if status != 401 || errorCode(payload) != "invalid_api_token" {
		t.Fatalf("disabled token = %d %v", status, payload)
	}

	// Re-enable, then expire the row directly (the API rejects past expiry).
	status, payload, _ = api.request("PATCH", "/api/admin/api-tokens/"+id, map[string]any{"enabled": true})
	if status != 200 {
		t.Fatalf("re-enable: %d %v", status, payload)
	}
	if _, err := db.SQL.Exec(`UPDATE api_tokens SET expires_at=? WHERE id=?`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), id); err != nil {
		t.Fatal(err)
	}
	status, payload, _ = bearerRequest(t, api.base, token, "GET", "/api/admin/providers", nil)
	if status != 401 || errorCode(payload) != "token_expired" {
		t.Fatalf("expired token = %d %v", status, payload)
	}

	// Extending the expiry revives it.
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	status, payload, _ = api.request("PATCH", "/api/admin/api-tokens/"+id, map[string]any{"expires_at": future})
	if status != 200 {
		t.Fatalf("extend: %d %v", status, payload)
	}
	status, _, _ = bearerRequest(t, api.base, token, "GET", "/api/admin/providers", nil)
	if status != 200 {
		t.Fatalf("revived token = %d", status)
	}

	// A past expiry is rejected up front.
	status, payload, _ = api.request("PATCH", "/api/admin/api-tokens/"+id, map[string]any{"expires_at": "2020-01-01T00:00:00Z"})
	if status != 400 || errorCode(payload) != "invalid_expiry" {
		t.Fatalf("past expiry = %d %v", status, payload)
	}
}

func TestManagementTokenRevokeAll(t *testing.T) {
	t.Parallel()
	api, _, _ := newManagementHarness(t)
	_, first := createManagementToken(t, api, "one", map[string]int{"providers": auth.ScopeRead}, "")
	_, second := createManagementToken(t, api, "two", map[string]int{"providers": auth.ScopeRead}, "")

	status, payload, _ := api.request("POST", "/api/admin/api-tokens/revoke-all", nil)
	if status != 200 || payload["revoked"] != float64(2) {
		t.Fatalf("revoke-all = %d %v", status, payload)
	}
	for _, token := range []string{first, second} {
		status, _, _ = bearerRequest(t, api.base, token, "GET", "/api/admin/providers", nil)
		if status != 401 {
			t.Fatalf("revoked token = %d", status)
		}
	}
	status, payload, _ = api.request("GET", "/api/admin/api-tokens", nil)
	if status != 200 || len(payload["data"].([]any)) != 0 {
		t.Fatalf("list after revoke-all = %d %v", status, payload)
	}
}

func TestManagementTokenAuthFailureLockout(t *testing.T) {
	t.Parallel()
	api, _, _ := newManagementHarness(t)
	_, token := createManagementToken(t, api, "lockout", map[string]int{"providers": auth.ScopeRead}, "")

	// Corrupt the secret while keeping the selector.
	wrong := token[:len(token)-1]
	if token[len(token)-1] == 'A' {
		wrong += "B"
	} else {
		wrong += "A"
	}
	sawLockout := false
	for i := 0; i < 25 && !sawLockout; i++ {
		status, _, headers := bearerRequest(t, api.base, wrong, "GET", "/api/admin/providers", nil)
		if status == 429 {
			sawLockout = headers.Get("Retry-After") != ""
		} else if status != 401 {
			t.Fatalf("wrong-secret attempt %d = %d", i, status)
		}
	}
	if !sawLockout {
		t.Fatal("repeated bad tokens never locked out")
	}

	// While locked, even the correct token is refused.
	status, _, _ := bearerRequest(t, api.base, token, "GET", "/api/admin/providers", nil)
	if status != 401 {
		t.Fatalf("locked-out valid token = %d", status)
	}
}

func TestManagementTokenDuplicateNameConflicts(t *testing.T) {
	t.Parallel()
	api, _, _ := newManagementHarness(t)
	createManagementToken(t, api, "dup", map[string]int{"providers": auth.ScopeRead}, "")
	status, payload, _ := api.request("POST", "/api/admin/api-tokens", map[string]any{"name": "dup", "scopes": map[string]int{"providers": auth.ScopeRead}})
	if status != 409 || errorCode(payload) != "name_conflict" {
		t.Fatalf("duplicate name = %d %v", status, payload)
	}
}

func TestManagementTokenInvalidScopesRejected(t *testing.T) {
	t.Parallel()
	api, _, _ := newManagementHarness(t)
	for _, scopes := range []map[string]int{{"bogus": auth.ScopeRead}, {"providers": 9}, {"providers": 0}} {
		status, payload, _ := api.request("POST", "/api/admin/api-tokens", map[string]any{"name": "bad", "scopes": scopes})
		if status != 400 || errorCode(payload) != "invalid_scopes" {
			t.Fatalf("scopes %v = %d %v", scopes, status, payload)
		}
	}
}

// TestAdminAPIKeyBootSeed creates a full-power token from the environment seed
// and proves it authenticates the admin surface, and that a re-seed with a new
// value replaces the old credential.
func TestAdminAPIKeyBootSeed(t *testing.T) {
	t.Parallel()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	first := "mt-tr-B9HQyF1IQyLu.NJh92J65MNar2AZWWJU6rzFB8eHTYAabB6SYEHF7rLA"
	app, err := New(config.Config{TillerUser: "admin", TillerUserPassword: "correct horse", DataDir: t.TempDir(), AdminAPIKey: first}, db, slog.New(slog.NewTextHandler(io.Discard, nil)), withSecretHasher(fastsecret.Hasher{}))
	if err != nil {
		t.Fatal(err)
	}
	app.liveHub.timings = testLiveTimings
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)

	// A fresh seed token authenticates with full provider write/delete power.
	status, _, _ := bearerRequest(t, router.URL, first, "GET", "/api/admin/providers", nil)
	if status != 200 {
		t.Fatalf("seed token read = %d", status)
	}
	status, _, _ = bearerRequest(t, router.URL, first, "GET", "/api/admin/settings", nil)
	if status != 200 {
		t.Fatalf("seed token settings read (full power) = %d", status)
	}

	// The store holds exactly one token, named boot-admin.
	var name string
	if err := db.SQL.QueryRow(`SELECT name FROM api_tokens WHERE id='boot-admin'`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "boot-admin" {
		t.Fatalf("seed token name = %q", name)
	}

	// Re-seed with a different value (simulating a restart after rotation):
	// the fixed id replaces the credential and the old value stops working.
	second := "mt-tr-B6jSqFaAl2ll.0qqcglnGU-1uqIueba2qmMXvWXnNE5KWaSy_bkTRzQA"
	app2, err := New(config.Config{TillerUser: "admin", TillerUserPassword: "correct horse", DataDir: t.TempDir(), AdminAPIKey: second}, db, slog.New(slog.NewTextHandler(io.Discard, nil)), withSecretHasher(fastsecret.Hasher{}))
	if err != nil {
		t.Fatal(err)
	}
	app2.liveHub.timings = testLiveTimings
	router2 := httptest.NewServer(app2.Handler())
	t.Cleanup(router2.Close)

	if status, _, _ := bearerRequest(t, router2.URL, first, "GET", "/api/admin/providers", nil); status != 401 {
		t.Fatalf("old seed token after re-seed = %d, want 401", status)
	}
	if status, _, _ := bearerRequest(t, router2.URL, second, "GET", "/api/admin/providers", nil); status != 200 {
		t.Fatalf("new seed token after re-seed = %d", status)
	}
	var rows int
	if err := db.SQL.QueryRow(`SELECT count(*) FROM api_tokens`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("token rows after re-seed = %d, want 1", rows)
	}
}

func TestAdminAPIKeyCountsAsConfigured(t *testing.T) {
	t.Parallel()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	// No username/password; only the seed key. The instance must not report
	// itself as needing first-run setup.
	app := newTestServer(t, config.Config{DataDir: t.TempDir(), AdminAPIKey: "mt-tr-B9HQyF1IQyLu.NJh92J65MNar2AZWWJU6rzFB8eHTYAabB6SYEHF7rLA"}, db)
	if app.setupRequired() {
		t.Fatal("a management API boot seed must count as configured")
	}
}

func TestManagementTokenEmptyAuthorizationNeverFallsBack(t *testing.T) {
	t.Parallel()
	api, _, _ := newManagementHarness(t)
	for _, present := range []bool{false, true} {
		req, err := http.NewRequest("GET", api.base+"/api/admin/providers", nil)
		if err != nil {
			t.Fatal(err)
		}
		if present {
			req.Header.Set("Authorization", "")
		}
		resp, err := api.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		want := http.StatusOK
		if present {
			want = http.StatusUnauthorized
			if resp.Header.Get("WWW-Authenticate") == "" {
				t.Fatal("empty authorization did not select bearer authentication")
			}
		}
		if resp.StatusCode != want {
			t.Fatalf("header present %v: status %d, want %d", present, resp.StatusCode, want)
		}
	}
}

func TestAdminAPIKeyEnvironmentManagedLifecycle(t *testing.T) {
	t.Parallel()
	api, db, app := newManagementHarness(t)
	seed := "mt-tr-B9HQyF1IQyLu.NJh92J65MNar2AZWWJU6rzFB8eHTYAabB6SYEHF7rLA"
	app.config.AdminAPIKey = seed
	if err := app.seedManagementToken(seed); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []struct {
		method string
		path   string
		body   map[string]any
	}{
		{"PATCH", "/api/admin/api-tokens/boot-admin", map[string]any{"enabled": false}},
		{"PATCH", "/api/admin/api-tokens/boot-admin", map[string]any{"scopes": map[string]int{"providers": auth.ScopeRead}}},
		{"PATCH", "/api/admin/api-tokens/boot-admin", map[string]any{"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)}},
		{"PATCH", "/api/admin/api-tokens/boot-admin", map[string]any{"name": "renamed", "description": "changed"}},
		{"POST", "/api/admin/api-tokens/boot-admin/rotate", nil},
		{"DELETE", "/api/admin/api-tokens/boot-admin", nil},
	} {
		status, payload, _ := api.request(mutation.method, mutation.path, mutation.body)
		if status != 409 || errorCode(payload) != "environment_managed_token" {
			t.Fatalf("%s %s: %d", mutation.method, mutation.path, status)
		}
	}
	status, self, _ := bearerRequest(t, api.base, seed, "GET", "/api/admin/api-tokens/self", nil)
	if status != 200 || self["environment_managed"] != true {
		t.Fatal("bootstrap identity not marked environment-managed")
	}
	_, ordinary := createManagementToken(t, api, "ordinary", map[string]int{"providers": auth.ScopeRead}, "")
	status, result, _ := api.request("POST", "/api/admin/api-tokens/revoke-all", nil)
	if status != 200 || result["revoked"] != float64(1) || result["environment_managed_excluded"] != true {
		t.Fatalf("bulk revoke result: %d %v", status, result)
	}
	if status, _, _ := bearerRequest(t, api.base, ordinary, "GET", "/api/admin/providers", nil); status != 401 {
		t.Fatal("ordinary token survived bulk revocation")
	}
	if status, _, _ := bearerRequest(t, api.base, seed, "GET", "/api/admin/providers", nil); status != 200 {
		t.Fatal("bootstrap token was revoked by bulk revocation")
	}
	_, retained := createManagementToken(t, api, "retained", map[string]int{"providers": auth.ScopeRead}, "")
	for _, configured := range []string{seed, ""} {
		cfg := app.config
		cfg.AdminAPIKey = configured
		restarted, err := New(cfg, db, slog.New(slog.NewTextHandler(io.Discard, nil)), withSecretHasher(fastsecret.Hasher{}))
		if err != nil {
			t.Fatal(err)
		}
		router := httptest.NewServer(restarted.Handler())
		want := 200
		if configured == "" {
			want = 401
		}
		status, _, _ := bearerRequest(t, router.URL, seed, "GET", "/api/admin/providers", nil)
		retainedStatus, _, _ := bearerRequest(t, router.URL, retained, "GET", "/api/admin/providers", nil)
		router.Close()
		if status != want {
			t.Fatalf("restart with key configured %v: %d, want %d", configured != "", status, want)
		}
		if retainedStatus != 200 {
			t.Fatal("configuration change revoked a browser-managed token")
		}
	}
}
