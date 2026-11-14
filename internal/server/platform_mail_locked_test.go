package server

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/tiller-router/tiller-router/internal/crypto"
	"github.com/tiller-router/tiller-router/internal/store"
)

// TestPlatformMailSecretWriteRejectedWhileLocked proves a locked master key
// refuses new mail secrets (no placeholder credential is activated) while
// non-secret edits still save and leave the running mailer alone.
func TestPlatformMailSecretWriteRejectedWhileLocked(t *testing.T) {
	t.Parallel()
	app, _, _ := hostedServerHarness(t, false)
	papi := hostedPlatformAPI(t, app)

	key, err := crypto.New(bytes.Repeat([]byte{1}, crypto.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	unlocked := store.New(app.db.SQL, store.WithCipher(key))
	if err := unlocked.SavePlatformSettings(context.Background(), store.PlatformSettingsProposal{
		HostedSignupEnabled: true, AuditRetentionDays: 30,
		Mail: store.PlatformMailSettings{Provider: "brevo", From: "no-reply@example.com", FromName: "Tiller", BrevoAPIKey: "xkeysib-secret", SMTPMode: "starttls"},
	}); err != nil {
		t.Fatal(err)
	}
	// Auth secrets share the key; drop their rows so the request reaches the
	// mail-locked branch rather than failing earlier on the auth-settings read.
	if _, err := app.db.SQL.Exec(`DELETE FROM platform_settings WHERE key IN (?,?)`, store.PlatformSettingGoogleClientSecret, store.PlatformSettingTurnstileSecret); err != nil {
		t.Fatal(err)
	}
	app.store = store.New(app.db.SQL, store.WithCipher(crypto.Locked()))

	status, payload, _ := papi.request(http.MethodPut, "/api/platform/settings", map[string]any{"mail_brevo_api_key": "new-key"})
	if status != http.StatusConflict || errorCode(payload) != "secrets_locked" {
		t.Fatalf("secret write while locked = %d %v, want 409 secrets_locked", status, payload)
	}
	if got := app.mailer.Status(); got.Configured {
		t.Fatalf("mailer was activated while locked: %+v", got)
	}

	status, payload, _ = papi.request(http.MethodPut, "/api/platform/settings", map[string]any{"mail_from_name": "Renamed"})
	if status != http.StatusNoContent {
		t.Fatalf("non-secret edit while locked = %d %v, want 204", status, payload)
	}
	if got := app.mailer.Status(); got.Configured {
		t.Fatalf("mailer was activated from partial data: %+v", got)
	}
}
