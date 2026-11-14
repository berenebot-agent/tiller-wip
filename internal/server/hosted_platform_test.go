package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/tiller-router/tiller-router/internal/store"
)

// TestPlatformLoginProbeReportsChallengeSettings proves the unauthenticated
// console probe always refuses access, and tells the login card whether this
// deployment challenges console logins. Without it the client-side gate on
// platform_login could never mount (the console has no /api/auth/options).
func TestPlatformLoginProbeReportsChallengeSettings(t *testing.T) {
	t.Parallel()
	app, api, _ := hostedAccountServer(t)
	anon := &testAPI{t: t, base: api.base, client: api.client, server: app}

	status, payload, _ := anon.request("GET", "/api/platform/session/probe", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("probe without Turnstile = %d %v, want 401", status, payload)
	}
	if payload["turnstile_enabled"] != false || payload["turnstile_site_key"] != "" {
		t.Fatalf("probe without Turnstile = %v, want disabled and no site key", payload)
	}

	err := app.storeHandle().SavePlatformSettings(context.Background(), store.PlatformSettingsProposal{
		AuditRetentionDays: 30,
		Auth:               store.PlatformAuthSettings{TurnstileEnabled: true, TurnstileSiteKey: "test-site", TurnstileSecret: "test-secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, payload, _ = anon.request("GET", "/api/platform/session/probe", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("probe with Turnstile = %d %v, want 401", status, payload)
	}
	if payload["turnstile_enabled"] != true || payload["turnstile_site_key"] != "test-site" {
		t.Fatalf("probe with Turnstile = %v, want enabled with the public site key", payload)
	}
	// The probe must never leak the secret half of the configuration.
	if _, leaked := payload["turnstile_secret"]; leaked {
		t.Fatal("probe exposed the Turnstile secret")
	}
}
