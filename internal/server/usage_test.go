package server

import (
	"fmt"
	"testing"
	"time"
)

func TestUsageEndpointPerWindowTotals(t *testing.T) {
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	now := time.Now().UTC()
	// Insert rows at controlled times with known token totals.
	// - 30 min ago: 100k tokens  -> in 1h, 24h, 7d
	// - 2 hours ago: 200k tokens -> in 24h, 7d (not 1h)
	// - 3 days ago: 300k tokens  -> in 7d (not 1h/24h)
	// - 10 days ago: 400k tokens -> in none
	rows := []struct {
		ago   time.Duration
		total int64
	}{
		{30 * time.Minute, 100000},
		{2 * time.Hour, 200000},
		{3 * 24 * time.Hour, 300000},
		{10 * 24 * time.Hour, 400000},
	}
	for i, r := range rows {
		created := now.Add(-r.ago).Format(time.RFC3339Nano)
		in := r.total / 2
		out := r.total - in
		if _, err := db.SQL.Exec(`INSERT INTO request_logs(id,client_key_id,requested_model,resolved_provider,resolved_model,protocol,streaming,http_status,latency_ms,input_tokens,output_tokens,client_request_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			fmt.Sprintf("row%d", i), clientID, "provider-a/model-a", "provider-a", "model-a", "chat", 0, 200, 1, in, out, "req-x", created); err != nil {
			t.Fatal(err)
		}
	}
	status, payload, _ := api.request("GET", "/api/admin/usage", nil)
	if status != 200 {
		t.Fatalf("usage: %d %v", status, payload)
	}
	ck := payload["client_keys"].(map[string]any)[clientID].(map[string]any)
	if ck["1h"] != float64(100000) {
		t.Fatalf("1h client usage wrong: %v", ck)
	}
	if ck["24h"] != float64(300000) {
		t.Fatalf("24h client usage wrong: %v", ck)
	}
	if ck["7d"] != float64(600000) {
		t.Fatalf("7d client usage wrong: %v", ck)
	}
	vm := payload["virtual_models"].(map[string]any)["provider-a/model-a"].(map[string]any)
	if vm["7d"] != float64(600000) {
		t.Fatalf("virtual usage wrong: %v", vm)
	}
	rm := payload["real_models"].(map[string]any)["provider-a/model-a"].(map[string]any)
	if rm["7d"] != float64(600000) {
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
