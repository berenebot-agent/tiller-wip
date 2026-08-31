package server

import (
	"encoding/csv"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/database"
)

// getCSV performs an authenticated GET (via the harness cookie jar) and returns
// the raw body so tests can parse the CSV attachment.
func getCSV(t *testing.T, api *testAPI, path string) (int, string) {
	t.Helper()
	resp, err := api.client.Get(api.base + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	// Strip the UTF-8 BOM (added for Excel compatibility) so CSV parsing and
	// header comparisons see clean text.
	return resp.StatusCode, strings.TrimPrefix(string(body), "\xEF\xBB\xBF")
}

// insertVirtualLogRow inserts a request_logs row attributable to a virtual
// model (route_kind='virtual', route_model_id set) so tests can control values
// deterministically.
func insertVirtualLogRow(t *testing.T, db *database.DB, id, clientKeyID, virtualID, routeModel, resolvedProvider, resolvedModel, createdAt string) {
	t.Helper()
	_, err := db.SQL.Exec(`INSERT INTO request_logs(id,client_key_id,requested_model,exposed_model,route_kind,route_model_id,route_model,resolved_provider,resolved_model,protocol,streaming,http_status,latency_ms,provider_request_id,client_request_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, clientKeyID, "main", "main", "virtual", virtualID, routeModel, resolvedProvider, resolvedModel, "chat", 0, 200, 10, "upstream-"+id, "req-"+id, createdAt)
	if err != nil {
		t.Fatal(err)
	}
}

func TestClientActivityCSVExport(t *testing.T) {
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	client2ID, _ := createClientWithModel(t, api, "second client")
	insertLogRow(t, db, "row-a", clientID, "provider-a/model-a", strPtr("provider-a"), strPtr("model-a"), "chat", 0, 200, 10, int64Ptr(1), int64Ptr(1), "upstream-a", "req-a", nil, "2026-01-01T00:00:01Z")
	insertLogRow(t, db, "row-b", client2ID, "provider-a/model-a", strPtr("provider-a"), strPtr("model-a"), "chat", 0, 200, 20, int64Ptr(2), int64Ptr(2), "upstream-b", "req-b", nil, "2026-01-01T00:00:02Z")
	insertLogRow(t, db, "row-c", clientID, "provider-a/nope", nil, nil, "chat", 0, 404, 5, nil, nil, "", "req-c", strPtr("model_not_found"), "2026-01-01T00:00:03Z")

	status, body := getCSV(t, api, "/api/admin/client-keys/"+clientID+"/activity/export")
	if status != 200 {
		t.Fatalf("export: %d", status)
	}
	records, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("expected header + 2 rows, got %d records", len(records))
	}
	if records[0][0] != "timestamp" || records[0][1] != "client_key" {
		t.Fatalf("bad header: %v", records[0])
	}
	// Scoped to client1 only, newest first: row-c then row-a.
	if records[1][0] != "2026-01-01T00:00:03Z" || records[2][0] != "2026-01-01T00:00:01Z" {
		t.Fatalf("rows not scoped/newest-first: %v", records)
	}
	if records[1][1] != "test client" {
		t.Fatalf("client name wrong: %v", records[1])
	}
	// Unknown values stay blank (row-c is the 404 with no resolved target).
	if records[1][6] != "" || records[1][7] != "" {
		t.Fatalf("unknown resolved fields not blank: %v", records[1])
	}
	// Search filter is honoured.
	status, body = getCSV(t, api, "/api/admin/client-keys/"+clientID+"/activity/export?search=404")
	if status != 200 {
		t.Fatalf("export search: %d", status)
	}
	records, _ = csv.NewReader(strings.NewReader(body)).ReadAll()
	if len(records) != 2 {
		t.Fatalf("search export should have header + 1 row, got %d", len(records))
	}
}

func TestClientActivityCSVExportRequiresAdmin(t *testing.T) {
	api, _, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	req, _ := http.NewRequest("GET", api.base+"/api/admin/client-keys/"+clientID+"/activity/export", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401 without admin auth, got %d", resp.StatusCode)
	}
}

func TestVirtualActivityCSVExportScopedToModel(t *testing.T) {
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	// Create a virtual model "virtual/coding" targeting model-a.
	status, payload, _ := api.request("GET", "/api/admin/providers", nil)
	if status != 200 {
		t.Fatal(payload)
	}
	var providerID string
	for _, raw := range payload["data"].([]any) {
		m := raw.(map[string]any)
		if m["name"] == "provider-a" {
			providerID = m["id"].(string)
		}
	}
	status, payload, _ = api.request("GET", "/api/admin/providers/"+providerID+"/models", nil)
	if status != 200 {
		t.Fatal(payload)
	}
	var modelID string
	for _, raw := range payload["data"].([]any) {
		m := raw.(map[string]any)
		if m["upstream_model_id"] == "model-a" {
			modelID = m["id"].(string)
		}
	}
	status, payload, _ = api.request("POST", "/api/admin/virtual-groups", map[string]any{"name": "virtual"})
	if status != 201 {
		t.Fatalf("create virtual group: %d %v", status, payload)
	}
	groupID := payload["id"].(string)
	status, payload, _ = api.request("POST", "/api/admin/virtual-models", map[string]any{"group_id": groupID, "name": "coding", "target_provider_id": providerID, "target_model_id": modelID})
	if status != 201 {
		t.Fatalf("create virtual model: %d %v", status, payload)
	}
	virtualID := payload["id"].(string)

	insertVirtualLogRow(t, db, "row-a", clientID, virtualID, "virtual/coding", "provider-a", "model-a", "2026-01-01T00:00:01Z")
	insertVirtualLogRow(t, db, "row-b", clientID, virtualID, "virtual/coding", "provider-a", "model-a", "2026-01-01T00:00:02Z")
	// A row attributable to a different virtual model must be excluded.
	insertVirtualLogRow(t, db, "row-other", clientID, "some-other-id", "virtual/other", "provider-a", "model-a", "2026-01-01T00:00:03Z")

	status, body := getCSV(t, api, "/api/admin/virtual-models/"+virtualID+"/activity/export")
	if status != 200 {
		t.Fatalf("export: %d", status)
	}
	records, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("expected header + 2 rows, got %d", len(records))
	}
	if records[1][4] != "virtual/coding" {
		t.Fatalf("virtual_model column wrong: %v", records[1])
	}
	for _, rec := range records {
		if rec[0] == "2026-01-01T00:00:03Z" {
			t.Fatalf("other virtual's row leaked into export: %v", records)
		}
	}
}

func TestVirtualActivityListScopedToModel(t *testing.T) {
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	status, payload, _ := api.request("GET", "/api/admin/providers", nil)
	if status != 200 {
		t.Fatal(payload)
	}
	var providerID string
	for _, raw := range payload["data"].([]any) {
		m := raw.(map[string]any)
		if m["name"] == "provider-a" {
			providerID = m["id"].(string)
		}
	}
	status, payload, _ = api.request("GET", "/api/admin/providers/"+providerID+"/models", nil)
	if status != 200 {
		t.Fatal(payload)
	}
	var modelID string
	for _, raw := range payload["data"].([]any) {
		m := raw.(map[string]any)
		if m["upstream_model_id"] == "model-a" {
			modelID = m["id"].(string)
		}
	}
	status, payload, _ = api.request("POST", "/api/admin/virtual-groups", map[string]any{"name": "virtual"})
	if status != 201 {
		t.Fatalf("create virtual group: %d %v", status, payload)
	}
	groupID := payload["id"].(string)
	status, payload, _ = api.request("POST", "/api/admin/virtual-models", map[string]any{"group_id": groupID, "name": "coding", "target_provider_id": providerID, "target_model_id": modelID})
	if status != 201 {
		t.Fatalf("create virtual model: %d %v", status, payload)
	}
	virtualID := payload["id"].(string)

	insertVirtualLogRow(t, db, "row-a", clientID, virtualID, "virtual/coding", "provider-a", "model-a", "2026-01-01T00:00:01Z")
	insertVirtualLogRow(t, db, "row-other", clientID, "some-other-id", "virtual/other", "provider-a", "model-a", "2026-01-01T00:00:02Z")

	status, payload, _ = api.request("GET", "/api/admin/virtual-models/"+virtualID+"/activity", nil)
	if status != 200 {
		t.Fatalf("list: %d %v", status, payload)
	}
	data := payload["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["id"] != "row-a" {
		t.Fatalf("virtual activity not scoped: %v", data)
	}
	status, _, _ = api.request("GET", "/api/admin/virtual-models/does-not-exist/activity", nil)
	if status != 404 {
		t.Fatalf("expected 404 for unknown virtual model, got %d", status)
	}
}

// realModelID returns the provider_models row id for the harness's model-a.
func realModelID(t *testing.T, api *testAPI) string {
	t.Helper()
	status, payload, _ := api.request("GET", "/api/admin/providers", nil)
	if status != 200 {
		t.Fatal(payload)
	}
	var providerID string
	for _, raw := range payload["data"].([]any) {
		m := raw.(map[string]any)
		if m["name"] == "provider-a" {
			providerID = m["id"].(string)
		}
	}
	status, payload, _ = api.request("GET", "/api/admin/providers/"+providerID+"/models", nil)
	if status != 200 {
		t.Fatal(payload)
	}
	for _, raw := range payload["data"].([]any) {
		m := raw.(map[string]any)
		if m["upstream_model_id"] == "model-a" {
			return m["id"].(string)
		}
	}
	t.Fatal("mock upstream did not expose model-a")
	return ""
}

// insertRealLogRow inserts a request_logs row attributable to a real model
// (route_kind='real', route_model_id set) so tests can control values
// deterministically.
func insertRealLogRow(t *testing.T, db *database.DB, id, clientKeyID, realModelID, routeModel, resolvedProvider, resolvedModel, createdAt string) {
	t.Helper()
	_, err := db.SQL.Exec(`INSERT INTO request_logs(id,client_key_id,requested_model,exposed_model,route_kind,route_model_id,route_model,resolved_provider,resolved_model,protocol,streaming,http_status,latency_ms,provider_request_id,client_request_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, clientKeyID, routeModel, routeModel, "real", realModelID, routeModel, resolvedProvider, resolvedModel, "chat", 0, 200, 10, "upstream-"+id, "req-"+id, createdAt)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRealModelActivityCSVExportScopedToModel(t *testing.T) {
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	modelID := realModelID(t, api)
	insertRealLogRow(t, db, "row-a", clientID, modelID, "provider-a/model-a", "provider-a", "model-a", "2026-01-01T00:00:01Z")
	insertRealLogRow(t, db, "row-b", clientID, modelID, "provider-a/model-a", "provider-a", "model-a", "2026-01-01T00:00:02Z")
	// A row attributable to a different real model must be excluded.
	insertRealLogRow(t, db, "row-other", clientID, "some-other-id", "provider-a/other", "provider-a", "other", "2026-01-01T00:00:03Z")

	status, body := getCSV(t, api, "/api/admin/models/"+modelID+"/activity/export")
	if status != 200 {
		t.Fatalf("export: %d", status)
	}
	records, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("expected header + 2 rows, got %d", len(records))
	}
	if records[1][5] != "provider-a/model-a" {
		t.Fatalf("bound_target column wrong: %v", records[1])
	}
	for _, rec := range records {
		if rec[0] == "2026-01-01T00:00:03Z" {
			t.Fatalf("other model's row leaked into export: %v", records)
		}
	}
}

func TestRealModelActivityListScopedToModel(t *testing.T) {
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	modelID := realModelID(t, api)
	insertRealLogRow(t, db, "row-a", clientID, modelID, "provider-a/model-a", "provider-a", "model-a", "2026-01-01T00:00:01Z")
	insertRealLogRow(t, db, "row-other", clientID, "some-other-id", "provider-a/other", "provider-a", "other", "2026-01-01T00:00:02Z")

	status, payload, _ := api.request("GET", "/api/admin/models/"+modelID+"/activity", nil)
	if status != 200 {
		t.Fatalf("list: %d %v", status, payload)
	}
	data := payload["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["id"] != "row-a" {
		t.Fatalf("real model activity not scoped: %v", data)
	}
	status, _, _ = api.request("GET", "/api/admin/models/does-not-exist/activity", nil)
	if status != 404 {
		t.Fatalf("expected 404 for unknown model, got %d", status)
	}
}

// TestRealModelActivityIncludesLegacyAndVirtualRoutedRows verifies that a row
// with NULL route_kind (legacy) or a virtual route that resolved to a real model
// still appears in that real model's activity.
func TestRealModelActivityIncludesLegacyAndVirtualRoutedRows(t *testing.T) {
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	modelID := realModelID(t, api)
	// Legacy row: route_kind NULL, resolved to model-a.
	insertLogRow(t, db, "row-legacy", clientID, "main/hermes-daily", strPtr("provider-a"), strPtr("model-a"), "chat", 0, 200, 10, int64Ptr(1), int64Ptr(1), "upstream-a", "req-legacy", nil, "2026-01-01T00:00:01Z")
	// Virtual-routed row: route_kind='virtual', resolved to model-a.
	insertVirtualLogRow(t, db, "row-virtual", clientID, "some-virtual-id", "main/hermes-daily", "provider-a", "model-a", "2026-01-01T00:00:02Z")

	status, payload, _ := api.request("GET", "/api/admin/models/"+modelID+"/activity", nil)
	if status != 200 {
		t.Fatalf("list: %d %v", status, payload)
	}
	data := payload["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("expected 2 rows (legacy + virtual-routed), got %d: %v", len(data), data)
	}
}

// TestVirtualModelActivityIncludesLegacyRows verifies a legacy row (route_kind
// NULL) that requested the virtual model by canonical name still appears.
func TestVirtualModelActivityIncludesLegacyRows(t *testing.T) {
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	status, payload, _ := api.request("GET", "/api/admin/providers", nil)
	if status != 200 {
		t.Fatal(payload)
	}
	var providerID string
	for _, raw := range payload["data"].([]any) {
		m := raw.(map[string]any)
		if m["name"] == "provider-a" {
			providerID = m["id"].(string)
		}
	}
	status, payload, _ = api.request("GET", "/api/admin/providers/"+providerID+"/models", nil)
	if status != 200 {
		t.Fatal(payload)
	}
	var modelID string
	for _, raw := range payload["data"].([]any) {
		m := raw.(map[string]any)
		if m["upstream_model_id"] == "model-a" {
			modelID = m["id"].(string)
		}
	}
	status, payload, _ = api.request("POST", "/api/admin/virtual-groups", map[string]any{"name": "virtual"})
	if status != 201 {
		t.Fatalf("create virtual group: %d %v", status, payload)
	}
	groupID := payload["id"].(string)
	status, payload, _ = api.request("POST", "/api/admin/virtual-models", map[string]any{"group_id": groupID, "name": "coding", "target_provider_id": providerID, "target_model_id": modelID})
	if status != 201 {
		t.Fatalf("create virtual model: %d %v", status, payload)
	}
	virtualID := payload["id"].(string)

	// Legacy row: route_kind NULL, requested the virtual model by canonical name.
	insertLogRow(t, db, "row-legacy", clientID, "virtual/coding", strPtr("provider-a"), strPtr("model-a"), "chat", 0, 200, 10, int64Ptr(1), int64Ptr(1), "upstream-a", "req-legacy", nil, "2026-01-01T00:00:01Z")

	status, payload, _ = api.request("GET", "/api/admin/virtual-models/"+virtualID+"/activity", nil)
	if status != 200 {
		t.Fatalf("list: %d %v", status, payload)
	}
	data := payload["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["id"] != "row-legacy" {
		t.Fatalf("legacy virtual row not found: %v", data)
	}
}

func TestActivityCSVExportNoSensitiveMaterial(t *testing.T) {
	api, _, clientID, secret := loggingTestHarness(t, mockUpstream(t))
	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "provider-a/model-a", "messages": []any{map[string]any{"role": "user", "content": "PROMPT-SECRET-MARKER"}}})
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("request status %d", resp.StatusCode)
	}
	status, body := getCSV(t, api, "/api/admin/client-keys/"+clientID+"/activity/export")
	if status != 200 {
		t.Fatalf("export: %d", status)
	}
	for _, forbidden := range []string{"PROMPT-SECRET-MARKER", "provider-secret", "Bearer", "sk-tr-", "Authorization", "tool", "reasoning"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("sensitive material leaked into CSV: %q", forbidden)
		}
	}
}
