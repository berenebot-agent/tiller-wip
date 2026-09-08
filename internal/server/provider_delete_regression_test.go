package server

import (
	"net/http"
	"testing"
)

func deleteMatrixHarness(t *testing.T) (*testAPI, string, string, string, string, string) {
	t.Helper()
	api, db, _, _ := loggingTestHarness(t, mockUpstream(t))
	var providerA, modelA string
	if err := db.SQL.QueryRow(`SELECT id FROM providers WHERE name='provider-a'`).Scan(&providerA); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL.QueryRow(`SELECT id FROM provider_models WHERE upstream_model_id='model-a'`).Scan(&modelA); err != nil {
		t.Fatal(err)
	}
	providerB, modelB := seedSecondProvider(t, db)

	status, payload, _ := api.request("POST", "/api/admin/virtual-groups", map[string]any{"name": "virtual"})
	if status != http.StatusCreated {
		t.Fatalf("create group: %d %v", status, payload)
	}
	groupID := payload["id"].(string)
	status, payload, _ = api.request("POST", "/api/admin/virtual-models", map[string]any{
		"group_id": groupID, "name": "fallback", "routing_mode": "ordered_fallback",
		"targets": []any{
			map[string]any{"provider_model_id": modelA, "enabled": true},
			map[string]any{"provider_model_id": modelB, "enabled": true},
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("create virtual: %d %v", status, payload)
	}
	return api, providerA, modelA, providerB, modelB, payload["id"].(string)
}

func TestProviderDeleteAvailabilityMatrix(t *testing.T) {
	cases := []struct {
		name       string
		aAvailable bool
		bEnabled   bool
		bAvailable bool
		wantStatus int
	}{
		{name: "a_unavailable_b_disabled_blocks", aAvailable: false, bEnabled: false, bAvailable: true, wantStatus: http.StatusConflict},
		{name: "a_unavailable_b_retired_blocks", aAvailable: false, bEnabled: true, bAvailable: false, wantStatus: http.StatusConflict},
		{name: "a_unavailable_b_healthy_succeeds", aAvailable: false, bEnabled: true, bAvailable: true, wantStatus: http.StatusNoContent},
		{name: "a_healthy_b_unavailable_blocks", aAvailable: true, bEnabled: true, bAvailable: false, wantStatus: http.StatusConflict},
		{name: "a_healthy_b_disabled_blocks", aAvailable: true, bEnabled: false, bAvailable: true, wantStatus: http.StatusConflict},
		{name: "a_disabled_b_healthy_succeeds", aAvailable: true, bEnabled: true, bAvailable: true, wantStatus: http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api, providerA, modelA, providerB, modelB, virtualID := deleteMatrixHarness(t)
			db := api.server.db
			if _, err := db.SQL.Exec(`UPDATE provider_models SET available=? WHERE id=?`, boolInt(tc.aAvailable), modelA); err != nil {
				t.Fatal(err)
			}
			if _, err := db.SQL.Exec(`UPDATE providers SET enabled=? WHERE id=?`, boolInt(tc.bEnabled), providerB); err != nil {
				t.Fatal(err)
			}
			if _, err := db.SQL.Exec(`UPDATE provider_models SET available=? WHERE id=?`, boolInt(tc.bAvailable), modelB); err != nil {
				t.Fatal(err)
			}
			if tc.name == "a_disabled_b_healthy_succeeds" {
				if _, err := db.SQL.Exec(`UPDATE providers SET enabled=0 WHERE id=?`, providerA); err != nil {
					t.Fatal(err)
				}
			}

			status, payload, _ := api.request("DELETE", "/api/admin/providers/"+providerA, nil)
			if status != tc.wantStatus {
				t.Fatalf("delete A: status = %d, want %d (%v)", status, tc.wantStatus, payload)
			}
			var count int
			if tc.wantStatus == http.StatusConflict {
				if payload["error"].(map[string]any)["code"] != "provider_in_use" {
					t.Fatalf("blocked delete must use provider_in_use: %v", payload)
				}
				if err := db.SQL.QueryRow(`SELECT count(*) FROM providers WHERE id=?`, providerA).Scan(&count); err != nil || count != 1 {
					t.Fatalf("blocked delete must roll back provider row: count=%d err=%v", count, err)
				}
				if err := db.SQL.QueryRow(`SELECT count(*) FROM virtual_model_targets WHERE virtual_model_id=?`, virtualID).Scan(&count); err != nil || count != 2 {
					t.Fatalf("blocked delete must keep both targets: count=%d err=%v", count, err)
				}
				var compatProvider, compatModel string
				if err := db.SQL.QueryRow(`SELECT target_provider_id,target_provider_model_id FROM virtual_models WHERE id=?`, virtualID).Scan(&compatProvider, &compatModel); err != nil {
					t.Fatal(err)
				}
				if compatProvider != providerA || compatModel != modelA {
					t.Fatalf("blocked delete must not repoint compat primary: got %s/%s", compatProvider, compatModel)
				}
				return
			}
			if err := db.SQL.QueryRow(`SELECT count(*) FROM providers WHERE id=?`, providerA).Scan(&count); err != nil || count != 0 {
				t.Fatalf("provider A should be deleted: count=%d err=%v", count, err)
			}
			if err := db.SQL.QueryRow(`SELECT count(*) FROM virtual_model_targets WHERE virtual_model_id=?`, virtualID).Scan(&count); err != nil || count != 1 {
				t.Fatalf("virtual model should have one target left: count=%d err=%v", count, err)
			}
			var compatProvider, compatModel string
			if err := db.SQL.QueryRow(`SELECT target_provider_id,target_provider_model_id FROM virtual_models WHERE id=?`, virtualID).Scan(&compatProvider, &compatModel); err != nil {
				t.Fatal(err)
			}
			if compatProvider != providerB || compatModel != modelB {
				t.Fatalf("compat primary after delete = %s/%s, want %s/%s", compatProvider, compatModel, providerB, modelB)
			}
		})
	}
}
