package server

import "testing"

func TestCreateClientKeyDefaultsToCatalogue(t *testing.T) {
	api, db, _, _ := loggingTestHarness(t, mockUpstream(t))

	status, payload, _ := api.request("POST", "/api/admin/client-keys", map[string]any{"name": "catalogue default"})
	if status != 201 {
		t.Fatalf("create key: %d %v", status, payload)
	}
	if payload["type"] != "catalogue" {
		t.Fatalf("created key type = %v, want catalogue", payload["type"])
	}

	clientID := payload["id"].(string)
	var keyType string
	if err := db.SQL.QueryRow(`SELECT key_type FROM client_keys WHERE id=?`, clientID).Scan(&keyType); err != nil {
		t.Fatal(err)
	}
	if keyType != "catalogue" {
		t.Fatalf("stored key type = %q, want catalogue", keyType)
	}

	var bindings int
	if err := db.SQL.QueryRow(`SELECT count(*) FROM client_single_bindings WHERE client_key_id=?`, clientID).Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	if bindings != 0 {
		t.Fatalf("catalogue key has %d Single bindings, want none", bindings)
	}
}

func TestDeleteProviderWithStaleSingleRealBinding(t *testing.T) {
	api, db, _, _ := loggingTestHarness(t, mockUpstream(t))

	var providerID, modelID string
	if err := db.SQL.QueryRow(`SELECT id FROM providers WHERE name='provider-a'`).Scan(&providerID); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL.QueryRow(`SELECT id FROM provider_models WHERE upstream_model_id='model-a'`).Scan(&modelID); err != nil {
		t.Fatal(err)
	}

	status, payload, _ := api.request("POST", "/api/admin/client-keys", map[string]any{
		"name": "single real client", "type": "single", "single_target_type": "real", "single_target_id": modelID,
	})
	if status != 201 {
		t.Fatalf("create Single key: %d %v", status, payload)
	}
	clientID := payload["id"].(string)

	status, payload, _ = api.request("PATCH", "/api/admin/client-keys/"+clientID, map[string]any{"type": "catalogue"})
	if status != 204 {
		t.Fatalf("switch to Catalogue: %d %v", status, payload)
	}

	var bindings int
	if err := db.SQL.QueryRow(`SELECT count(*) FROM client_single_bindings WHERE client_key_id=?`, clientID).Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	if bindings != 1 {
		t.Fatalf("switch to Catalogue removed the binding row: %d", bindings)
	}

	status, payload, _ = api.request("DELETE", "/api/admin/providers/"+providerID, nil)
	if status != 204 {
		t.Fatalf("delete provider with stale real binding: %d %v", status, payload)
	}
	if err := db.SQL.QueryRow(`SELECT count(*) FROM client_single_bindings WHERE client_key_id=?`, clientID).Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	if bindings != 0 {
		t.Fatalf("provider deletion left stale binding rows: %d", bindings)
	}
}
