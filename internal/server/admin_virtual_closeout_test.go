package server

import (
	"net/http"
	"testing"
	"time"
)

func boolPtr(v bool) *bool { return &v }

func TestTriStateANDBoolTruthTableAndEmptySet(t *testing.T) {
	cases := []struct {
		name string
		in   []*bool
		want *bool
	}{
		{name: "empty is unknown", in: nil, want: nil},
		{name: "yes yes is yes", in: []*bool{boolPtr(true), boolPtr(true)}, want: boolPtr(true)},
		{name: "yes no is no", in: []*bool{boolPtr(true), boolPtr(false)}, want: boolPtr(false)},
		{name: "yes unknown is unknown", in: []*bool{boolPtr(true), nil}, want: nil},
		{name: "unknown no is no", in: []*bool{nil, boolPtr(false)}, want: boolPtr(false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := triStateANDBool(tc.in)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("got %v, want unknown", *got)
				}
				return
			}
			if got == nil || *got != *tc.want {
				t.Fatalf("got %v, want %v", got, *tc.want)
			}
		})
	}
}

func TestAggregateVirtualNumericRequiresKnownPositiveEligibleValues(t *testing.T) {
	known := []virtualTargetView{
		{Enabled: true, Available: true, ContextLength: int64Ptr(128000), MaxOutputTokens: int64Ptr(8192)},
		{Enabled: true, Available: true, ContextLength: int64Ptr(64000), MaxOutputTokens: int64Ptr(4096)},
		{Enabled: false, Available: true, ContextLength: int64Ptr(1), MaxOutputTokens: int64Ptr(1)},
	}
	if got := aggregateVirtualNumeric(known, func(target virtualTargetView) *int64 { return target.ContextLength }); got == nil || *got != 64000 {
		t.Fatalf("context aggregate = %v, want 64000", got)
	}
	if got := aggregateVirtualNumeric(known, func(target virtualTargetView) *int64 { return target.MaxOutputTokens }); got == nil || *got != 4096 {
		t.Fatalf("output aggregate = %v, want 4096", got)
	}
	for _, target := range []virtualTargetView{
		{Enabled: true, Available: true},
		{Enabled: true, Available: true, ContextLength: int64Ptr(0)},
		{Enabled: true, Available: false, ContextLength: int64Ptr(1)},
	} {
		if got := aggregateVirtualNumeric([]virtualTargetView{target}, func(target virtualTargetView) *int64 { return target.ContextLength }); got != nil {
			t.Fatalf("aggregate for %+v = %v, want unknown", target, *got)
		}
	}
}

func TestVirtualAdminUsesAllEligibleTargetsForAvailabilityCapabilitiesAndDeletion(t *testing.T) {
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	var providerA, modelA string
	if err := db.SQL.QueryRow(`SELECT id FROM providers WHERE name='provider-a'`).Scan(&providerA); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL.QueryRow(`SELECT id FROM provider_models WHERE upstream_model_id='model-a'`).Scan(&modelA); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.SQL.Exec(`UPDATE provider_models SET context_length=128000,max_output_tokens=8192,supports_tools=1,supports_vision=1,supports_reasoning=1,supports_structured_output=1 WHERE id=?`, modelA); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO namespaces(name,kind,entity_id) VALUES('provider-b','real','provider-b-id')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO providers(id,name,type,base_url,credential_secret,enabled,protocols,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, "provider-b-id", "provider-b", "generic-openai", "http://127.0.0.1:1/v1", "secret-b", 1, `["chat"]`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO provider_models(id,provider_id,upstream_model_id,available,first_seen_at,last_seen_at,created_at,updated_at,context_length,max_output_tokens,supports_tools,supports_vision,supports_reasoning,supports_structured_output) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, "model-b-id", "provider-b-id", "model-b", 1, now, now, now, now, 64000, 4096, 1, 0, nil, 1); err != nil {
		t.Fatal(err)
	}

	status, payload, _ := api.request("POST", "/api/admin/virtual-groups", map[string]any{"name": "virtual"})
	if status != http.StatusCreated {
		t.Fatalf("create group: %d %v", status, payload)
	}
	groupID := payload["id"].(string)
	status, payload, _ = api.request("POST", "/api/admin/virtual-models", map[string]any{
		"group_id": groupID, "name": "fallback", "routing_mode": "ordered_fallback",
		"targets": []any{
			map[string]any{"provider_model_id": modelA, "enabled": true},
			map[string]any{"provider_model_id": "model-b-id", "enabled": true},
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("create virtual: %d %v", status, payload)
	}
	virtualID := payload["id"].(string)

	status, payload, _ = api.request("GET", "/api/admin/virtual-models", nil)
	if status != http.StatusOK {
		t.Fatalf("list virtual: %d %v", status, payload)
	}
	virtual := findObjectByID(t, payload["data"], virtualID)
	if virtual["available"] != true || virtual["context_length"] != float64(64000) || virtual["max_output_tokens"] != float64(4096) {
		t.Fatalf("virtual aggregate ignored target 2: %v", virtual)
	}
	if virtual["supports_tools"] != true || virtual["supports_vision"] != false {
		t.Fatalf("virtual capabilities were not aggregated from both targets: %v", virtual)
	}
	if virtual["supports_reasoning"] != nil {
		t.Fatalf("unknown target capability should remain unknown: %v", virtual)
	}

	// The fallback remains available when the first target's provider is down.
	if _, err := db.SQL.Exec(`UPDATE providers SET enabled=0 WHERE id=?`, providerA); err != nil {
		t.Fatal(err)
	}
	status, payload, _ = api.request("GET", "/api/admin/virtual-models", nil)
	virtual = findObjectByID(t, payload["data"], virtualID)
	if status != http.StatusOK || virtual["available"] != true {
		t.Fatalf("fallback target did not preserve availability: %d %v", status, virtual)
	}
	status, payload, _ = api.request("GET", "/api/admin/health", nil)
	if status != http.StatusOK || payload["broken_virtual_models"] != float64(0) {
		t.Fatalf("fallback target incorrectly counted as broken: %d %v", status, payload)
	}
	status, payload, _ = api.request("PUT", "/api/admin/client-keys/"+clientID+"/permissions", map[string]any{
		"defaults": []any{}, "permissions": []any{map[string]any{"kind": "virtual", "model_id": virtualID, "enabled": true}},
	})
	if status != http.StatusNoContent {
		t.Fatalf("set virtual permission: %d %v", status, payload)
	}
	status, payload, _ = api.request("GET", "/api/admin/client-keys/"+clientID+"/permissions", nil)
	if status != http.StatusOK || permissionAvailability(payload, virtualID) != true {
		t.Fatalf("permission view ignored fallback availability: %d %v", status, payload)
	}

	// Target 2 is a real dependency even though the compatibility primary
	// column points at provider A. Deletion must be rejected atomically.
	status, payload, _ = api.request("DELETE", "/api/admin/providers/provider-b-id", nil)
	if status != http.StatusConflict || payload["error"].(map[string]any)["code"] != "provider_in_use" {
		t.Fatalf("target-2 provider deletion was not blocked: %d %v", status, payload)
	}
	var count int
	if err := db.SQL.QueryRow(`SELECT count(*) FROM providers WHERE id='provider-b-id'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("provider changed after rejected deletion: count=%d err=%v", count, err)
	}
	if err := db.SQL.QueryRow(`SELECT count(*) FROM virtual_models WHERE id=?`, virtualID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("virtual model changed after rejected deletion: count=%d err=%v", count, err)
	}
}

func findObjectByID(t *testing.T, raw any, id string) map[string]any {
	t.Helper()
	for _, item := range raw.([]any) {
		object := item.(map[string]any)
		if object["id"] == id {
			return object
		}
	}
	t.Fatalf("object %q not found in %v", id, raw)
	return nil
}

func permissionAvailability(payload map[string]any, modelID string) bool {
	for _, group := range payload["groups"].([]any) {
		for _, raw := range group.(map[string]any)["models"].([]any) {
			model := raw.(map[string]any)
			if model["id"] == modelID {
				return model["available"] == true
			}
		}
	}
	return false
}
