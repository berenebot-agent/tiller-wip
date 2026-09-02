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
