package server

import (
	"bytes"
	"context"
	"encoding/csv"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

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

func TestClientActivityCSVExportPeriodFilter(t *testing.T) {
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	now := time.Now().UTC()
	insertLogRow(t, db, "row-old", clientID, "provider-a/model-a", strPtr("provider-a"), strPtr("model-a"), "chat", 0, 200, 10, int64Ptr(1), int64Ptr(1), "upstream-a", "req-old", nil, now.Add(-40*24*time.Hour).Format(time.RFC3339Nano))
	insertLogRow(t, db, "row-recent", clientID, "provider-a/model-a", strPtr("provider-a"), strPtr("model-a"), "chat", 0, 200, 10, int64Ptr(1), int64Ptr(1), "upstream-a", "req-recent", nil, now.Add(-time.Hour).Format(time.RFC3339Nano))

	// 24h excludes the 40-day-old row.
	status, body := getCSV(t, api, "/api/admin/client-keys/"+clientID+"/activity/export?period=24h")
	if status != 200 {
		t.Fatalf("export: %d", status)
	}
	records, _ := csv.NewReader(strings.NewReader(body)).ReadAll()
	if len(records) != 2 { // header + 1 row
		t.Fatalf("24h export should have header + 1 row, got %d", len(records))
	}
	// all includes both.
	status, body = getCSV(t, api, "/api/admin/client-keys/"+clientID+"/activity/export?period=all")
	if status != 200 {
		t.Fatalf("export: %d", status)
	}
	records, _ = csv.NewReader(strings.NewReader(body)).ReadAll()
	if len(records) != 3 { // header + 2 rows
		t.Fatalf("all export should have header + 2 rows, got %d", len(records))
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

// providerAndModelIDs returns the harness provider id and its model-a id.
func providerAndModelIDs(t *testing.T, api *testAPI) (string, string) {
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
			return providerID, m["id"].(string)
		}
	}
	t.Fatal("mock upstream did not expose model-a")
	return "", ""
}

// TestWriteLogInvariant verifies the write-time invariant guard: a 2xx row
// written with NULL resolved_provider/resolved_model is still persisted
// (best-effort, never fails the request) but triggers a warning so a future
// code path that forgets to set the resolved target surfaces early.
func TestWriteLogInvariant(t *testing.T) {
	_, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	s := &Server{db: db, logger: logger}
	// A 2xx row with NULL resolved target must still be written but must warn.
	s.writeLog(context.Background(), &logRow{clientKeyID: clientID, clientRequestID: "req-invariant", requestedModel: "provider-a/model-a", protocol: "chat", httpStatus: 200, latencyMs: 1, createdAt: database.Now()})
	var count int
	if err := db.SQL.QueryRow(`SELECT count(*) FROM request_logs WHERE id='req-invariant'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("2xx row with NULL resolved was not written: count=%d err=%v", count, err)
	}
	if !strings.Contains(buf.String(), "resolved") {
		t.Fatalf("expected invariant warning, got log: %q", buf.String())
	}
	// A 2xx row WITH a resolved target must not warn.
	buf.Reset()
	s.writeLog(context.Background(), &logRow{clientKeyID: clientID, clientRequestID: "req-ok", requestedModel: "provider-a/model-a", protocol: "chat", httpStatus: 200, latencyMs: 1, resolvedProvider: strPtr("provider-a"), resolvedModel: strPtr("model-a"), createdAt: database.Now()})
	if strings.Contains(buf.String(), "resolved") {
		t.Fatalf("unexpected warning for resolved 2xx row: %q", buf.String())
	}
}

// TestAttributionHelperConsistency guards against drift between list, CSV
// export, and usage: for a given virtual model they must all attribute the same
// set of rows (new route_kind='virtual' rows plus legacy NULL-route rows).
func TestAttributionHelperConsistency(t *testing.T) {
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	providerID, modelID := providerAndModelIDs(t, api)
	status, payload, _ := api.request("POST", "/api/admin/virtual-groups", map[string]any{"name": "virtual"})
	if status != 201 {
		t.Fatal(payload)
	}
	groupID := payload["id"].(string)
	status, payload, _ = api.request("POST", "/api/admin/virtual-models", map[string]any{"group_id": groupID, "name": "coding", "target_provider_id": providerID, "target_model_id": modelID})
	if status != 201 {
		t.Fatalf("create virtual model: %d %v", status, payload)
	}
	virtualID := payload["id"].(string)

	now := time.Now().UTC()
	// One new row (route_kind='virtual') and two legacy rows (route_kind NULL,
	// requested by canonical) attributable to virtual/coding; one row for a
	// different virtual model that must be excluded everywhere.
	insertVirtualLogRow(t, db, "row-new", clientID, virtualID, "virtual/coding", "provider-a", "model-a", now.Add(-time.Minute).Format(time.RFC3339Nano))
	insertLogRow(t, db, "row-legacy-1", clientID, "virtual/coding", strPtr("provider-a"), strPtr("model-a"), "chat", 0, 200, 10, int64Ptr(100), int64Ptr(0), "up-a", "req-l1", nil, now.Add(-2*time.Minute).Format(time.RFC3339Nano))
	insertLogRow(t, db, "row-legacy-2", clientID, "virtual/coding", strPtr("provider-a"), strPtr("model-a"), "chat", 0, 200, 10, int64Ptr(200), int64Ptr(0), "up-b", "req-l2", nil, now.Add(-3*time.Minute).Format(time.RFC3339Nano))
	insertVirtualLogRow(t, db, "row-other", clientID, "some-other-id", "virtual/other", "provider-a", "model-a", now.Add(-4*time.Minute).Format(time.RFC3339Nano))

	// List.
	status, payload, _ = api.request("GET", "/api/admin/virtual-models/"+virtualID+"/activity?limit=50", nil)
	if status != 200 {
		t.Fatalf("list: %d %v", status, payload)
	}
	if rows := payload["data"].([]any); len(rows) != 3 {
		t.Fatalf("list: expected 3 rows, got %d: %v", len(rows), rows)
	}

	// CSV export.
	status, body := getCSV(t, api, "/api/admin/virtual-models/"+virtualID+"/activity/export")
	if status != 200 {
		t.Fatalf("export: %d", status)
	}
	records, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 4 { // header + 3 rows
		t.Fatalf("export: expected header + 3 rows, got %d", len(records))
	}

	// Usage: virtual/coding totals 300 tokens (100+200) in the 1h window.
	status, payload, _ = api.request("GET", "/api/admin/usage", nil)
	if status != 200 {
		t.Fatalf("usage: %d %v", status, payload)
	}
	vm := payload["virtual_models"].(map[string]any)["virtual/coding"].(map[string]any)
	if vm["1h"] != float64(300) {
		t.Fatalf("usage 1h: expected 300, got %v", vm["1h"])
	}
}

// TestNeutralizeCSVField guards against spreadsheet formula injection: a leading
// formula prefix must be escaped with a single quote so the cell is not treated
// as a formula when opened in a spreadsheet.
func TestNeutralizeCSVField(t *testing.T) {
	for _, prefix := range []string{"=", "+", "-", "@", "\t", "\r"} {
		got := neutralizeCSVField(prefix + "cmd|' /C calc'!A0")
		if !strings.HasPrefix(got, "'") {
			t.Fatalf("prefix %q not neutralized: %q", prefix, got)
		}
	}
	// Non-formula values and empty strings are untouched.
	if got := neutralizeCSVField("provider-a/model-a"); got != "provider-a/model-a" {
		t.Fatalf("plain value altered: %q", got)
	}
	if got := neutralizeCSVField(""); got != "" {
		t.Fatalf("empty value altered: %q", got)
	}
}

// TestActivityCSVExportNeutralizesFormulaModel verifies end-to-end that a
// client-chosen model beginning with a formula prefix is neutralized in the
// admin CSV export.
func TestActivityCSVExportNeutralizesFormulaModel(t *testing.T) {
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	// A client-chosen model string that would otherwise execute as a formula.
	insertLogRow(t, db, "row-formula", clientID, "=1+1", strPtr("provider-a"), strPtr("model-a"), "chat", 0, 200, 10, int64Ptr(1), int64Ptr(1), "upstream-a", "req-a", nil, "2026-01-01T00:00:01Z")

	status, body := getCSV(t, api, "/api/admin/client-keys/"+clientID+"/activity/export")
	if status != 200 {
		t.Fatalf("export: %d", status)
	}
	records, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	// client_requested_model is column index 2.
	if got := records[1][2]; !strings.HasPrefix(got, "'") {
		t.Fatalf("formula-prefixed model not neutralized in export: %q", got)
	}
}

func TestActivityCSVExportHeaderRowAlignment(t *testing.T) {
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	if _, err := db.SQL.Exec(`UPDATE client_keys SET name=? WHERE id=?`, "align-client", clientID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO request_logs(id,client_key_id,requested_model,exposed_model,route_kind,route_model_id,route_model,resolved_provider,resolved_model,protocol,streaming,http_status,latency_ms,input_tokens,output_tokens,cache_read_input_tokens,cache_creation_input_tokens,provider_request_id,client_request_id,error_message,request_body,request_body_truncated,error_body,error_body_truncated,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"row-align", clientID, "main", "main", "virtual", "vm-1", "vm-1/main", "prov-a", "model-a", "chat", 1, 200, 10, int64Ptr(5), int64Ptr(3), int64Ptr(1), int64Ptr(2), "upstream-align", "req-align", "boom", "the request body", 1, "the error body", 1, "2026-01-01T00:00:01Z"); err != nil {
		t.Fatal(err)
	}

	status, body := getCSV(t, api, "/api/admin/client-keys/"+clientID+"/activity/export")
	if status != 200 {
		t.Fatalf("export: %d", status)
	}
	records, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("expected header + 1 row, got %d records", len(records))
	}
	header := records[0]
	if len(header) != 28 {
		t.Fatalf("expected 28 columns, got %d", len(header))
	}
	row := records[1]

	// Every value must land under its own header. Build a header->index map and
	// assert each value at that index matches the expected source.
	indexOf := func(name string) int {
		for i, h := range header {
			if h == name {
				return i
			}
		}
		t.Fatalf("header %q not found", name)
		return -1
	}

	checks := []struct {
		header string
		value  string
	}{
		{"timestamp", "2026-01-01T00:00:01Z"},
		{"client_key", "align-client"},
		{"client_requested_model", "main"},
		{"client_exposed_model", "main"},
		{"virtual_model", "vm-1/main"},
		{"bound_target", "vm-1/main"},
		{"final_provider", "prov-a"},
		{"final_model", "model-a"},
		{"protocol", "chat"},
		{"streaming", "true"},
		{"http_status", "200"},
		{"latency_ms", "10"},
		{"input_tokens", "5"},
		{"output_tokens", "3"},
		{"cached_input_tokens", "1"},
		{"cache_creation_input_tokens", "2"},
		{"attempt_count", "1"},
		{"fallback_used", "false"},
		{"fallback_reason", ""},
		{"error_message", "boom"},
		{"provider_request_id", "upstream-align"},
		{"client_request_id", "req-align"},
		{"route_kind", "virtual"},
	}
	for _, c := range checks {
		if got := row[indexOf(c.header)]; got != c.value {
			t.Errorf("column %q: want %q, got %q", c.header, c.value, got)
		}
	}
}

func TestActivityExposesCacheCreationTokensAndEmptyExport(t *testing.T) {
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	insertLogRow(t, db, "row-cache-creation", clientID, "provider-a/model-a", strPtr("provider-a"), strPtr("model-a"), "chat", 0, 200, 10, int64Ptr(10), int64Ptr(2), "upstream-cache", "req-cache", nil, "2026-01-01T00:00:01Z")
	if _, err := db.SQL.Exec(`UPDATE request_logs SET cache_creation_input_tokens=42 WHERE id=?`, "row-cache-creation"); err != nil {
		t.Fatal(err)
	}
	status, payload, _ := api.request("GET", "/api/admin/client-keys/"+clientID+"/activity", nil)
	if status != 200 {
		t.Fatalf("activity: %d %v", status, payload)
	}
	row := payload["data"].([]any)[0].(map[string]any)
	if row["cache_creation_input_tokens"] != float64(42) {
		t.Fatalf("cache creation tokens missing from activity JSON: %v", row)
	}
	status, body := getCSV(t, api, "/api/admin/client-keys/"+clientID+"/activity/export")
	if status != 200 {
		t.Fatalf("export: %d", status)
	}
	records, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	var column int
	for i, name := range records[0] {
		if name == "cache_creation_input_tokens" {
			column = i
		}
	}
	if records[1][column] != "42" {
		t.Fatalf("cache creation tokens missing from CSV: %v", records[1])
	}

	// A client with no rows still receives a valid header-only export and a
	// readable, client-derived filename.
	emptyID, _ := createClientWithModel(t, api, "empty-export-client")
	resp, err := api.client.Get(api.base + "/api/admin/client-keys/" + emptyID + "/activity/export")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	emptyBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Disposition"), "empty-export-client") {
		t.Fatalf("empty export response: status=%d disposition=%q", resp.StatusCode, resp.Header.Get("Content-Disposition"))
	}
	emptyRecords, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(emptyBody), "\xEF\xBB\xBF"))).ReadAll()
	if err != nil || len(emptyRecords) != 1 {
		t.Fatalf("empty export should contain only headers: records=%v err=%v", emptyRecords, err)
	}
}
