package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/store"
	"github.com/tiller-router/tiller-router/internal/testutil/fastsecret"
)

// hostedServerHarness builds a hosted server with one verified customer and an
// authenticated test API. When dropActivity is true the Activity database
// handle is removed before the server is constructed, simulating activity.db
// failing to open.
func hostedServerHarness(t *testing.T, dropActivity bool) (*Server, *testAPI, string) {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	if dropActivity {
		activity := db.Activity
		db.Activity = nil
		t.Cleanup(func() {
			if activity != nil {
				_ = activity.Close()
			}
			db.Close()
		})
	} else {
		t.Cleanup(func() { db.Close() })
	}
	app, err := New(config.Config{
		Mode: config.ModeHosted, TillerUser: "owner@example.com", TillerUserPassword: "correct horse battery staple",
		TillerPlatformAdminUser: "platform-admin", TillerPlatformAdminPassword: "platform-secret",
		PublicURL: "https://tiller.example.com", DataDir: t.TempDir(),
	}, db, slog.New(slog.NewTextHandler(io.Discard, nil)), withSecretHasher(fastsecret.Hasher{}))
	if err != nil {
		t.Fatal(err)
	}
	app.liveHub.timings = testLiveTimings
	app.usageCacheTTL = 0
	if err := app.storeHandle().SetHostedSignupEnabled(context.Background(), true); err != nil {
		t.Fatal(err)
	}

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
	return app, api, payload["account_id"].(string)
}

// insertAccountActivityRow writes one request_logs row for the account with the
// given status so onboarding/export tests control Activity deterministically.
func insertAccountActivityRow(t *testing.T, app *Server, accountID, id string, status int) {
	t.Helper()
	clientRequestID := "req-" + id
	_, err := app.db.Activity.Exec(`INSERT INTO request_logs(id,account_id,client_key_id,client_name,requested_model,resolved_provider,resolved_model,protocol,streaming,http_status,latency_ms,client_request_id,request_body,error_body,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, accountID, "ck-"+id, "client", "provider-a/model-a", "provider-a", "model-a", "chat", 0, status, 5, clientRequestID, "PROMPT-SECRET-MARKER", "ERROR-BODY-MARKER", "2026-01-01T00:00:01Z")
	if err != nil {
		t.Fatal(err)
	}
}

func TestOnboardingStateBeforeAfterActivityAndDismissal(t *testing.T) {
	app, api, accountID := hostedServerHarness(t, false)

	status, payload, _ := api.request("GET", "/api/auth/onboarding", nil)
	if status != 200 {
		t.Fatalf("onboarding state: %d %v", status, payload)
	}
	if payload["needs_onboarding"] != true || payload["dismissed"] != false || payload["first_request_at"] != "" {
		t.Fatalf("fresh account onboarding = %v, want needs_onboarding true", payload)
	}

	// A non-2xx row must not satisfy onboarding.
	insertAccountActivityRow(t, app, accountID, "row-failed", 500)
	status, payload, _ = api.request("GET", "/api/auth/onboarding", nil)
	if status != 200 || payload["needs_onboarding"] != true {
		t.Fatalf("failed request satisfied onboarding: %d %v", status, payload)
	}

	// A 2xx row completes onboarding on its own.
	insertAccountActivityRow(t, app, accountID, "row-ok", 200)
	status, payload, _ = api.request("GET", "/api/auth/onboarding", nil)
	if status != 200 || payload["needs_onboarding"] != false || payload["first_request_at"] == "" {
		t.Fatalf("successful request did not complete onboarding: %d %v", status, payload)
	}

	// Dismissal persists and survives a reload.
	status, _, _ = api.request("POST", "/api/auth/onboarding/dismiss", nil)
	if status != http.StatusNoContent {
		t.Fatalf("dismiss status = %d, want 204", status)
	}
	status, payload, _ = api.request("GET", "/api/auth/onboarding", nil)
	if status != 200 || payload["dismissed"] != true || payload["needs_onboarding"] != false {
		t.Fatalf("dismissal not persisted: %d %v", status, payload)
	}
}

func TestOnboardingDismissedBeforeAnyRequest(t *testing.T) {
	_, api, _ := hostedServerHarness(t, false)
	status, _, _ := api.request("POST", "/api/auth/onboarding/dismiss", nil)
	if status != http.StatusNoContent {
		t.Fatalf("dismiss status = %d, want 204", status)
	}
	status, payload, _ := api.request("GET", "/api/auth/onboarding", nil)
	if status != 200 || payload["needs_onboarding"] != false || payload["dismissed"] != true {
		t.Fatalf("dismissed fresh account = %d %v", status, payload)
	}
}

func TestAccountExportRequiresAuth(t *testing.T) {
	app, _, _ := hostedServerHarness(t, false)
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)
	for _, path := range []string{"/api/auth/account/export", "/api/auth/onboarding"} {
		req, err := http.NewRequest("GET", router.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("GET %s unauthenticated status = %d, want 401", path, resp.StatusCode)
		}
	}
}

// getExport performs an authenticated GET and returns the status, headers and
// raw body for the ZIP export.
func getExport(t *testing.T, api *testAPI) (int, http.Header, []byte) {
	t.Helper()
	resp, err := api.client.Get(api.base + "/api/auth/account/export")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header.Clone(), body
}

func TestAccountExportZipContainsThreeEntriesWithoutSecrets(t *testing.T) {
	app, api, accountID := hostedServerHarness(t, false)
	ctx := context.Background()
	sc := app.storeHandle().For(accountID)

	if err := sc.CreateProvider(ctx, store.CreateProviderInput{ID: "prov-1", Name: "provider-a", Type: "generic-openai", BaseURL: "https://api.example.com/v1", Credential: "provider-credential-marker", Enabled: true, Protocols: "chat"}); err != nil {
		t.Fatal(err)
	}
	if err := sc.CreateClientKey(ctx, store.CreateClientKeyInput{ID: "ck-1", Name: "export client", Description: "d", Group: "default", Selector: "selector-marker", Hash: "hash-marker", Fingerprint: "fingerprint-marker", Type: "catalogue", LoggingEnabled: true, RetentionDays: 30}); err != nil {
		t.Fatal(err)
	}
	if err := sc.SetSetting(ctx, store.SettingNotificationsAuthHeader, "Authorization: Bearer auth-header-marker"); err != nil {
		t.Fatal(err)
	}
	insertAccountActivityRow(t, app, accountID, "row-ok", 200)

	status, header, body := getExport(t, api)
	if status != http.StatusOK {
		t.Fatalf("export status = %d, body=%s", status, body)
	}
	if header.Get("Content-Type") != "application/zip" {
		t.Fatalf("Content-Type = %q, want application/zip", header.Get("Content-Type"))
	}
	if !strings.HasPrefix(header.Get("Content-Disposition"), "attachment") || !strings.Contains(header.Get("Content-Disposition"), ".zip") {
		t.Fatalf("Content-Disposition = %q", header.Get("Content-Disposition"))
	}
	if header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", header.Get("Cache-Control"))
	}

	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("zip open: %v", err)
	}
	entries := map[string][]byte{}
	for _, file := range reader.File {
		rc, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		entries[file.Name] = content
	}
	for _, name := range []string{"config.json", "activity.csv", "audit.csv"} {
		if _, ok := entries[name]; !ok {
			t.Fatalf("export missing %s; entries=%v", name, keys(entries))
		}
	}

	configBody := string(entries["config.json"])
	var configJSON map[string]any
	if err := json.Unmarshal([]byte(configBody), &configJSON); err != nil {
		t.Fatalf("config.json not valid JSON: %v", err)
	}
	if configJSON["profile"].(map[string]any)["account_id"] != accountID {
		t.Fatalf("config.json profile account mismatch: %v", configJSON["profile"])
	}
	if _, ok := configJSON["plan"]; !ok {
		t.Fatal("config.json missing plan/limits")
	}
	for _, forbidden := range []string{
		"provider-credential-marker", "auth-header-marker",
		"selector-marker", "hash-marker", "fingerprint-marker",
		"credential_secret", "secret_hash", "secret_fingerprint", "selector",
	} {
		if strings.Contains(configBody, forbidden) {
			t.Fatalf("config.json leaked %q: %s", forbidden, configBody)
		}
	}
	clientKeys := configJSON["client_keys"].([]any)
	if len(clientKeys) != 1 || clientKeys[0].(map[string]any)["name"] != "export client" {
		t.Fatalf("config.json client key metadata wrong: %v", clientKeys)
	}

	// Activity bodies must never be exported.
	activityBody := string(entries["activity.csv"])
	if strings.Contains(activityBody, "PROMPT-SECRET-MARKER") || strings.Contains(activityBody, "ERROR-BODY-MARKER") {
		t.Fatalf("activity.csv leaked request/error body: %s", activityBody)
	}
	if !strings.Contains(activityBody, "provider-a/model-a") {
		t.Fatalf("activity.csv missing activity metadata: %s", activityBody)
	}
	// The login during harness setup produced account audit history.
	if !strings.Contains(string(entries["audit.csv"]), "user.login") {
		t.Fatalf("audit.csv missing account audit events: %s", entries["audit.csv"])
	}
}

func TestAccountExportRejectsWhenActivityUnavailable(t *testing.T) {
	_, api, _ := hostedServerHarness(t, true)
	status, payload, _ := api.request("GET", "/api/auth/account/export", nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("export with Activity unavailable = %d, want 503 (%v)", status, payload)
	}
	if code := errorCode(payload); code != "activity_unavailable" {
		t.Fatalf("export error code = %q, want activity_unavailable", code)
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// signupAndReadVerificationToken signs up a new customer and returns the raw
// verification token enqueued to the durable outbox. In tests no cipher is
// injected, so the token ciphertext column holds the raw token verbatim.
func signupAndReadVerificationToken(t *testing.T, app *Server, api *testAPI, email string) string {
	t.Helper()
	status, payload, _ := api.request("POST", "/api/auth/signup", map[string]any{"email": email, "password": "correct horse battery staple", "accept_terms": true})
	if status != http.StatusAccepted {
		t.Fatalf("signup: %d %v", status, payload)
	}
	var token string
	if err := app.db.SQL.QueryRow(`SELECT token_ciphertext FROM mail_outbox WHERE type='verify_email' AND recipient=?`, email).Scan(&token); err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("verification mail carried no token")
	}
	return token
}

func TestSignupRequiresTermsAcceptance(t *testing.T) {
	_, api, _ := hostedServerHarness(t, false)
	status, payload, _ := api.request("POST", "/api/auth/signup", map[string]any{"email": "new@example.com", "password": "correct horse battery staple", "accept_terms": false})
	if status != http.StatusBadRequest {
		t.Fatalf("signup without acceptance = %d, want 400 (%v)", status, payload)
	}
	if code := errorCode(payload); code != "terms_not_accepted" {
		t.Fatalf("signup error code = %q, want terms_not_accepted", code)
	}
	var users int
	if err := api.server.db.SQL.QueryRow(`SELECT count(*) FROM users WHERE email=?`, "new@example.com").Scan(&users); err != nil {
		t.Fatal(err)
	}
	if users != 0 {
		t.Fatalf("rejected signup still created %d user rows", users)
	}
}

func TestVerifyEmailMintsSessionAndRecordsAcceptance(t *testing.T) {
	app, api, _ := hostedServerHarness(t, false)
	token := signupAndReadVerificationToken(t, app, api, "new@example.com")

	status, payload, header := api.request("POST", "/api/auth/verify-email", map[string]any{"token": token})
	if status != http.StatusOK {
		t.Fatalf("verify-email: %d %v", status, payload)
	}
	if payload["verified"] != true || payload["authenticated"] != true {
		t.Fatalf("verify-email payload = %v, want verified + authenticated", payload)
	}
	if payload["account_id"] == nil || payload["account_id"] == "" {
		t.Fatalf("verify-email did not return an account_id: %v", payload)
	}
	if !strings.Contains(strings.Join(header.Values("Set-Cookie"), ";"), userSessionCookie) {
		t.Fatalf("verify-email did not set the user session cookie: %v", header.Values("Set-Cookie"))
	}
	// The minted session belongs to the verified user, not the logged-in owner.
	var userID string
	if err := app.db.SQL.QueryRow(`SELECT id FROM users WHERE email=?`, "new@example.com").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	var acceptances int
	if err := app.db.SQL.QueryRow(`SELECT count(*) FROM legal_acceptances WHERE user_id=? AND terms_updated_at IS NOT NULL AND privacy_updated_at IS NOT NULL`, userID).Scan(&acceptances); err != nil {
		t.Fatal(err)
	}
	if acceptances != 1 {
		t.Fatalf("signup acceptance rows = %d, want 1", acceptances)
	}
	// The link is one-time: replaying it must fail.
	status, _, _ = api.request("POST", "/api/auth/verify-email", map[string]any{"token": token})
	if status != http.StatusBadRequest {
		t.Fatalf("replayed verification token status = %d, want 400", status)
	}
}
