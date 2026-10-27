package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
)

// TestCatalogueTemplateLifecycle drives templates through the admin API and
// checks the client-visible catalogue follows the linked template.
func TestCatalogueTemplateLifecycle(t *testing.T) {
	t.Parallel()
	api, db, _, _ := loggingTestHarness(t, mockUpstream(t))

	var modelID string
	if err := db.SQL.QueryRow(`SELECT id FROM provider_models WHERE upstream_model_id='model-a'`).Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	clientModels := func(secret string) []string {
		t.Helper()
		req, _ := http.NewRequest("GET", api.base+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var catalogue struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&catalogue); err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, m := range catalogue.Data {
			out = append(out, m.ID)
		}
		return out
	}

	status, payload, _ := api.request("POST", "/api/admin/catalogue-templates", map[string]any{"name": "Coding agents", "start_from": "empty"})
	if status != 201 {
		t.Fatalf("create template: %d %v", status, payload)
	}
	templateID := payload["id"].(string)
	if status, payload, _ = api.request("POST", "/api/admin/catalogue-templates", map[string]any{"name": "Coding agents"}); status != 409 {
		t.Fatalf("duplicate template name: %d %v", status, payload)
	}
	if status, payload, _ = api.request("POST", "/api/admin/catalogue-templates", map[string]any{"name": "x", "start_from": "bogus"}); status != 400 {
		t.Fatalf("bad start_from: %d %v", status, payload)
	}

	if status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "bad link", "type": "catalogue", "template_id": "missing"}); status != 400 || payload["error"] == nil {
		t.Fatalf("unknown template link: %d %v", status, payload)
	}
	if status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "single link", "type": "single", "single_target_type": "real", "single_target_id": modelID, "template_id": templateID}); status != 400 {
		t.Fatalf("single key with template: %d %v", status, payload)
	}

	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "linked", "type": "catalogue", "template_id": templateID})
	if status != 201 {
		t.Fatalf("create linked client: %d %v", status, payload)
	}
	clientID, secret := payload["id"].(string), payload["secret"].(string)
	if got := clientModels(secret); len(got) != 0 {
		t.Fatalf("empty template catalogue = %v", got)
	}

	status, payload, _ = api.request("PUT", "/api/admin/catalogue-templates/"+templateID+"/permissions", map[string]any{"defaults": []any{}, "permissions": []any{map[string]any{"kind": "real", "model_id": modelID, "enabled": true}}})
	if status != 204 {
		t.Fatalf("template permissions: %d %v", status, payload)
	}
	if got := clientModels(secret); len(got) != 1 || got[0] != "provider-a/model-a" {
		t.Fatalf("linked catalogue after template save = %v", got)
	}

	status, payload, _ = api.request("PUT", "/api/admin/client-keys/"+clientID+"/permissions", map[string]any{"defaults": []any{}, "permissions": []any{}})
	if status != 409 || payload["error"].(map[string]any)["code"] != "client_linked_to_template" {
		t.Fatalf("direct permissions on linked client: %d %v", status, payload)
	}

	status, payload, _ = api.request("GET", "/api/admin/client-keys", nil)
	if status != 200 {
		t.Fatalf("list clients: %d %v", status, payload)
	}
	found := false
	for _, raw := range payload["data"].([]any) {
		c := raw.(map[string]any)
		if c["id"] == clientID {
			found = c["template_id"] == templateID && c["template_name"] == "Coding agents"
		}
	}
	if !found {
		t.Fatalf("client list does not show the template link: %v", payload)
	}
	status, payload, _ = api.request("GET", "/api/admin/catalogue-templates", nil)
	if status != 200 || len(payload["data"].([]any)) != 1 || payload["data"].([]any)[0].(map[string]any)["client_count"] != float64(1) {
		t.Fatalf("list templates: %d %v", status, payload)
	}

	// Changing to Single clears the link.
	if status, payload, _ = api.request("PATCH", "/api/admin/client-keys/"+clientID, map[string]any{"type": "single", "single_target_type": "real", "single_target_id": modelID}); status != 204 {
		t.Fatalf("switch to single: %d %v", status, payload)
	}
	var link sql.NullString
	if err := db.SQL.QueryRow(`SELECT template_id FROM client_keys WHERE id=?`, clientID).Scan(&link); err != nil {
		t.Fatal(err)
	}
	if link.Valid {
		t.Fatalf("single key kept template link %q", link.String)
	}

	// Relink as Catalogue, then delete the template: the client keeps access.
	if status, payload, _ = api.request("PATCH", "/api/admin/client-keys/"+clientID, map[string]any{"type": "catalogue", "template_id": templateID}); status != 204 {
		t.Fatalf("relink: %d %v", status, payload)
	}
	if status, payload, _ = api.request("DELETE", "/api/admin/catalogue-templates/"+templateID, nil); status != 204 {
		t.Fatalf("delete template: %d %v", status, payload)
	}
	if got := clientModels(secret); len(got) != 1 || got[0] != "provider-a/model-a" {
		t.Fatalf("catalogue after template delete = %v", got)
	}
	if status, _, _ = api.request("GET", "/api/admin/catalogue-templates/"+templateID, nil); status != 404 {
		t.Fatalf("deleted template still readable: %d", status)
	}
	if status, payload, _ = api.request("PUT", "/api/admin/client-keys/"+clientID+"/permissions", map[string]any{"defaults": []any{}, "permissions": []any{}}); status != 204 {
		t.Fatalf("permissions after detach: %d %v", status, payload)
	}
}
