package store_test

import (
	"context"
	"testing"

	"github.com/tiller-router/tiller-router/internal/store"
)

// TestPlatformAnalyticsSettingsRoundTrip proves the operator analytics config
// persists and reads back. Unlike provider/mail secrets these values are public
// (they are served to every hosted page), so they must be stored in plaintext
// and never treated as recoverable secrets.
func TestPlatformAnalyticsSettingsRoundTrip(t *testing.T) {
	db, st := openStore(t)
	ctx := context.Background()

	got, err := st.GetPlatformAnalyticsSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled || got.Provider != "" || got.ScriptURL != "" || got.SiteID != "" {
		t.Fatalf("analytics defaults = %+v, want disabled/empty", got)
	}

	proposal := store.PlatformSettingsProposal{
		HostedSignupEnabled: true,
		AuditRetentionDays:  30,
		Analytics: store.PlatformAnalyticsSettings{
			Enabled:   true,
			Provider:  "umami",
			ScriptURL: "https://analytics.example.com/script.js",
			SiteID:    "site-1",
		},
	}
	if err := st.SavePlatformSettings(ctx, proposal); err != nil {
		t.Fatal(err)
	}

	got, err = st.GetPlatformAnalyticsSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.Provider != "umami" || got.ScriptURL != "https://analytics.example.com/script.js" || got.SiteID != "site-1" {
		t.Fatalf("analytics round-trip = %+v", got)
	}

	var raw string
	if err := db.SQL.QueryRow(`SELECT value FROM platform_settings WHERE key=?`, store.PlatformSettingAnalyticsScriptURL).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != "https://analytics.example.com/script.js" {
		t.Fatalf("script URL stored as %q, want plaintext", raw)
	}
}
