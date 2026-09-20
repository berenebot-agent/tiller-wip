package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/legal"
	"github.com/tiller-router/tiller-router/internal/testutil/fastsecret"
)

// legalTestServer builds a hosted Server with seeded embedded legal drafts and
// returns the app plus a platform-authenticated API and an unauthenticated
// client.
func legalTestServer(t *testing.T) (*Server, *testAPI, *http.Client, string) {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	app, err := New(config.Config{
		Mode: config.ModeHosted, AdminUsername: "owner@example.com", AdminPassword: "correct horse battery staple",
		PlatformUsername: "platform-admin", PlatformPassword: "platform-secret",
		PublicURL: "https://tiller.example.com", DataDir: t.TempDir(),
	}, db, slog.New(slog.NewTextHandler(io.Discard, nil)), withSecretHasher(fastsecret.Hasher{}))
	if err != nil {
		t.Fatal(err)
	}
	app.liveHub.timings = testLiveTimings
	app.usageCacheTTL = 0
	if err := app.SeedLegalDocuments(ctx); err != nil {
		t.Fatal(err)
	}

	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	api := &testAPI{t: t, base: router.URL, client: &http.Client{Jar: jar}, server: app}
	status, payload, _ := api.request("POST", "/api/platform/session", map[string]any{"username": "platform-admin", "password": "platform-secret"})
	if status != 200 {
		t.Fatalf("platform login: %d %v", status, payload)
	}
	api.csrf = payload["csrf_token"].(string)
	return app, api, &http.Client{}, router.URL
}

func TestLegalDocPublicGetReturnsSeededDraft(t *testing.T) {
	_, _, client, base := legalTestServer(t)

	for _, slug := range []string{"terms", "privacy", "aup", "subprocessors", "security"} {
		resp, err := client.Get(base + "/api/legal/" + slug)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		decodeResponse(t, resp, &payload)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/legal/%s = %d %v", slug, resp.StatusCode, payload)
		}
		if payload["slug"] != slug || payload["title"] == "" || payload["updated_at"] == "" {
			t.Fatalf("legal doc payload incomplete for %s: %v", slug, payload)
		}
		body, _ := payload["body"].(string)
		if body == "" {
			t.Fatalf("legal doc %s has empty body", slug)
		}
		if _, leaked := payload["updated_by"]; leaked {
			t.Fatalf("public legal doc leaked updated_by: %v", payload)
		}
	}
}

func TestLegalDocUnknownSlugReturns404(t *testing.T) {
	_, _, client, base := legalTestServer(t)
	resp, err := client.Get(base + "/api/legal/does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	decodeResponse(t, resp, &payload)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown slug status = %d, want 404: %v", resp.StatusCode, payload)
	}
	if code := errorCode(payload); code != "legal_not_found" {
		t.Fatalf("unknown slug code = %q, want legal_not_found", code)
	}
}

func TestPlatformLegalListAndPublish(t *testing.T) {
	app, api, _, _ := legalTestServer(t)

	status, list, _ := api.request("GET", "/api/platform/legal", nil)
	if status != 200 {
		t.Fatalf("list legal: %d %v", status, list)
	}
	data, _ := list["data"].([]any)
	if len(data) != len(legal.Documents()) {
		t.Fatalf("platform legal list = %d docs, want %d", len(data), len(legal.Documents()))
	}

	status, payload, _ := api.request("PUT", "/api/platform/legal/terms", map[string]any{"title": "Terms of Service", "body": "Operator-approved terms body."})
	if status != http.StatusNoContent {
		t.Fatalf("publish terms: %d %v", status, payload)
	}
	doc, err := app.storeHandle().GetLegalDoc(context.Background(), "terms")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Body != "Operator-approved terms body." || doc.UpdatedBy != "platform" {
		t.Fatalf("publish did not persist: %+v", doc)
	}

	// The publish is recorded in the platform audit log with the slug.
	var audit int
	if err := app.db.SQL.QueryRow(`SELECT count(*) FROM platform_audit_events WHERE event='platform.legal_document_published' AND target_id='terms'`).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if audit != 1 {
		t.Fatalf("platform.legal_document_published audit rows = %d, want 1", audit)
	}

	// Empty title or body is rejected.
	status, _, _ = api.request("PUT", "/api/platform/legal/terms", map[string]any{"title": "", "body": "x"})
	if status != http.StatusBadRequest {
		t.Fatalf("empty title status = %d, want 400", status)
	}
	status, _, _ = api.request("PUT", "/api/platform/legal/terms", map[string]any{"title": "x", "body": "   "})
	if status != http.StatusBadRequest {
		t.Fatalf("blank body status = %d, want 400", status)
	}
	// An unknown slug is rejected, not upserted.
	status, _, _ = api.request("PUT", "/api/platform/legal/made-up", map[string]any{"title": "x", "body": "y"})
	if status != http.StatusNotFound {
		t.Fatalf("unknown publish slug status = %d, want 404", status)
	}
	// The signup notice is not a platform-editable document.
	status, _, _ = api.request("PUT", "/api/platform/legal/signup-notice", map[string]any{"title": "x", "body": "y"})
	if status != http.StatusNotFound {
		t.Fatalf("publish to signup-notice status = %d, want 404", status)
	}
}

func TestSeedLegalDocumentsDoesNotOverwriteOperatorEdit(t *testing.T) {
	app, api, _, base := legalTestServer(t)

	status, _, _ := api.request("PUT", "/api/platform/legal/privacy", map[string]any{"title": "Privacy Policy", "body": "Operator-edited privacy."})
	if status != http.StatusNoContent {
		t.Fatalf("publish privacy: %d", status)
	}
	if err := app.SeedLegalDocuments(context.Background()); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(base + "/api/legal/privacy")
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	decodeResponse(t, resp, &payload)
	if payload["body"] != "Operator-edited privacy." {
		t.Fatalf("startup seed overwrote operator edit: %v", payload["body"])
	}
}

func decodeResponse(t *testing.T, resp *http.Response, target any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}
