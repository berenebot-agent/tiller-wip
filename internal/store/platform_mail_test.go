package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/tiller-router/tiller-router/internal/crypto"
	"github.com/tiller-router/tiller-router/internal/database"
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

// TestPlatformMailSettingsSurviveLockedCipher proves a locked secret cipher does
// not blank the public mail fields. Only the secret degrades, and the error
// wraps ErrSecretsLocked so the API can report it separately.
func TestPlatformMailSettingsSurviveLockedCipher(t *testing.T) {
	t.Parallel()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()

	unlocked := store.New(db.SQL, store.WithCipher(testCipher(t, 1)))
	proposal := store.PlatformSettingsProposal{
		HostedSignupEnabled: true,
		AuditRetentionDays:  30,
		Mail: store.PlatformMailSettings{
			Provider: "brevo", From: "no-reply@example.com", FromName: "Tiller",
			BrevoAPIKey: "xkeysib-secret", SMTPMode: "starttls",
		},
	}
	if err := unlocked.SavePlatformSettings(ctx, proposal); err != nil {
		t.Fatal(err)
	}

	locked := store.New(db.SQL, store.WithCipher(crypto.Locked()))
	got, err := locked.GetPlatformMailSettings(ctx)
	if !errors.Is(err, store.ErrSecretsLocked) {
		t.Fatalf("locked read error = %v, want ErrSecretsLocked", err)
	}
	if got.Provider != "brevo" || got.From != "no-reply@example.com" || got.FromName != "Tiller" {
		t.Fatalf("public fields lost under a locked cipher: %+v", got)
	}
	if got.BrevoAPIKey != "" {
		t.Fatalf("secret should not decrypt while locked, got %q", got.BrevoAPIKey)
	}

	// A save while locked must not overwrite the stored secret with the empty
	// value the locked read produced.
	proposal.Mail.BrevoAPIKey = store.SecretUnchanged
	proposal.Mail.FromName = "Tiller Router"
	if err := locked.SavePlatformSettings(ctx, proposal); err != nil {
		t.Fatal(err)
	}
	got, err = unlocked.GetPlatformMailSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.BrevoAPIKey != "xkeysib-secret" {
		t.Fatalf("stored secret was erased by a locked save: %q", got.BrevoAPIKey)
	}
	if got.FromName != "Tiller Router" {
		t.Fatalf("public field did not save while locked: %q", got.FromName)
	}
}

// TestSeedPlatformMailSettingsRunsOnce proves a value cleared in the dashboard
// is not silently re-seeded from the environment on the next boot.
func TestSeedPlatformMailSettingsRunsOnce(t *testing.T) {
	t.Parallel()
	_, st := openStore(t)
	ctx := context.Background()

	if err := st.SeedPlatformMailSettings(ctx, store.PlatformMailSettings{Provider: "smtp", From: "a@example.com", FromName: "Seeded", SMTPHost: "h", SMTPPort: "587", SMTPMode: "starttls"}); err != nil {
		t.Fatal(err)
	}
	// Operator clears the display name in the dashboard.
	if err := st.SetPlatformSetting(ctx, store.PlatformSettingMailFromName, ""); err != nil {
		t.Fatal(err)
	}
	// The next boot re-runs the seed with the environment value.
	if err := st.SeedPlatformMailSettings(ctx, store.PlatformMailSettings{Provider: "smtp", From: "a@example.com", FromName: "Seeded", SMTPHost: "h", SMTPPort: "587", SMTPMode: "starttls"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetPlatformMailSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.FromName != "" {
		t.Fatalf("cleared from name was re-seeded to %q", got.FromName)
	}
}

// TestSavePlatformMailBlankSecretKeepsStoredKey proves a blank secret field does
// not erase the stored secret. The dashboard never sends secrets back to the
// browser, so the box is always empty; saving any other field (the sender name,
// say) must leave the API key exactly as it was.
func TestSavePlatformMailBlankSecretKeepsStoredKey(t *testing.T) {
	t.Parallel()
	_, st := openStore(t)
	ctx := context.Background()

	proposal := store.PlatformSettingsProposal{
		HostedSignupEnabled: true,
		AuditRetentionDays:  30,
		Mail: store.PlatformMailSettings{
			Provider: "brevo", From: "tiller@example.com", FromName: "Old Name",
			BrevoAPIKey: "xkeysib-secret",
		},
	}
	if err := st.SavePlatformSettings(ctx, proposal); err != nil {
		t.Fatal(err)
	}

	current, err := st.GetPlatformMailSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	current.FromName = "Tiller Router"
	current.BrevoAPIKey = ""
	proposal.Mail = current
	if err := st.SavePlatformSettings(ctx, proposal); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetPlatformMailSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.BrevoAPIKey != "xkeysib-secret" {
		t.Fatalf("blank secret field erased the stored key: %q", got.BrevoAPIKey)
	}
	if got.FromName != "Tiller Router" {
		t.Fatalf("from name did not save: %q", got.FromName)
	}
}
