package server

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
)

// manualModelUpstream returns model-a on the first discovery (provider create)
// and adds model-meta on later probes, so model-meta exists upstream but is not
// in the persisted catalogue — the case manual add exists to cover.
func manualModelUpstream() http.HandlerFunc {
	var mu sync.Mutex
	calls := 0
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		data := []any{map[string]any{"id": "model-a"}}
		if !first {
			data = append(data, map[string]any{"id": "model-meta", "context_length": 4096, "max_output_tokens": 512})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}
}

func testProviderID(t *testing.T, api *testAPI) string {
	t.Helper()
	var providerID string
	if err := api.server.db.SQL.QueryRow(`SELECT id FROM providers WHERE name='provider-a'`).Scan(&providerID); err != nil {
		t.Fatal(err)
	}
	return providerID
}

func TestManualModelLookupAddAndDelete(t *testing.T) {
	api, db, _, _ := loggingTestHarness(t, manualModelUpstream())
	providerID := testProviderID(t, api)

	status, payload, _ := api.request("GET", "/api/admin/providers/"+providerID+"/models/lookup?upstream_model_id=model-meta", nil)
	if status != 200 {
		t.Fatalf("lookup: %d %v", status, payload)
	}
	if payload["context_length"].(float64) != 4096 || payload["max_output_tokens"].(float64) != 512 {
		t.Fatalf("lookup metadata = %v", payload)
	}

	status, payload, _ = api.request("POST", "/api/admin/providers/"+providerID+"/models", map[string]any{"upstream_model_id": "model-meta"})
	if status != 201 {
		t.Fatalf("add manual model: %d %v", status, payload)
	}
	manualID := payload["id"].(string)

	status, payload, _ = api.request("GET", "/api/admin/models?all=1&search=model-meta", nil)
	if status != 200 {
		t.Fatalf("list models: %d %v", status, payload)
	}
	found := false
	for _, raw := range payload["data"].([]any) {
		model := raw.(map[string]any)
		if model["id"] == manualID {
			found = true
			if model["origin"] != "manual" {
				t.Fatalf("origin = %v, want manual", model["origin"])
			}
			if model["context_length"].(float64) != 4096 {
				t.Fatalf("detected context not persisted: %v", model["context_length"])
			}
		}
	}
	if !found {
		t.Fatalf("manual model %s missing from catalogue", manualID)
	}

	status, _, _ = api.request("POST", "/api/admin/providers/"+providerID+"/models", map[string]any{"upstream_model_id": "model-meta"})
	if status != 409 {
		t.Fatalf("duplicate add status = %d, want 409", status)
	}

	var discoveredID string
	if err := db.SQL.QueryRow(`SELECT id FROM provider_models WHERE provider_id=? AND upstream_model_id='model-a'`, providerID).Scan(&discoveredID); err != nil {
		t.Fatal(err)
	}
	status, _, _ = api.request("DELETE", "/api/admin/models/"+discoveredID, nil)
	if status != 403 {
		t.Fatalf("deleting discovered model status = %d, want 403", status)
	}

	status, _, _ = api.request("DELETE", "/api/admin/models/"+manualID, nil)
	if status != 204 {
		t.Fatalf("delete manual model status = %d, want 204", status)
	}
	status, _, _ = api.request("DELETE", "/api/admin/models/"+manualID, nil)
	if status != 404 {
		t.Fatalf("delete missing model status = %d, want 404", status)
	}
}

func TestManualModelValidation(t *testing.T) {
	api, _, _, _ := loggingTestHarness(t, manualModelUpstream())
	providerID := testProviderID(t, api)

	status, _, _ := api.request("POST", "/api/admin/providers/"+providerID+"/models", map[string]any{})
	if status != 400 {
		t.Fatalf("empty model id status = %d, want 400", status)
	}
	status, _, _ = api.request("POST", "/api/admin/providers/"+providerID+"/models", map[string]any{"upstream_model_id": "x", "native_protocol": "bogus"})
	if status != 400 {
		t.Fatalf("invalid protocol status = %d, want 400", status)
	}
	status, _, _ = api.request("POST", "/api/admin/providers/missing/models", map[string]any{"upstream_model_id": "x"})
	if status != 404 {
		t.Fatalf("unknown provider status = %d, want 404", status)
	}
	status, _, _ = api.request("GET", "/api/admin/providers/"+providerID+"/models/lookup", nil)
	if status != 400 {
		t.Fatalf("lookup without id status = %d, want 400", status)
	}
}
