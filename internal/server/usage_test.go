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

func TestUsageCacheHitWindows(t *testing.T) {
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	now := time.Now().UTC()
	type row struct {
		in, out, cacheRead int64
		hasCache           bool
	}
	// Three real-model groups resolved to the same target:
	//  - cache-valid: cache_read present (e.g. Anthropic reporting cache).
	//  - zero-cache:  cache_read present but 0 (reported cache, no hits).
	//  - no-cache:    cache_read absent (e.g. Ollama — not reported).
	rows := []row{
		{in: 1000, out: 100, cacheRead: 900, hasCache: true},  // 90% hit
		{in: 1000, out: 100, cacheRead: 500, hasCache: true},  // 50% hit
		{in: 1000, out: 100, cacheRead: 0, hasCache: true},    // 0% hit (real)
		{in: 1000, out: 100, cacheRead: 0, hasCache: false},   // missing -> n.a.
	}
	for i, r := range rows {
		created := now.Add(-10 * time.Minute).Format(time.RFC3339Nano)
		resolvedModel := "model-a"
		req := "provider-a/model-a"
		var cacheRead any
		if !r.hasCache {
			req = "provider-a/model-b"
			resolvedModel = "model-b"
		} else {
			cacheRead = r.cacheRead
		}
		if _, err := db.SQL.Exec(`INSERT INTO request_logs(id,client_key_id,requested_model,resolved_provider,resolved_model,protocol,streaming,http_status,latency_ms,input_tokens,output_tokens,cache_read_input_tokens,client_request_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			fmt.Sprintf("cache%d", i), clientID, req, "provider-a", resolvedModel, "chat", 0, 200, 1, r.in, r.out, cacheRead, "req-c", created); err != nil {
			t.Fatal(err)
		}
	}
	status, payload, _ := api.request("GET", "/api/admin/usage", nil)
	if status != 200 {
		t.Fatalf("usage: %d %v", status, payload)
	}
	// Mixed window: cache-valid rows (90%, 50%, 0%) blend to
	// (900+500+0)/(1000+1000+1000) = 1400/3000 ≈ 46.67%. n.a. rows are dropped.
	if v, ok := payload["real_cache"].(map[string]any)["provider-a/model-a"].(map[string]any); !ok {
		t.Fatalf("expected real_cache for provider-a/model-a, got %v", payload["real_cache"])
	} else if v["1h"] == nil {
		t.Fatalf("expected non-nil cache %% for blended window, got %v", v)
	} else if p := v["1h"].(float64); p < 46.4 || p > 47.0 {
		t.Fatalf("blended cache %% wrong: %.3f (want ~46.67)", p)
	}
	// n.a. window: only non-reporting rows -> null.
	if v, ok := payload["real_cache"].(map[string]any)["provider-a/model-b"].(map[string]any); !ok {
		t.Fatalf("expected real_cache for provider-a/model-b, got %v", payload["real_cache"])
	} else if v["1h"] != nil {
		t.Fatalf("expected null (n.a.) cache %% for non-reporting window, got %v", v)
	}
}
