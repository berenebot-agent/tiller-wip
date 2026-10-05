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

// TestAnalyticsOriginWidensHostedCSP proves the hosted policy only gains the
// operator's script origin while analytics is enabled, and that it is added to
// the directives a browser needs for script load and event beacons.
func TestAnalyticsOriginWidensHostedCSP(t *testing.T) {
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

	_, _, header = papi.request(http.MethodGet, "/api/runtime", nil)
	csp := header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self' https://analytics.example.com") {
		t.Fatalf("script-src missing analytics origin: %q", csp)
	}
	if !strings.Contains(csp, "connect-src 'self' https://analytics.example.com") {
		t.Fatalf("connect-src missing analytics origin: %q", csp)
	}
	if !strings.Contains(csp, "img-src 'self' data: https://analytics.example.com") {
		t.Fatalf("img-src missing analytics origin: %q", csp)
	}
	if !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "base-uri 'none'") || !strings.Contains(csp, "form-action 'self'") {
		t.Fatalf("analytics disabled hardening: %q", csp)
	}
}

// TestAnalyticsOptionsHostedOnly proves the public analytics endpoint is not
// registered outside hosted mode.
func TestAnalyticsOptionsHostedOnly(t *testing.T) {
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
