package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/database"
)

// TestManagementGuideServedLocally proves the public guide is reachable at the
// API base paths in local mode, in HTML for humans and markdown for agents,
// without any credential.
func TestManagementGuideServedLocally(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	app := newTenantServer(t, db)
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)

	for _, path := range []string{"/api", "/api/admin"} {
		resp, err := http.Get(router.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s = %d", path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("GET %s content-type = %q", path, ct)
		}
		page := string(body)
		for _, want := range []string{"Management API", "Authorization: Bearer mt-tr-", "/api/admin/providers", "insufficient_scope", "api-tokens/self"} {
			if !strings.Contains(page, want) {
				t.Fatalf("GET %s guide missing %q", path, want)
			}
		}
	}

	// An agent requesting markdown gets the plain-text guide.
	req, err := http.NewRequest("GET", router.URL+"/api/admin", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/markdown")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("markdown guide = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Fatalf("markdown content-type = %q", ct)
	}
	md := string(body)
	for _, want := range []string{"# Tiller Router — Management API", "Authorization: Bearer mt-tr-", "GET /api/admin/api-tokens/self", "/api/admin/virtual-models"} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown guide missing %q", want)
		}
	}
}

// TestManagementGuideAbsentInHosted proves the local-only feature leaves no
// guide (or token surface) in hosted mode.
func TestManagementGuideAbsentInHosted(t *testing.T) {
	_, api, _ := hostedServerHarness(t, false)
	for _, path := range []string{"/api", "/api/admin"} {
		status, _, _ := api.request("GET", path, nil)
		if status != 404 {
			t.Fatalf("hosted GET %s = %d, want 404", path, status)
		}
	}
}
