package server

import (
	"fmt"
	"testing"
	"time"
)

func TestUsageEndpointPerWindowTotals(t *testing.T) {
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	// Create a real virtual group + virtual model so the virtual request's
	// requested_model matches a real canonical ID (virtual/coding).
	var providerID, modelID string
	if err := db.SQL.QueryRow(`SELECT id FROM providers WHERE name='provider-a'`).Scan(&providerID); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL.QueryRow(`SELECT id FROM provider_models WHERE upstream_model_id='model-a'`).Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	status, payload, _ := api.request("POST", "/api/admin/virtual-groups", map[string]any{"name": "virtual"})
	if status != 201 {
		t.Fatalf("create virtual group: %d %v", status, payload)
	}
	groupID := payload["id"].(string)
	status, payload, _ = api.request("POST", "/api/admin/virtual-models", map[string]any{"group_id": groupID, "name": "coding", "target_provider_id": providerID, "target_model_id": modelID})
	if status != 201 {
		t.Fatalf("create virtual model: %d %v", status, payload)
	}
	now := time.Now().UTC()
	// Insert rows at controlled times with known token totals.
	// Direct-real rows (requested_model = provider-a/model-a):
	// - 30 min ago: 100k tokens  -> in 1h, 24h, 7d
	// - 2 hours ago: 200k tokens -> in 24h, 7d (not 1h)
	// - 3 days ago: 300k tokens  -> in 7d (not 1h/24h)
	// - 10 days ago: 400k tokens -> in none
	// Virtual rows (requested_model = virtual/coding, resolved to provider-a/model-a):
	// - 30 min ago: 50k tokens  -> in 1h, 24h, 7d
	// - 2 hours ago: 100k tokens -> in 24h, 7d (not 1h)
	rows := []struct {
		ago   time.Duration
		total int64
		kind  string // "direct" or "virtual"
	}{
		{30 * time.Minute, 100000, "direct"},
		{2 * time.Hour, 200000, "direct"},
		{3 * 24 * time.Hour, 300000, "direct"},
		{10 * 24 * time.Hour, 400000, "direct"},
		{30 * time.Minute, 50000, "virtual"},
		{2 * time.Hour, 100000, "virtual"},
	}
	for i, r := range rows {
		created := now.Add(-r.ago).Format(time.RFC3339Nano)
		in := r.total / 2
		out := r.total - in
		requested := "provider-a/model-a"
		if r.kind == "virtual" {
			requested = "virtual/coding"
		}
		if _, err := db.SQL.Exec(`INSERT INTO request_logs(id,client_key_id,requested_model,resolved_provider,resolved_model,protocol,streaming,http_status,latency_ms,input_tokens,output_tokens,client_request_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			fmt.Sprintf("row%d", i), clientID, requested, "provider-a", "model-a", "chat", 0, 200, 1, in, out, "req-x", created); err != nil {
			t.Fatal(err)
		}
	}
	status, payload, _ = api.request("GET", "/api/admin/usage", nil)
	if status != 200 {
		t.Fatalf("usage: %d %v", status, payload)
	}
	// Client totals include both direct and virtual traffic.
	ck := payload["client_keys"].(map[string]any)[clientID].(map[string]any)
	if ck["1h"] != float64(150000) {
		t.Fatalf("1h client usage wrong: %v", ck)
	}
	if ck["24h"] != float64(450000) {
		t.Fatalf("24h client usage wrong: %v", ck)
	}
	if ck["7d"] != float64(750000) {
		t.Fatalf("7d client usage wrong: %v", ck)
	}
	// The direct real ID must be absent from virtual_models.
	vm := payload["virtual_models"].(map[string]any)
	if _, ok := vm["provider-a/model-a"]; ok {
		t.Fatalf("direct real ID misclassified as virtual: %v", vm)
	}
	// The virtual ID has only the virtual request's totals.
	v := vm["virtual/coding"].(map[string]any)
	if v["1h"] != float64(50000) || v["24h"] != float64(150000) || v["7d"] != float64(150000) {
		t.Fatalf("virtual usage wrong: %v", v)
	}
	// The resolved real target includes both direct and virtual traffic.
	rm := payload["real_models"].(map[string]any)["provider-a/model-a"].(map[string]any)
	if rm["1h"] != float64(150000) || rm["24h"] != float64(450000) || rm["7d"] != float64(750000) {
		t.Fatalf("real usage wrong: %v", rm)
	}
}

func TestUsageEndpointEmpty(t *testing.T) {
	api, _, _, _ := loggingTestHarness(t, mockUpstream(t))
	status, payload, _ := api.request("GET", "/api/admin/usage", nil)
	if status != 200 {
		t.Fatalf("usage: %d %v", status, payload)
	}
	if len(payload["client_keys"].(map[string]any)) != 0 || len(payload["virtual_models"].(map[string]any)) != 0 || len(payload["real_models"].(map[string]any)) != 0 {
		t.Fatalf("expected empty usage, got %v", payload)
	}
}
