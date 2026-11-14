package server

import (
	"net/http"
	"strings"
	"testing"
)

// TestPlatformMailSaveKeepsSecretAndName covers the operator path that looked
// lossy from the dashboard: set a provider and key, then later edit only the
// sender display name while the secret box is blank (it is always blank, since
// secrets are never sent to the browser).
func TestPlatformMailSaveKeepsSecretAndName(t *testing.T) {
	t.Parallel()
	app, _, _ := hostedServerHarness(t, false)
	papi := hostedPlatformAPI(t, app)

	status, payload, _ := papi.request(http.MethodPut, "/api/platform/settings", map[string]any{
		"mail_provider": "brevo", "mail_from": "tiller@example.com",
		"mail_from_name": "First", "mail_brevo_api_key": "xkeysib-secret",
	})
	if status != http.StatusNoContent {
		t.Fatalf("initial mail save = %d %v", status, payload)
	}

	// The reload reports the secret as stored.
	_, payload, _ = papi.request(http.MethodGet, "/api/platform/settings", nil)
	mail, _ := payload["mail"].(map[string]any)
	if mail["secret_configured"] != true {
		t.Fatalf("secret_configured after first save = %v", mail["secret_configured"])
	}

	// Second save: the form posts every public field, and omits the blank
	// secret entirely (as the dashboard does).
	status, payload, _ = papi.request(http.MethodPut, "/api/platform/settings", map[string]any{
		"mail_provider": "brevo", "mail_from": "tiller@example.com",
		"mail_from_name": "Tiller Router", "mail_smtp_port": 0,
		"mail_smtp_host": "", "mail_smtp_mode": "", "mail_smtp_username": "",
	})
	if status != http.StatusNoContent {
		t.Fatalf("second mail save = %d %v", status, payload)
	}

	_, payload, _ = papi.request(http.MethodGet, "/api/platform/settings", nil)
	mail, _ = payload["mail"].(map[string]any)
	if mail["from_name"] != "Tiller Router" {
		t.Fatalf("from_name after edit = %v, want Tiller Router", mail["from_name"])
	}
	if mail["secret_configured"] != true {
		t.Fatalf("blank secret field erased the stored key (secret_configured=%v)", mail["secret_configured"])
	}
}

// TestPlatformMailSaveExplainsInvalidSettings proves a rejected save names the
// offending field instead of a bare "Mail settings are invalid".
func TestPlatformMailSaveExplainsInvalidSettings(t *testing.T) {
	t.Parallel()
	app, _, _ := hostedServerHarness(t, false)
	papi := hostedPlatformAPI(t, app)

	status, payload, _ := papi.request(http.MethodPut, "/api/platform/settings", map[string]any{
		"mail_provider": "brevo", "mail_from": "tiller@example.com",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("brevo without api key = %d %v", status, payload)
	}
	errObj, _ := payload["error"].(map[string]any)
	detail, _ := errObj["detail"].(string)
	if detail == "" {
		t.Fatalf("invalid mail settings carried no detail: %v", payload)
	}
	// The detail must name the missing field without leaking any value.
	if !strings.Contains(strings.ToLower(detail), "api key") {
		t.Fatalf("detail = %q, want it to mention the api key", detail)
	}
}
