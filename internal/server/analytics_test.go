package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
)

func TestPlatformAnalyticsSettingsValidationAndOptions(t *testing.T) {
	t.Parallel()
	app, _, _ := hostedServerHarness(t, false)
	papi := hostedPlatformAPI(t, app)

	// Analytics is disabled by default; the public endpoint says so plainly.
	status, payload, _ := papi.request(http.MethodGet, "/api/analytics/options", nil)
	if status != http.StatusOK || payload["enabled"] != false {
		t.Fatalf("default analytics options = %d %v", status, payload)
	}

	// Enabling without choosing a provider is rejected.
	status, payload, _ = papi.request(http.MethodPut, "/api/platform/settings", map[string]any{"analytics_enabled": true})
	if status != http.StatusBadRequest || errorCode(payload) != "invalid_analytics_settings" {
		t.Fatalf("enable without provider = %d %v", status, payload)
	}

	// A non-https script URL is rejected: it must be an absolute TLS origin.
	status, payload, _ = papi.request(http.MethodPut, "/api/platform/settings", map[string]any{
		"analytics_enabled": true, "analytics_provider": "umami",
		"analytics_script_url": "http://analytics.example.com/script.js", "analytics_site_id": "site-1",
	})
	if status != http.StatusBadRequest || errorCode(payload) != "invalid_analytics_settings" {
		t.Fatalf("http script url = %d %v", status, payload)
	}

	// A named provider without a site ID is rejected.
	status, payload, _ = papi.request(http.MethodPut, "/api/platform/settings", map[string]any{
		"analytics_enabled": true, "analytics_provider": "umami",
		"analytics_script_url": "https://analytics.example.com/script.js", "analytics_site_id": "",
	})
	if status != http.StatusBadRequest || errorCode(payload) != "invalid_analytics_settings" {
		t.Fatalf("missing site id = %d %v", status, payload)
	}

	// A valid Umami configuration is accepted and persisted in plaintext (the
	// values are public and served to the browser).
	status, payload, _ = papi.request(http.MethodPut, "/api/platform/settings", map[string]any{
		"analytics_enabled": true, "analytics_provider": "umami",
		"analytics_script_url": "https://analytics.example.com/script.js", "analytics_site_id": "site-1",
	})
	if status != http.StatusNoContent {
		t.Fatalf("valid analytics = %d %v", status, payload)
	}
	settings, err := app.storeHandle().GetPlatformAnalyticsSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !settings.Enabled || settings.Provider != "umami" || settings.ScriptURL != "https://analytics.example.com/script.js" || settings.SiteID != "site-1" {
		t.Fatalf("persisted analytics = %+v", settings)
	}

	// The public endpoint exposes the configuration (it is not secret) and the
	// platform settings view echoes it back.
	status, payload, _ = papi.request(http.MethodGet, "/api/analytics/options", nil)
	if status != http.StatusOK || payload["enabled"] != true || payload["provider"] != "umami" || payload["script_url"] != "https://analytics.example.com/script.js" || payload["site_id"] != "site-1" {
		t.Fatalf("enabled analytics options = %d %v", status, payload)
	}
	status, payload, _ = papi.request(http.MethodGet, "/api/platform/settings", nil)
	analytics, _ := payload["analytics"].(map[string]any)
	if status != http.StatusOK || analytics["enabled"] != true || analytics["provider"] != "umami" {
		t.Fatalf("settings analytics block = %d %v", status, payload)
	}

	// The change is audited.
	status, audit, _ := papi.request(http.MethodGet, "/api/platform/audit", nil)
	if status != http.StatusOK || !auditContains(audit, "platform.analytics_settings_changed") {
		t.Fatalf("analytics audit = %d %v", status, audit)
	}
}

func TestPlatformPreventSearchIndexing(t *testing.T) {
	t.Parallel()
	app, _, _ := hostedServerHarness(t, false)
	papi := hostedPlatformAPI(t, app)

	status, payload, header := papi.request(http.MethodGet, "/api/runtime", nil)
	if status != http.StatusOK || payload == nil {
		t.Fatalf("default runtime response = %d %v", status, payload)
	}
	if got := header.Get("X-Robots-Tag"); got != "" {
		t.Fatalf("default X-Robots-Tag = %q, want unset", got)
	}

	status, payload, _ = papi.request(http.MethodPut, "/api/platform/settings", map[string]any{"prevent_search_indexing": true})
	if status != http.StatusNoContent {
		t.Fatalf("enable search indexing prevention = %d %v", status, payload)
	}
	status, payload, header = papi.request(http.MethodGet, "/api/runtime", nil)
	if status != http.StatusOK || header.Get("X-Robots-Tag") != "noindex, nofollow" {
		t.Fatalf("enabled runtime response = %d, X-Robots-Tag %q", status, header.Get("X-Robots-Tag"))
	}

	enabled, err := app.storeHandle().GetPlatformPreventSearchIndexing(context.Background())
	if err != nil || !enabled {
		t.Fatalf("persisted search indexing setting = %v, %v", enabled, err)
	}
	status, payload, _ = papi.request(http.MethodGet, "/api/platform/settings", nil)
	if status != http.StatusOK || payload["prevent_search_indexing"] != true {
		t.Fatalf("platform setting response = %d %v", status, payload)
	}

	status, payload, _ = papi.request(http.MethodPut, "/api/platform/settings", map[string]any{"prevent_search_indexing": false})
	if status != http.StatusNoContent {
		t.Fatalf("disable search indexing prevention = %d %v", status, payload)
	}
	_, _, header = papi.request(http.MethodGet, "/api/runtime", nil)
	if got := header.Get("X-Robots-Tag"); got != "" {
		t.Fatalf("disabled X-Robots-Tag = %q, want unset", got)
	}
}

// TestAnalyticsOriginNeverWidensConsoleCSP proves the analytics origin never
// appears in the application document's policy, even while analytics is
// enabled. The vendor script runs in the /analytics-frame.html sandbox, so the
// console keeps connect-src 'self' and a compromised vendor script cannot
// reach the API with the session cookie.
// rawGet performs a GET through the harness client and returns the live
// response, so a test can assert on headers the JSON helper discards.
func rawGet(t *testing.T, a *testAPI, path string) *http.Response {
	t.Helper()
	resp, err := a.client.Get(a.base + path)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAnalyticsOriginNeverWidensConsoleCSP(t *testing.T) {
	t.Parallel()
	app, _, _ := hostedServerHarness(t, false)
	papi := hostedPlatformAPI(t, app)

	_, _, header := papi.request(http.MethodGet, "/api/runtime", nil)
	if csp := header.Get("Content-Security-Policy"); strings.Contains(csp, "analytics.example.com") {
		t.Fatalf("analytics origin present before enabling: %q", csp)
	}

	status, payload, _ := papi.request(http.MethodPut, "/api/platform/settings", map[string]any{
		"analytics_enabled": true, "analytics_provider": "umami",
		"analytics_script_url": "https://analytics.example.com/script.js", "analytics_site_id": "site-1",
	})
	if status != http.StatusNoContent {
		t.Fatalf("enable analytics = %d %v", status, payload)
	}

	// The console document must stay locked down with analytics enabled.
	_, _, header = papi.request(http.MethodGet, "/api/runtime", nil)
	csp := header.Get("Content-Security-Policy")
	if strings.Contains(csp, "analytics.example.com") {
		t.Fatalf("console CSP widened with analytics origin: %q", csp)
	}
	if !strings.Contains(csp, "connect-src 'self'") {
		t.Fatalf("console connect-src must stay 'self': %q", csp)
	}
	if !strings.Contains(csp, "script-src 'self' https://challenges.cloudflare.com") {
		t.Fatalf("console script-src unexpectedly changed: %q", csp)
	}
	if !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "base-uri 'none'") || !strings.Contains(csp, "form-action 'self'") {
		t.Fatalf("console hardening directives: %q", csp)
	}
}

// TestAnalyticsFrameIsIsolated proves the analytics document gets its own
// policy naming only the vendor origin, and stays frameable by the console.
func TestAnalyticsFrameIsIsolated(t *testing.T) {
	t.Parallel()
	app, _, _ := hostedServerHarness(t, false)
	papi := hostedPlatformAPI(t, app)

	status, payload, _ := papi.request(http.MethodPut, "/api/platform/settings", map[string]any{
		"analytics_enabled": true, "analytics_provider": "umami",
		"analytics_script_url": "https://analytics.example.com/script.js", "analytics_site_id": "site-1",
	})
	if status != http.StatusNoContent {
		t.Fatalf("enable analytics = %d %v", status, payload)
	}

	resp := rawGet(t, papi, analyticsFramePath)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("analytics frame status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("analytics frame content type = %q", got)
	}
	if got := resp.Header.Get("X-Frame-Options"); got != "SAMEORIGIN" {
		t.Fatalf("analytics frame X-Frame-Options = %q, want SAMEORIGIN", got)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self' https://analytics.example.com") {
		t.Fatalf("frame script-src missing analytics origin: %q", csp)
	}
	if !strings.Contains(csp, "connect-src https://analytics.example.com") {
		t.Fatalf("frame connect-src missing analytics origin: %q", csp)
	}
	// The frame may only reach the analytics origin, never 'self'.
	if strings.Contains(csp, "connect-src 'self'") {
		t.Fatalf("frame connect-src must not allow 'self': %q", csp)
	}
	if !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("frame default-src must be 'none': %q", csp)
	}
}

// TestAnalyticsFrameDeniesOriginsWithoutAnalytics proves the frame is inert
// (no script and no connect origin) when analytics is disabled.
func TestAnalyticsFrameDeniesOriginsWithoutAnalytics(t *testing.T) {
	t.Parallel()
	app, _, _ := hostedServerHarness(t, false)
	papi := hostedPlatformAPI(t, app)

	resp := rawGet(t, papi, analyticsFramePath)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("analytics frame status = %d, want 200", resp.StatusCode)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if strings.Contains(csp, "https://") {
		t.Fatalf("disabled analytics frame grants a remote origin: %q", csp)
	}
	if !strings.Contains(csp, "connect-src 'none'") {
		t.Fatalf("disabled analytics frame connect-src = %q", csp)
	}
}

// TestAnalyticsFrameHostedOnly proves the frame route is not registered in
// local mode.
func TestAnalyticsFrameHostedOnly(t *testing.T) {
	t.Parallel()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	app := newTestServer(t, config.Config{TillerUser: "admin", TillerUserPassword: "correct horse", DataDir: t.TempDir(), ListenAddr: ":8080"}, db)
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)

	resp, err := http.Get(router.URL + analyticsFramePath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// The embedded asset fallback may still serve the file, but the route that
	// stamps the frame CSP is hosted-only, so a local response must not carry
	// the widened policy.
	if csp := resp.Header.Get("Content-Security-Policy"); strings.Contains(csp, "connect-src 'none'") {
		t.Fatalf("local mode served the hosted analytics frame policy: %q", csp)
	}
}

// TestAnalyticsOptionsHostedOnly proves the public analytics endpoint is not
// registered outside hosted mode.
func TestAnalyticsOptionsHostedOnly(t *testing.T) {
	t.Parallel()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	app := newTestServer(t, config.Config{TillerUser: "admin", TillerUserPassword: "correct horse", DataDir: t.TempDir(), ListenAddr: ":8080"}, db)
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)

	resp, err := http.Get(router.URL + "/api/analytics/options")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("local analytics options status = %d, want 404", resp.StatusCode)
	}
}
