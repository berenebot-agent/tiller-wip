package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/auth"
	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/testutil/fastsecret"
)

const otherAccountID = "11111111-1111-1111-1111-111111111111"

// newTenantServer builds a Server on the given DB, optionally injecting a
// non-local admin account resolver.
func newTenantServer(t *testing.T, db *database.DB, opts ...serverOption) *Server {
	t.Helper()
	all := append([]serverOption{withSecretHasher(fastsecret.Hasher{})}, opts...)
	app, err := New(config.Config{TillerUser: "admin", TillerUserPassword: "correct horse", DataDir: t.TempDir()}, db, slog.New(slog.NewTextHandler(io.Discard, nil)), all...)
	if err != nil {
		t.Fatal(err)
	}
	app.liveHub.timings = testLiveTimings
	app.usageCacheTTL = 0
	return app
}

func loginTestAPI(t *testing.T, app *Server) *testAPI {
	t.Helper()
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	api := &testAPI{t: t, base: router.URL, client: &http.Client{Jar: jar}, server: app}
	status, payload, _ := api.request("POST", "/api/admin/session", map[string]any{"username": "admin", "password": "correct horse"})
	if status != 200 {
		t.Fatalf("login: %d %v", status, payload)
	}
	api.csrf = payload["csrf_token"].(string)
	return api
}

// TestCrossAccountHTTPIsolation proves account B cannot read, patch, delete or
// enumerate account A's admin resources, and that a client key bound to A
// cannot resolve B's models.
func TestCrossAccountHTTPIsolation(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.SQL.Exec(`INSERT INTO accounts(id,plan,status,created_at,updated_at) VALUES(?,'free','active','2026-09-19T00:00:00Z','2026-09-19T00:00:00Z')`, otherAccountID); err != nil {
		t.Fatal(err)
	}
	appA := newTenantServer(t, db)
	appB := newTenantServer(t, db, withAdminAccount(func(auth.Session) string { return otherAccountID }))
	apiA := loginTestAPI(t, appA)
	apiB := loginTestAPI(t, appB)

	// --- A creates resources ---
	status, payload, _ := apiA.request("POST", "/api/admin/providers", map[string]any{"name": "prov-a", "type": "generic-openai", "base_url": "http://127.0.0.1:1/v1", "credential": "key-a"})
	if status != 201 {
		t.Fatalf("create provider A: %d %v", status, payload)
	}
	providerA := payload["id"].(string)
	status, payload, _ = apiA.request("POST", "/api/admin/virtual-groups", map[string]any{"name": "group-a"})
	if status != 201 {
		t.Fatalf("create virtual group A: %d %v", status, payload)
	}
	groupA := payload["id"].(string)
	status, payload, _ = apiA.request("POST", "/api/admin/client-keys", map[string]any{"name": "key-a"})
	if status != 201 {
		t.Fatalf("create client key A: %d %v", status, payload)
	}
	keyA := payload["id"].(string)
	secretA := payload["secret"].(string)

	// A can see its own resources.
	status, payload, _ = apiA.request("GET", "/api/admin/providers", nil)
	if status != 200 || !listContains(payload, "id", providerA) {
		t.Fatalf("A cannot see its provider: %d %v", status, payload)
	}
	status, payload, _ = apiA.request("GET", "/api/admin/client-keys", nil)
	if status != 200 || !listContains(payload, "id", keyA) {
		t.Fatalf("A cannot see its client key: %d %v", status, payload)
	}

	// --- B cannot enumerate A's resources ---
	for _, path := range []string{"/api/admin/providers", "/api/admin/client-keys", "/api/admin/virtual-groups", "/api/admin/virtual-models", "/api/admin/models", "/api/admin/activity"} {
		status, payload, _ = apiB.request("GET", path, nil)
		if status != 200 {
			t.Fatalf("B GET %s: %d %v", path, status, payload)
		}
		if data, ok := payload["data"].([]any); ok && len(data) != 0 {
			t.Fatalf("B saw %d rows at %s: %v", len(data), path, data)
		}
	}

	// --- B cannot read/patch/delete A's objects ---
	mutations := []struct {
		method, path string
		body         any
	}{
		{"PATCH", "/api/admin/providers/" + providerA, map[string]any{"base_url": "http://127.0.0.1:1/v1"}},
		{"DELETE", "/api/admin/providers/" + providerA, nil},
		{"PATCH", "/api/admin/virtual-groups/" + groupA, map[string]any{"name": "hijacked"}},
		{"DELETE", "/api/admin/virtual-groups/" + groupA, nil},
		{"PATCH", "/api/admin/client-keys/" + keyA, map[string]any{"description": "hijacked"}},
		{"DELETE", "/api/admin/client-keys/" + keyA, nil},
		{"POST", "/api/admin/client-keys/" + keyA + "/rotate", nil},
		{"GET", "/api/admin/client-keys/" + keyA + "/permissions", nil},
		{"PUT", "/api/admin/client-keys/" + keyA + "/permissions", map[string]any{}},
		{"GET", "/api/admin/client-keys/" + keyA + "/activity", nil},
		{"DELETE", "/api/admin/client-keys/" + keyA + "/activity", nil},
		{"GET", "/api/admin/client-keys/" + keyA + "/activity/export", nil},
	}
	for _, m := range mutations {
		status, payload, _ = apiB.request(m.method, m.path, m.body)
		if status != 404 {
			t.Fatalf("B %s %s = %d, want 404 (%v)", m.method, m.path, status, payload)
		}
	}

	// --- B creates its own provider + manual model ---
	status, payload, _ = apiB.request("POST", "/api/admin/providers", map[string]any{"name": "prov-b", "type": "generic-openai", "base_url": "http://127.0.0.1:1/v1", "credential": "key-b"})
	if status != 201 {
		t.Fatalf("create provider B: %d %v", status, payload)
	}
	providerB := payload["id"].(string)
	status, payload, _ = apiB.request("POST", "/api/admin/providers/"+providerB+"/models", map[string]any{"upstream_model_id": "model-b"})
	if status != 201 {
		t.Fatalf("create manual model B: %d %v", status, payload)
	}
	modelB := payload["id"].(string)

	// A's client key cannot resolve B's model.
	resp, body := clientCall(t, apiA.base, secretA, "/v1/chat/completions", map[string]any{"model": "prov-b/model-b", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if resp.StatusCode != 404 {
		t.Fatalf("A key resolved B model: status=%d body=%v", resp.StatusCode, body)
	}

	// B's own client key can see B's model (control).
	status, payload, _ = apiB.request("POST", "/api/admin/client-keys", map[string]any{"name": "key-b"})
	if status != 201 {
		t.Fatalf("create client key B: %d %v", status, payload)
	}
	keyB := payload["id"].(string)
	secretB := payload["secret"].(string)
	status, payload, _ = apiB.request("PUT", "/api/admin/client-keys/"+keyB+"/permissions", map[string]any{
		"permissions": []any{map[string]any{"kind": "real", "model_id": modelB, "enabled": true}},
	})
	if status != 204 {
		t.Fatalf("enable B model permission: %d %v", status, payload)
	}
	models := clientModelsList(t, apiB.base, secretB)
	if !strings.Contains(models, "prov-b/model-b") {
		t.Fatalf("B key catalogue missing its own model: %s", models)
	}
}

// clientModelsList fetches the client-visible catalogue for a key as raw JSON.
func clientModelsList(t *testing.T, base, secret string) string {
	t.Helper()
	req, err := http.NewRequest("GET", base+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("models list: %d %s", resp.StatusCode, raw)
	}
	return string(raw)
}

func listContains(payload map[string]any, field, value string) bool {
	data, ok := payload["data"].([]any)
	if !ok {
		return false
	}
	for _, raw := range data {
		row, ok := raw.(map[string]any)
		if ok && row[field] == value {
			return true
		}
	}
	return false
}
