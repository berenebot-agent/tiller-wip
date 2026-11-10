package server

import (
	"testing"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/providers"
)

func lookupProvider(typ string) (bool, bool) {
	d, ok := providers.Lookup(typ)
	return d.BaseURLLocked, ok
}

func insertTestProvider(t *testing.T, db *database.DB, id, typ, baseURL, credential string) {
	t.Helper()
	var accountID string
	if err := db.SQL.QueryRow(`SELECT account_id FROM providers LIMIT 1`).Scan(&accountID); err != nil {
		t.Fatal(err)
	}
	var cred any
	if credential != "" {
		cred = credential
	}
	if _, err := db.SQL.Exec(`INSERT INTO namespaces(account_id,name,kind,entity_id) VALUES(?,?,'real',?)`, accountID, id, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO providers(id,account_id,name,type,base_url,credential_secret,enabled,protocols,created_at,updated_at) VALUES(?,?,?,?,?,?,1,'chat',?,?)`,
		id, accountID, id, typ, baseURL, cred, database.Now(), database.Now()); err != nil {
		t.Fatal(err)
	}
}

func credentialPresent(t *testing.T, db *database.DB, id string) bool {
	t.Helper()
	var present bool
	if err := db.SQL.QueryRow(`SELECT credential_secret IS NOT NULL FROM providers WHERE id=?`, id).Scan(&present); err != nil {
		t.Fatal(err)
	}
	return present
}

func TestProviderBaseURLLockedForFixedVendorAndOAuth(t *testing.T) {
	api, db, _, _ := loggingTestHarness(t, mockUpstream(t))
	insertTestProvider(t, db, "p-openai", "openai", "https://api.openai.com/v1", "sk-secret")
	insertTestProvider(t, db, "p-codex", "codex-subscription", "https://chatgpt.com/backend-api/codex", "")

	for _, id := range []string{"p-openai", "p-codex"} {
		status, payload, _ := api.request("PATCH", "/api/admin/providers/"+id, map[string]any{"base_url": "https://evil.example.com/v1", "confirm_credential_reset": true})
		if status != 400 {
			t.Fatalf("%s: repointing base_url = %d %v, want 400", id, status, payload)
		}
		if errObj, _ := payload["error"].(map[string]any); errObj["code"] != "base_url_locked" {
			t.Fatalf("%s: error = %v, want base_url_locked", id, payload)
		}
	}
	if !credentialPresent(t, db, "p-openai") {
		t.Fatal("rejected URL change cleared the credential")
	}

	// Resending the unchanged URL (with or without a trailing slash) is not a change.
	status, payload, _ := api.request("PATCH", "/api/admin/providers/p-openai", map[string]any{"base_url": "https://api.openai.com/v1/"})
	if status != 204 {
		t.Fatalf("unchanged base_url = %d %v, want 204", status, payload)
	}
	if !credentialPresent(t, db, "p-openai") {
		t.Fatal("unchanged URL cleared the credential")
	}
}

func TestProviderHostChangeClearsCredentialOnEditableTypes(t *testing.T) {
	api, db, _, _ := loggingTestHarness(t, mockUpstream(t))
	insertTestProvider(t, db, "p-gen", "generic-openai", "https://llm.example.com/v1", "sk-secret")

	// A new host needs confirmation and leaves everything untouched without it.
	status, payload, _ := api.request("PATCH", "/api/admin/providers/p-gen", map[string]any{"base_url": "https://other.example.net/v1"})
	if status != 409 {
		t.Fatalf("host change without confirmation = %d %v, want 409", status, payload)
	}
	if errObj, _ := payload["error"].(map[string]any); errObj["code"] != "credential_reset_confirmation_required" {
		t.Fatalf("error = %v", payload)
	}
	if !credentialPresent(t, db, "p-gen") {
		t.Fatal("unconfirmed host change cleared the credential")
	}

	// A path-only change on the same host keeps the credential.
	status, payload, _ = api.request("PATCH", "/api/admin/providers/p-gen", map[string]any{"base_url": "https://LLM.example.com/v2"})
	if status != 204 {
		t.Fatalf("path change = %d %v, want 204", status, payload)
	}
	if !credentialPresent(t, db, "p-gen") {
		t.Fatal("same-host change cleared the credential")
	}

	// A confirmed host change clears it in the same update.
	status, payload, _ = api.request("PATCH", "/api/admin/providers/p-gen", map[string]any{"base_url": "https://other.example.net/v1", "confirm_credential_reset": true})
	if status != 204 {
		t.Fatalf("confirmed host change = %d %v, want 204", status, payload)
	}
	if credentialPresent(t, db, "p-gen") {
		t.Fatal("host change left the old credential in place")
	}
	var base string
	if err := db.SQL.QueryRow(`SELECT base_url FROM providers WHERE id='p-gen'`).Scan(&base); err != nil || base != "https://other.example.net/v1" {
		t.Fatalf("base_url = %q, %v", base, err)
	}
}

func TestProviderDescriptorsExposeBaseURLLock(t *testing.T) {
	for typ, want := range map[string]bool{
		"openai": true, "anthropic": true, "codex-subscription": true, "github-copilot": true,
		"generic-openai": false, "vllm": false, "azure-openai": false, "bedrock-api-key": false, "cloudflare-ai": false,
		"ollama-local": false, "lm-studio": false, "llama-cpp": false, "opencode-free": false,
	} {
		d, ok := lookupProvider(typ)
		if !ok {
			t.Fatalf("unknown type %s", typ)
		}
		if d != want {
			t.Errorf("%s locked = %v, want %v", typ, d, want)
		}
	}
}
