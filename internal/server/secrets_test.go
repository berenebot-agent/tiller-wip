package server

import (
	"bytes"
	"testing"

	"github.com/tiller-router/tiller-router/internal/crypto"
)

func TestSettingsExposeEncryptionState(t *testing.T) {
	api, _, _, _ := loggingTestHarness(t, mockUpstream(t))

	state := func() string {
		status, payload, _ := api.request("GET", "/api/admin/settings", nil)
		if status != 200 {
			t.Fatalf("get settings: %d %v", status, payload)
		}
		raw, ok := payload["provider_credential_encryption"].(map[string]any)
		if !ok {
			t.Fatalf("missing provider_credential_encryption in settings: %v", payload)
		}
		return raw["state"].(string)
	}

	if got := state(); got != "disabled" {
		t.Fatalf("default encryption state = %q, want disabled", got)
	}
	c, err := crypto.New(bytes.Repeat([]byte{1}, crypto.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	api.server.secretCipher = c
	if got := state(); got != "enabled" {
		t.Fatalf("enabled encryption state = %q, want enabled", got)
	}
	api.server.secretCipher = crypto.Locked()
	if got := state(); got != "locked" {
		t.Fatalf("locked encryption state = %q, want locked", got)
	}
}
