package server

import (
	"context"
	"encoding/json"
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
	t.Parallel()
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
		for _, want := range []string{"Management API", "Authorization: Bearer mt-tr-", "/api/admin/providers", "insufficient_scope", "api-tokens/self", "client_key_names"} {
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

// TestManagementGuideServedInHosted proves hosted serves the guide with the
// hosted limits and without delete-tier endpoints.
func TestManagementGuideServedInHosted(t *testing.T) {
	t.Parallel()
	_, api, _ := hostedServerHarness(t, false)
	req, _ := http.NewRequest("GET", api.base+"/api/admin", nil)
	req.Header.Set("Accept", "text/markdown")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	md := string(body)
	if resp.StatusCode != 200 {
		t.Fatalf("hosted guide = %d", resp.StatusCode)
	}
	for _, want := range []string{"Hosted accounts", "Paging and limits", "must be public", "60 requests per minute", "rate_limited", "/api/admin/catalogue-templates", "not available to hosted tokens"} {
		if !strings.Contains(md, want) {
			t.Fatalf("hosted guide missing %q", want)
		}
	}
	if strings.Contains(md, "| DELETE | `/api/admin/providers/{id}`") {
		t.Fatal("hosted guide lists a delete-tier route")
	}
	for _, path := range []string{"/api", "/api.json", "/api/openapi.json"} {
		if status, _, _ := api.request("GET", path, nil); status != 200 {
			t.Fatalf("hosted GET %s = %d, want 200", path, status)
		}
	}
	status, doc, _ := api.request("GET", "/api.json", nil)
	limits, _ := doc["limits"].(map[string]any)
	if status != 200 || doc["mode"] != "hosted" || doc["token_management"] != "dashboard-only" || limits["requests_per_minute_per_account"] != float64(hostedManagementRequestsPerMinute) {
		t.Fatalf("hosted discovery = %v", doc)
	}
}

// TestManagementReferenceMatchesRegisteredRoutes proves the generated guide
// and OpenAPI file cover every token-reachable route (including catalogue
// templates) and enforce what they advertise.
func TestManagementReferenceMatchesRegisteredRoutes(t *testing.T) {
	t.Parallel()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	app := newTenantServer(t, db)
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)

	if len(app.managementRoutes) < 50 {
		t.Fatalf("only %d management routes recorded", len(app.managementRoutes))
	}
	resp, err := http.Get(router.URL + "/api/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		OpenAPI string                               `json:"openapi"`
		Paths   map[string]map[string]map[string]any `json:"paths"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if doc.OpenAPI != "3.1.0" {
		t.Fatalf("openapi version = %q", doc.OpenAPI)
	}
	md := renderManagementGuideMarkdown(app.managementGuideInfo())
	for _, r := range app.managementRoutes {
		op, ok := doc.Paths[r.Path][strings.ToLower(r.Method)]
		if !ok {
			t.Fatalf("openapi missing %s %s", r.Method, r.Path)
		}
		if want := r.Area + ":" + managementTierLabel(r.Tier); op["x-required-scope"] != want {
			t.Fatalf("openapi %s %s scope = %v, want %s", r.Method, r.Path, op["x-required-scope"], want)
		}
		if !strings.Contains(md, "| "+r.Method+" | `"+r.Path+"` | "+managementTierLabel(r.Tier)+" |") {
			t.Fatalf("guide missing %s %s", r.Method, r.Path)
		}
	}
	// Every advertised route is genuinely token-reachable and scope-gated: a
	// token with no grants is refused with the advertised requirement.
	api := loginTestAPI(t, app)
	_, empty := createManagementToken(t, api, "empty", map[string]int{}, "")
	for _, r := range app.managementRoutes {
		path := strings.ReplaceAll(strings.ReplaceAll(r.Path, "{id}", "x"), "{model...}", "x")
		status, payload, _ := bearerRequest(t, router.URL, empty, r.Method, path, nil)
		required := ""
		if e, ok := payload["error"].(map[string]any); ok {
			required, _ = e["required"].(string)
		}
		if status != 403 || required != r.Area+":"+managementTierLabel(r.Tier) {
			t.Fatalf("%s %s with empty token = %d required=%q, want 403 %s:%s", r.Method, r.Path, status, required, r.Area, managementTierLabel(r.Tier))
		}
	}
	if _, ok := doc.Paths["/api/admin/catalogue-templates/{id}/permissions"]["put"]; !ok {
		t.Fatal("catalogue-template routes missing from the reference")
	}
}

func TestManagementDiscoveryDocument(t *testing.T) {
	t.Parallel()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	app := newTenantServer(t, db)
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)

	// Explicit /api.json always returns JSON.
	resp, err := http.Get(router.URL + "/api.json")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api.json = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q", ct)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode discovery: %v", err)
	}
	if doc["api_base"] != "/api/admin" {
		t.Fatalf("api_base = %v", doc["api_base"])
	}
	authDoc := doc["auth"].(map[string]any)
	if authDoc["scheme"] != "bearer" || authDoc["key_prefix"] != "mt-tr-" {
		t.Fatalf("auth = %v", authDoc)
	}
	if _, ok := doc["capabilities"].([]any); !ok {
		t.Fatalf("capabilities missing or wrong type: %v", doc["capabilities"])
	}
	limits, ok := doc["limits"].(map[string]any)
	if !ok {
		t.Fatalf("limits missing: %v", doc["limits"])
	}
	if limits["page_size_max"] != float64(200) || limits["offset_max"] != float64(10000) || limits["admin_request_body_bytes"] != float64(32<<20) {
		t.Fatalf("limits = %v", limits)
	}

	// Accept: application/json negotiates the same document at /api.
	req, _ := http.NewRequest("GET", router.URL+"/api", nil)
	req.Header.Set("Accept", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("negotiated content-type = %q", resp.Header.Get("Content-Type"))
	}
	var neg map[string]any
	if err := json.Unmarshal(body, &neg); err != nil || neg["name"] != "Tiller Router" {
		t.Fatalf("negotiated discovery = %v err=%v", neg, err)
	}
}
