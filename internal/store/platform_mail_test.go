package store_test

import (
	"context"
	"testing"

	"github.com/tiller-router/tiller-router/internal/store"
)

// TestPlatformMailSettingsFromNameRoundTrip proves the operator-configured
// sender display name persists and reads back. The display name is public
// presentation data, not a secret, so it is stored in plaintext.
func TestPlatformMailSettingsFromNameRoundTrip(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	ctx := context.Background()

	got, err := st.GetPlatformMailSettings(ctx)
	if err != nil && err.Error() != "sql: no rows in result set" {
		t.Fatalf("defaults: %v", err)
	}
	if got.FromName != "" {
		t.Fatalf("from name default = %q, want empty", got.FromName)
	}

	proposal := store.PlatformSettingsProposal{
		HostedSignupEnabled: true,
		AuditRetentionDays:  30,
		Mail: store.PlatformMailSettings{
			Provider: "brevo",
			From:     "no-reply@example.com",
			FromName: "Tiller",
			SMTPMode: "starttls",
		},
	}
	if err := st.SavePlatformSettings(ctx, proposal); err != nil {
		t.Fatal(err)
	}

	got, err = st.GetPlatformMailSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "brevo" || got.From != "no-reply@example.com" || got.FromName != "Tiller" {
		t.Fatalf("mail round-trip = %+v", got)
	}

	var raw string
	if err := db.SQL.QueryRow(`SELECT value FROM platform_settings WHERE key=?`, store.PlatformSettingMailFromName).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != "Tiller" {
		t.Fatalf("from name stored as %q, want plaintext", raw)
	}
}

// TestSeedPlatformMailSettingsSeedsFromName proves the environment bootstrap
// seeds the display name once and never overwrites a later dashboard value.
func TestSeedPlatformMailSettingsSeedsFromName(t *testing.T) {
	t.Parallel()
	_, st := openStore(t)
	ctx := context.Background()

	if err := st.SeedPlatformMailSettings(ctx, store.PlatformMailSettings{Provider: "smtp", From: "a@example.com", FromName: "Seeded", SMTPHost: "h", SMTPPort: "587", SMTPMode: "starttls"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetPlatformMailSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.FromName != "Seeded" {
		t.Fatalf("seeded from name = %q", got.FromName)
	}

	if err := st.SeedPlatformMailSettings(ctx, store.PlatformMailSettings{Provider: "smtp", From: "b@example.com", FromName: "Overwritten", SMTPHost: "h", SMTPPort: "587", SMTPMode: "starttls"}); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetPlatformMailSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.FromName != "Seeded" || got.From != "a@example.com" {
		t.Fatalf("re-seed overwrote existing values: %+v", got)
	}
}
