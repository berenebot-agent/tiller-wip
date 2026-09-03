package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/database"
)

// createClientWithModel creates a client key and grants it access to the
// harness's provider-a/model-a real model, returning the client id and secret.
func createClientWithModel(t *testing.T, api *testAPI, name string) (string, string) {
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
	var modelID string
	for _, raw := range payload["data"].([]any) {
		m := raw.(map[string]any)
		if m["upstream_model_id"] == "model-a" {
			modelID = m["id"].(string)
		}
	}
	if modelID == "" {
		t.Fatal("mock upstream did not expose model-a")
	}
	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": name, "description": "global activity", "type": "catalogue"})
	if status != 201 {
		t.Fatalf("create key: %d %v", status, payload)
	}
	clientID := payload["id"].(string)
	clientSecret := payload["secret"].(string)
	status, payload, _ = api.request("PUT", "/api/admin/client-keys/"+clientID+"/permissions", map[string]any{"defaults": []any{}, "permissions": []any{map[string]any{"kind": "real", "model_id": modelID, "enabled": true}}})
	if status != 204 {
		t.Fatalf("permissions: %d %v", status, payload)
	}
	return clientID, clientSecret
}

// insertLogRow inserts a request_logs row directly so tests can control field
// values and created_at ordering deterministically.
func insertLogRow(t *testing.T, db *database.DB, id, clientKeyID, requestedModel string, resolvedProvider, resolvedModel *string, protocol string, streaming int, httpStatus int, latencyMs int64, inputTokens, outputTokens *int64, providerRequestID, clientRequestID string, errorText *string, createdAt string) {
	t.Helper()
	_, err := db.SQL.Exec(`INSERT INTO request_logs(id,client_key_id,requested_model,resolved_provider,resolved_model,protocol,streaming,http_status,latency_ms,input_tokens,output_tokens,provider_request_id,client_request_id,error_text,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, clientKeyID, requestedModel, resolvedProvider, resolvedModel, protocol, streaming, httpStatus, latencyMs, inputTokens, outputTokens, providerRequestID, clientRequestID, errorText, createdAt)
	if err != nil {
		t.Fatal(err)
	}
}

func TestGlobalActivityRequiresAdmin(t *testing.T) {
	api, _, _, _ := loggingTestHarness(t, mockUpstream(t))
	// A request without an admin session cookie must be rejected.
	req, _ := http.NewRequest("GET", api.base+"/api/admin/activity", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401 without admin auth, got %d", resp.StatusCode)
	}
}

func TestGlobalActivityMultipleClientsNewestFirst(t *testing.T) {
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	client2ID, _ := createClientWithModel(t, api, "second client")
	// Insert rows with distinct created_at so ordering is deterministic.
	insertLogRow(t, db, "row-a", clientID, "provider-a/model-a", strPtr("provider-a"), strPtr("model-a"), "chat", 0, 200, 10, int64Ptr(1), int64Ptr(1), "upstream-a", "req-a", nil, "2026-01-01T00:00:01Z")
	insertLogRow(t, db, "row-b", client2ID, "provider-a/model-a", strPtr("provider-a"), strPtr("model-a"), "chat", 0, 200, 20, int64Ptr(2), int64Ptr(2), "upstream-b", "req-b", nil, "2026-01-01T00:00:02Z")
	insertLogRow(t, db, "row-c", clientID, "provider-a/model-a", strPtr("provider-a"), strPtr("model-a"), "chat", 0, 200, 30, int64Ptr(3), int64Ptr(3), "upstream-c", "req-c", nil, "2026-01-01T00:00:03Z")

	status, payload, _ := api.request("GET", "/api/admin/activity", nil)
	if status != 200 {
		t.Fatalf("activity: %d %v", status, payload)
	}
	data := payload["data"].([]any)
	if len(data) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(data))
	}
	// Newest first: row-c, row-b, row-a.
	if data[0].(map[string]any)["id"] != "row-c" || data[1].(map[string]any)["id"] != "row-b" || data[2].(map[string]any)["id"] != "row-a" {
		t.Fatalf("rows not newest first: %v", data)
	}
}

func TestGlobalActivityIncludesClientIdentity(t *testing.T) {
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	client2ID, _ := createClientWithModel(t, api, "second client")
	insertLogRow(t, db, "row-a", clientID, "provider-a/model-a", strPtr("provider-a"), strPtr("model-a"), "chat", 0, 200, 10, int64Ptr(1), int64Ptr(1), "upstream-a", "req-a", nil, "2026-01-01T00:00:01Z")
	insertLogRow(t, db, "row-b", client2ID, "provider-a/model-a", strPtr("provider-a"), strPtr("model-a"), "chat", 0, 200, 20, int64Ptr(2), int64Ptr(2), "upstream-b", "req-b", nil, "2026-01-01T00:00:02Z")

	status, payload, _ := api.request("GET", "/api/admin/activity", nil)
	if status != 200 {
		t.Fatalf("activity: %d %v", status, payload)
	}
	data := payload["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(data))
	}
	first := data[0].(map[string]any)
	if first["client_key_id"] != client2ID || first["client_name"] != "second client" {
		t.Fatalf("client identity wrong for first row: %v", first)
	}
	second := data[1].(map[string]any)
	if second["client_key_id"] != clientID || second["client_name"] != "test client" {
		t.Fatalf("client identity wrong for second row: %v", second)
	}
}

func TestGlobalActivitySearchFields(t *testing.T) {
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	client2ID, _ := createClientWithModel(t, api, "second client")
	// Row A: client1 direct success.
	insertLogRow(t, db, "row-a", clientID, "provider-a/model-a", strPtr("provider-a"), strPtr("model-a"), "chat", 0, 200, 10, int64Ptr(1), int64Ptr(1), "upstream-a", "req-a", nil, "2026-01-01T00:00:01Z")
	// Row B: client2 virtual request with a distinct requested vs resolved model
	// and a distinct resolved provider so each search field is uniquely testable.
	insertLogRow(t, db, "row-b", client2ID, "virtual/x", strPtr("other-provider"), strPtr("model-b"), "chat", 0, 200, 20, int64Ptr(2), int64Ptr(2), "upstream-b", "req-b", nil, "2026-01-01T00:00:02Z")
	// Row C: client1 failure with error text and 404 status.
	insertLogRow(t, db, "row-c", clientID, "provider-a/nope", nil, nil, "chat", 0, 404, 5, nil, nil, "", "req-c", strPtr("model_not_found"), "2026-01-01T00:00:03Z")

	search := func(term string) []any {
		status, payload, _ := api.request("GET", "/api/admin/activity?search="+term, nil)
		if status != 200 {
			t.Fatalf("search %q: %d %v", term, status, payload)
		}
		return payload["data"].([]any)
	}

	// Client name.
	if rows := search("second"); len(rows) != 1 || rows[0].(map[string]any)["id"] != "row-b" {
		t.Fatalf("search by client name failed: %v", rows)
	}
	// Requested model.
	if rows := search("virtual%2Fx"); len(rows) != 1 || rows[0].(map[string]any)["id"] != "row-b" {
		t.Fatalf("search by requested model failed: %v", rows)
	}
	// Resolved provider.
	if rows := search("other-provider"); len(rows) != 1 || rows[0].(map[string]any)["id"] != "row-b" {
		t.Fatalf("search by resolved provider failed: %v", rows)
	}
	// Resolved model (distinct from requested model for row-b).
	if rows := search("model-b"); len(rows) != 1 || rows[0].(map[string]any)["id"] != "row-b" {
		t.Fatalf("search by resolved model failed: %v", rows)
	}
	// HTTP status text.
	if rows := search("404"); len(rows) != 1 || rows[0].(map[string]any)["id"] != "row-c" {
		t.Fatalf("search by http status failed: %v", rows)
	}
	if rows := search("200"); len(rows) != 2 {
		t.Fatalf("search by http status 200 failed: %v", rows)
	}
	// Client request ID.
	if rows := search("req-b"); len(rows) != 1 || rows[0].(map[string]any)["id"] != "row-b" {
		t.Fatalf("search by client request id failed: %v", rows)
	}
	// Provider request ID.
	if rows := search("upstream-b"); len(rows) != 1 || rows[0].(map[string]any)["id"] != "row-b" {
		t.Fatalf("search by provider request id failed: %v", rows)
	}
	// Error text.
	if rows := search("model_not_found"); len(rows) != 1 || rows[0].(map[string]any)["id"] != "row-c" {
		t.Fatalf("search by error text failed: %v", rows)
	}
	// Case-insensitive LIKE.
	if rows := search("MODEL-B"); len(rows) != 1 || rows[0].(map[string]any)["id"] != "row-b" {
		t.Fatalf("case-insensitive search failed: %v", rows)
	}
}

// TestGlobalActivitySearchResolvedPair covers the Real Models "Activity" button:
// a real-model request is logged with the client's requested_model as an alias
// (e.g. "main/hermes-daily") while the provider and model are stored in separate
// resolved_provider/resolved_model columns. Searching by the canonical
// "provider/model" id must still find the row via the concatenated resolved pair.
func TestGlobalActivitySearchResolvedPair(t *testing.T) {
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	// Row A: real-model request addressed by an alias, resolved to provider-a/model-a.
	insertLogRow(t, db, "row-a", clientID, "main/hermes-daily", strPtr("provider-a"), strPtr("model-a"), "chat", 0, 200, 10, int64Ptr(1), int64Ptr(1), "upstream-a", "req-a", nil, "2026-01-01T00:00:01Z")
	// Row B: direct request whose requested_model already equals the canonical id.
	insertLogRow(t, db, "row-b", clientID, "provider-a/model-a", strPtr("provider-a"), strPtr("model-a"), "chat", 0, 200, 20, int64Ptr(2), int64Ptr(2), "upstream-b", "req-b", nil, "2026-01-01T00:00:02Z")

	search := func(term string) []any {
		status, payload, _ := api.request("GET", "/api/admin/activity?search="+term, nil)
		if status != 200 {
			t.Fatalf("search %q: %d %v", term, status, payload)
		}
		return payload["data"].([]any)
	}

	// Searching by the canonical id must surface both the alias-addressed row and
	// the direct row (the alias row only matches via the resolved provider/model pair).
	if rows := search("provider-a%2Fmodel-a"); len(rows) != 2 {
		t.Fatalf("search by canonical resolved pair failed, want 2 rows, got %d: %v", len(rows), rows)
	}
	// The alias row must not be found by its requested_model alone.
	if rows := search("hermes-daily"); len(rows) != 1 || rows[0].(map[string]any)["id"] != "row-a" {
		t.Fatalf("search by alias requested_model failed: %v", rows)
	}
}

func TestGlobalActivityPagination(t *testing.T) {
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	// Insert 5 rows with increasing created_at.
	for i := 1; i <= 5; i++ {
		ts := "2026-01-01T00:00:0" + string(rune('0'+i)) + "Z"
		insertLogRow(t, db, "row-"+string(rune('0'+i)), clientID, "provider-a/model-a", strPtr("provider-a"), strPtr("model-a"), "chat", 0, 200, int64(i), int64Ptr(int64(i)), int64Ptr(int64(i)), "upstream", "req-"+string(rune('0'+i)), nil, ts)
	}
	// limit=2 offset=0 -> newest first: row-5, row-4.
	status, payload, _ := api.request("GET", "/api/admin/activity?limit=2&offset=0", nil)
	if status != 200 {
		t.Fatalf("activity: %d %v", status, payload)
	}
	data := payload["data"].([]any)
	if len(data) != 2 || data[0].(map[string]any)["id"] != "row-5" || data[1].(map[string]any)["id"] != "row-4" {
		t.Fatalf("limit/offset page 1 wrong: %v", data)
	}
	if payload["limit"] != float64(2) || payload["offset"] != float64(0) {
		t.Fatalf("metadata wrong: %v", payload)
	}
	// limit=2 offset=2 -> row-3, row-2.
	status, payload, _ = api.request("GET", "/api/admin/activity?limit=2&offset=2", nil)
	if status != 200 {
		t.Fatalf("activity: %d %v", status, payload)
	}
	data = payload["data"].([]any)
	if len(data) != 2 || data[0].(map[string]any)["id"] != "row-3" || data[1].(map[string]any)["id"] != "row-2" {
		t.Fatalf("limit/offset page 2 wrong: %v", data)
	}
	// limit=2 offset=4 -> row-1 only.
	status, payload, _ = api.request("GET", "/api/admin/activity?limit=2&offset=4", nil)
	if status != 200 {
		t.Fatalf("activity: %d %v", status, payload)
	}
	data = payload["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["id"] != "row-1" {
		t.Fatalf("limit/offset page 3 wrong: %v", data)
	}
}

func TestGlobalActivityNoSensitiveMaterial(t *testing.T) {
	api, _, _, secret := loggingTestHarness(t, mockUpstream(t))
	_, secret2 := createClientWithModel(t, api, "second client")
	// Make real requests through both clients with distinctive prompt content.
	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "provider-a/model-a", "messages": []any{map[string]any{"role": "user", "content": "PROMPT-SECRET-MARKER"}}})
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("client1 request status %d", resp.StatusCode)
	}
	resp, _ = clientCall(t, api.base, secret2, "/v1/chat/completions", map[string]any{"model": "provider-a/model-a", "messages": []any{map[string]any{"role": "user", "content": "RESPONSE-SECRET-MARKER"}}})
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("client2 request status %d", resp.StatusCode)
	}

	status, payload, _ := api.request("GET", "/api/admin/activity?limit=200", nil)
	if status != 200 {
		t.Fatalf("activity: %d %v", status, payload)
	}
	raw, _ := json.Marshal(payload)
	body := string(raw)
	for _, forbidden := range []string{"PROMPT-SECRET-MARKER", "RESPONSE-SECRET-MARKER", "provider-secret", "Bearer", "sk-tr-", "Authorization", "tool", "reasoning"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("sensitive material leaked into global activity: %q", forbidden)
		}
	}
}

func int64Ptr(v int64) *int64 { return &v }
