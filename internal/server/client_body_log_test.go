package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/store"
	"github.com/tiller-router/tiller-router/internal/testutil/fastsecret"
)

// bodyLogUpstream is a mock provider that always fails, so a failed request
// produces the row the detailed-body logging governs.
func bodyLogUpstream(t *testing.T) http.HandlerFunc {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "PROVIDER-ERROR-SECRET-MARKER"}})
	})
}

// setBodyLogMode patches a client key's detailed-error-body mode.
func setBodyLogMode(t *testing.T, api *testAPI, clientID, mode string) int {
	t.Helper()
	status, payload, _ := api.request("PATCH", "/api/admin/client-keys/"+clientID, map[string]any{"log_error_bodies": mode})
	if status != http.StatusNoContent {
		t.Fatalf("set body log mode %q: %d %v", mode, status, payload)
	}
	return status
}

// setAccountBodyLogging switches the account-level detailed-error-logging
// setting on or off.
func setAccountBodyLogging(t *testing.T, db *database.DB, enabled bool) {
	t.Helper()
	value := "0"
	if enabled {
		value = "1"
	}
	if err := store.New(db.SQL).For(database.LocalAccountID).SetSetting(context.Background(), store.SettingLogErrorBodies, value); err != nil {
		t.Fatal(err)
	}
}

// loggedBodies reads the persisted request and provider error bodies for the
// most recent request of a client key.
func loggedBodies(t *testing.T, db *database.DB, clientID string) (int, *string, *string) {
	t.Helper()
	var status int
	var requestBody, errorBody *string
	if err := activityDB(t, db).QueryRow(`SELECT http_status,request_body,error_body FROM request_logs WHERE client_key_id=? ORDER BY created_at DESC LIMIT 1`, clientID).Scan(&status, &requestBody, &errorBody); err != nil {
		t.Fatal(err)
	}
	return status, requestBody, errorBody
}

// failRequest drives one failing request through the router.
func failRequest(t *testing.T, api *testAPI, secret string) {
	t.Helper()
	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "provider-a/model-a", "messages": []any{map[string]any{"role": "user", "content": "CLIENT-REQUEST-SECRET-MARKER"}}})
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("provider error status = %d", resp.StatusCode)
	}
}

// TestClientBodyLogNeverKeepsMetadataButDropsBodies pins the per-client
// "never" mode: the request is still logged as metadata, but no body content
// is stored even though the account setting allows it.
func TestClientBodyLogNeverKeepsMetadataButDropsBodies(t *testing.T) {
	t.Parallel()
	api, db, clientID, secret := loggingTestHarness(t, bodyLogUpstream(t))
	setAccountBodyLogging(t, db, true)
	setBodyLogMode(t, api, clientID, store.BodyLogNever)

	failRequest(t, api, secret)

	status, requestBody, errorBody := loggedBodies(t, db, clientID)
	if status != http.StatusBadGateway {
		t.Fatalf("activity row status = %d", status)
	}
	if requestBody != nil || errorBody != nil {
		t.Fatalf("bodies persisted for a client set to never: request=%v error=%v", requestBody, errorBody)
	}
}

// TestClientBodyLogAlwaysCapturesBodiesWithoutAccountSetting pins the
// per-client "always" mode: bodies are stored even when the account setting
// is off.
func TestClientBodyLogAlwaysCapturesBodiesWithoutAccountSetting(t *testing.T) {
	t.Parallel()
	api, db, clientID, secret := loggingTestHarness(t, bodyLogUpstream(t))
	setAccountBodyLogging(t, db, false)
	setBodyLogMode(t, api, clientID, store.BodyLogAlways)

	failRequest(t, api, secret)

	_, requestBody, errorBody := loggedBodies(t, db, clientID)
	if requestBody == nil || !strings.Contains(*requestBody, "CLIENT-REQUEST-SECRET-MARKER") {
		t.Fatalf("request body was not persisted for a client set to always: %v", requestBody)
	}
	if errorBody == nil || !strings.Contains(*errorBody, "PROVIDER-ERROR-SECRET-MARKER") {
		t.Fatalf("provider error body was not persisted for a client set to always: %v", errorBody)
	}
}

// TestClientBodyLogInheritFollowsAccountSetting pins the default mode: with
// no per-client choice, the account setting decides.
func TestClientBodyLogInheritFollowsAccountSetting(t *testing.T) {
	t.Parallel()
	api, db, clientID, secret := loggingTestHarness(t, bodyLogUpstream(t))
	setAccountBodyLogging(t, db, false)

	failRequest(t, api, secret)

	_, requestBody, errorBody := loggedBodies(t, db, clientID)
	if requestBody != nil || errorBody != nil {
		t.Fatalf("bodies persisted while the account setting is off: request=%v error=%v", requestBody, errorBody)
	}
}

// TestClientBodyLogModeAPI covers the admin surface: the mode round-trips
// and an unknown mode is rejected.
func TestClientBodyLogModeAPI(t *testing.T) {
	t.Parallel()
	api, _, clientID, _ := loggingTestHarness(t, bodyLogUpstream(t))

	for _, mode := range []string{store.BodyLogNever, store.BodyLogAlways, store.BodyLogInherit} {
		setBodyLogMode(t, api, clientID, mode)
		status, payload, _ := api.request("GET", "/api/admin/client-keys", nil)
		if status != http.StatusOK {
			t.Fatalf("list client keys: %d %v", status, payload)
		}
		rows := payload["data"].([]any)
		if len(rows) != 1 {
			t.Fatalf("expected one client key, got %d", len(rows))
		}
		if got := rows[0].(map[string]any)["log_error_bodies"]; got != mode {
			t.Fatalf("client mode = %v, want %q", got, mode)
		}
	}

	status, payload, _ := api.request("PATCH", "/api/admin/client-keys/"+clientID, map[string]any{"log_error_bodies": "sometimes"})
	if status != http.StatusBadRequest {
		t.Fatalf("unknown mode status = %d %v", status, payload)
	}
}

// hostedBodyLogHarness wires a hosted account to a failing mock upstream and
// returns the account's single client key. It mirrors loggingTestHarness but
// authenticates as a hosted customer instead of the local operator, so the
// hosted-mode body-logging path is what gets exercised.
func hostedBodyLogHarness(t *testing.T) (*testAPI, *database.DB, string, string) {
	t.Helper()
	upstream := httptest.NewServer(bodyLogUpstream(t))
	t.Cleanup(upstream.Close)
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	app, err := New(config.Config{
		Mode: config.ModeHosted, TillerUser: "owner@example.com", TillerUserPassword: "correct horse battery staple",
		TillerPlatformAdminUser: "platform-admin", TillerPlatformAdminPassword: "platform-secret",
		PublicURL: "https://tiller.example.com", TrustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}, DataDir: t.TempDir(),
	}, db, slog.New(slog.NewTextHandler(io.Discard, nil)), withSecretHasher(fastsecret.Hasher{}))
	if err != nil {
		t.Fatal(err)
	}
	app.liveHub.timings = testLiveTimings
	app.usageCacheTTL = 0
	// Hosted mode routes provider traffic through the public-HTTPS-only
	// transport, which cannot reach a loopback mock. Point the registry at a
	// plain client so the failing upstream is actually dialled.
	app.providers.Registry().SetHTTPClient(&http.Client{Transport: &http.Transport{}})
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	api := &testAPI{t: t, base: router.URL, client: &http.Client{Jar: jar}, server: app}
	status, payload, _ := api.request("POST", "/api/auth/login", map[string]any{"email": "owner@example.com", "password": "correct horse battery staple"})
	if status != 200 {
		t.Fatalf("customer login: %d %v", status, payload)
	}
	api.csrf = payload["csrf_token"].(string)
	// The hosted provider API refuses non-public HTTPS destinations by design,
	// so the mock upstream is seeded straight into the account's catalogue
	// rather than discovered through the admin endpoint.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.SQL.Exec(`INSERT INTO namespaces(name,kind,entity_id) VALUES('provider-a','real','provider-a-id')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO providers(id,name,type,base_url,credential_secret,enabled,protocols,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, "provider-a-id", "provider-a", "generic-openai", upstream.URL+"/v1", "provider-secret", 1, `["chat"]`, now, now); err != nil {
		t.Fatal(err)
	}
	const modelID = "model-a-id"
	if _, err := db.SQL.Exec(`INSERT INTO provider_models(id,provider_id,upstream_model_id,available,first_seen_at,last_seen_at,created_at,updated_at,context_length,max_output_tokens,supports_tools,supports_vision,supports_reasoning,supports_structured_output) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, modelID, "provider-a-id", "model-a", 1, now, now, now, now, 64000, 4096, 1, 0, nil, 1); err != nil {
		t.Fatal(err)
	}
	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "hosted client", "description": "body log", "type": "catalogue"})
	if status != 201 {
		t.Fatalf("create client key: %d %v", status, payload)
	}
	clientID := payload["id"].(string)
	clientSecret := payload["secret"].(string)
	status, payload, _ = api.request("PUT", "/api/admin/client-keys/"+clientID+"/permissions", map[string]any{"defaults": []any{}, "permissions": []any{map[string]any{"kind": "real", "model_id": modelID, "enabled": true}}})
	if status != 204 {
		t.Fatalf("permissions: %d %v", status, payload)
	}
	return api, db, clientID, clientSecret
}

// TestHostedClientBodyLoggingIsOptIn pins the hosted posture: detailed error
// logging is available to a hosted account, but only when the account or the
// client key explicitly opts in. Both switches default to off, so a hosted
// deployment that ignores them still persists no body content.
func TestHostedClientBodyLoggingIsOptIn(t *testing.T) {
	t.Parallel()
	api, db, clientID, secret := hostedBodyLogHarness(t)

	// Default: account setting off, client inherits — no bodies are stored.
	status, payload, _ := api.request("GET", "/api/admin/settings", nil)
	if status != http.StatusOK {
		t.Fatalf("settings: %d %v", status, payload)
	}
	if payload["log_error_bodies"] != false {
		t.Fatalf("hosted detailed error logging must default to off, got %v", payload["log_error_bodies"])
	}
	failRequest(t, api, secret)
	_, requestBody, errorBody := loggedBodies(t, db, clientID)
	if requestBody != nil || errorBody != nil {
		t.Fatalf("bodies persisted without an opt-in: request=%v error=%v", requestBody, errorBody)
	}

	// Account-level opt-in succeeds in hosted mode and captures both bodies.
	status, payload, _ = api.request("PUT", "/api/admin/settings", map[string]any{"log_error_bodies": true})
	if status != http.StatusNoContent {
		t.Fatalf("enable hosted detailed error logging: %d %v", status, payload)
	}
	status, payload, _ = api.request("GET", "/api/admin/settings", nil)
	if status != http.StatusOK || payload["log_error_bodies"] != true {
		t.Fatalf("hosted setting did not round-trip: %d %v", status, payload)
	}
	failRequest(t, api, secret)
	_, requestBody, errorBody = loggedBodies(t, db, clientID)
	if requestBody == nil || !strings.Contains(*requestBody, "CLIENT-REQUEST-SECRET-MARKER") {
		t.Fatalf("request body was not persisted after opt-in: %v", requestBody)
	}
	if errorBody == nil || !strings.Contains(*errorBody, "PROVIDER-ERROR-SECRET-MARKER") {
		t.Fatalf("provider error body was not persisted after opt-in: %v", errorBody)
	}

	// Per-client modes work in hosted too: "never" overrides the account
	// setting, "always" overrides it being switched back off.
	status, payload, _ = api.request("PATCH", "/api/admin/client-keys/"+clientID, map[string]any{"log_error_bodies": store.BodyLogNever})
	if status != http.StatusNoContent {
		t.Fatalf("never in hosted mode: %d %v", status, payload)
	}
	failRequest(t, api, secret)
	_, requestBody, errorBody = loggedBodies(t, db, clientID)
	if requestBody != nil || errorBody != nil {
		t.Fatalf("bodies persisted for a hosted client set to never: request=%v error=%v", requestBody, errorBody)
	}
	status, payload, _ = api.request("PUT", "/api/admin/settings", map[string]any{"log_error_bodies": false})
	if status != http.StatusNoContent {
		t.Fatalf("disable hosted detailed error logging: %d %v", status, payload)
	}
	status, payload, _ = api.request("PATCH", "/api/admin/client-keys/"+clientID, map[string]any{"log_error_bodies": store.BodyLogAlways})
	if status != http.StatusNoContent {
		t.Fatalf("always in hosted mode: %d %v", status, payload)
	}
	failRequest(t, api, secret)
	_, requestBody, errorBody = loggedBodies(t, db, clientID)
	if requestBody == nil || !strings.Contains(*requestBody, "CLIENT-REQUEST-SECRET-MARKER") {
		t.Fatalf("hosted client set to always did not persist the request body: %v", requestBody)
	}
	if errorBody == nil || !strings.Contains(*errorBody, "PROVIDER-ERROR-SECRET-MARKER") {
		t.Fatalf("hosted client set to always did not persist the provider error body: %v", errorBody)
	}
}
