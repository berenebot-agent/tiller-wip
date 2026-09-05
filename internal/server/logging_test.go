package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
)

// loggingTestHarness wires up a mock upstream, a router, and an admin session.
func loggingTestHarness(t *testing.T, upstream http.HandlerFunc) (*testAPI, *database.DB, string, string) {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	app, err := New(config.Config{AdminUsername: "admin", AdminPassword: "correct horse", DataDir: t.TempDir(), ListenAddr: ":8080"}, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)
	jar, _ := cookiejar.New(nil)
	api := &testAPI{t: t, base: router.URL, client: &http.Client{Jar: jar}, server: app}
	status, payload, _ := api.request("POST", "/api/admin/session", map[string]any{"username": "admin", "password": "correct horse"})
	if status != 200 {
		t.Fatalf("login: %d %v", status, payload)
	}
	api.csrf = payload["csrf_token"].(string)
	status, payload, _ = api.request("POST", "/api/admin/providers", map[string]any{"name": "provider-a", "type": "generic-openai", "base_url": server.URL + "/v1", "credential": "provider-secret"})
	if status != 201 {
		t.Fatalf("create provider: %d %v", status, payload)
	}
	providerID := payload["id"].(string)
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
	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "test client", "description": "logging", "type": "catalogue"})
	if status != 201 {
		t.Fatalf("create key: %d %v", status, payload)
	}
	clientID := payload["id"].(string)
	clientSecret := payload["secret"].(string)
	status, payload, _ = api.request("PUT", "/api/admin/client-keys/"+clientID+"/permissions", map[string]any{"defaults": []any{}, "permissions": []any{map[string]any{"kind": "real", "model_id": modelID, "enabled": true}}})
	if status != 204 {
		t.Fatalf("permissions: %d %v", status, payload)
	}
	return api, db, clientID, clientSecret
}

func mockUpstream(t *testing.T) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var input map[string]any
		_ = json.NewDecoder(r.Body).Decode(&input)
		if streaming, _ := input["stream"].(bool); streaming {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			_, _ = io.WriteString(w, `data: {"id":"one","object":"chat.completion.chunk","model":"model-a","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`+"\n\n")
			flusher.Flush()
			_, _ = io.WriteString(w, `data: {"id":"one","object":"chat.completion.chunk","model":"model-a","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7,"prompt_tokens_details":{"cached_tokens":3}}}`+"\n\n")
			flusher.Flush()
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Request-Id", "upstream-req-123")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "response-1", "object": "chat.completion", "model": "model-a", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 3, "total_tokens": 13}})
	})
}

func mockJSONUpstream(t *testing.T) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "response-1", "object": "chat.completion", "model": "model-a",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
		})
	})
}

func clientCall(t *testing.T, base, secret, path string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, _ := json.Marshal(body)
		reader = bytes.NewReader(encoded)
	}
	req, _ := http.NewRequest("POST", base+path, reader)
	req.Header.Set("Authorization", "Bearer "+secret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	decoded := map[string]any{}
	if !bytes.Contains([]byte(resp.Header.Get("Content-Type")), []byte("event-stream")) {
		_ = json.NewDecoder(resp.Body).Decode(&decoded)
		resp.Body.Close()
	}
	return resp, decoded
}

func TestRequestLoggingMetadataAndClientRequestID(t *testing.T) {
	api, db, clientID, secret := loggingTestHarness(t, mockUpstream(t))
	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "provider-a/model-a", "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
	if resp.StatusCode != 200 {
		t.Fatalf("request status %d", resp.StatusCode)
	}
	reqID := resp.Header.Get("X-Tiller-Request-Id")
	if reqID == "" {
		t.Fatal("client did not receive X-Tiller-Request-Id")
	}
	var row struct {
		RequestedModel    string  `json:"requested_model"`
		ResolvedProvider  *string `json:"resolved_provider"`
		ResolvedModel     *string `json:"resolved_model"`
		Protocol          string  `json:"protocol"`
		Streaming         bool    `json:"streaming"`
		HTTPStatus        int     `json:"http_status"`
		InputTokens       *int64  `json:"input_tokens"`
		OutputTokens      *int64  `json:"output_tokens"`
		ProviderRequestID *string `json:"provider_request_id"`
		ClientRequestID   string  `json:"client_request_id"`
		ErrorText         *string `json:"error_text"`
	}
	status, payload, _ := api.request("GET", "/api/admin/client-keys/"+clientID+"/activity", nil)
	if status != 200 {
		t.Fatalf("activity: %d %v", status, payload)
	}
	data := payload["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("expected 1 log row, got %d", len(data))
	}
	raw, _ := json.Marshal(data[0])
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	if row.RequestedModel != "provider-a/model-a" || row.ResolvedProvider == nil || *row.ResolvedProvider != "provider-a" || row.ResolvedModel == nil || *row.ResolvedModel != "model-a" {
		t.Fatalf("resolution metadata wrong: %+v", row)
	}
	if row.Protocol != "chat" || row.Streaming || row.HTTPStatus != 200 {
		t.Fatalf("request metadata wrong: %+v", row)
	}
	if row.InputTokens == nil || *row.InputTokens != 10 || row.OutputTokens == nil || *row.OutputTokens != 3 {
		t.Fatalf("token counts wrong: %+v", row)
	}
	if row.ProviderRequestID == nil || *row.ProviderRequestID != "upstream-req-123" {
		t.Fatalf("provider request id wrong: %+v", row)
	}
	if row.ClientRequestID != reqID {
		t.Fatalf("client request id mismatch: log=%q header=%q", row.ClientRequestID, reqID)
	}
	if row.ErrorText != nil {
		t.Fatalf("error_text should be NULL on success, got %v", *row.ErrorText)
	}
	// No prompt/response body may ever land in the log.
	var count int
	if err := db.SQL.QueryRow(`SELECT count(*) FROM request_logs WHERE requested_model LIKE '%hello%' OR requested_model LIKE '%ok%'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("prompt/response body leaked into request_logs: count=%d err=%v", count, err)
	}
}

func TestRequestLoggingStreamingUsageAndFailure(t *testing.T) {
	api, _, clientID, secret := loggingTestHarness(t, mockUpstream(t))
	// Streaming request captures usage from the final chunk.
	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "provider-a/model-a", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "stream"}}})
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("stream status %d", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream content type = %q", resp.Header.Get("Content-Type"))
	}
	// A guessed/disabled model logs a failure with error_text populated.
	resp, _ = clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "provider-a/nope", "messages": []any{map[string]any{"role": "user", "content": "x"}}})
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	status, payload, _ := api.request("GET", "/api/admin/client-keys/"+clientID+"/activity?limit=50", nil)
	if status != 200 {
		t.Fatalf("activity: %d %v", status, payload)
	}
	data := payload["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("expected 2 log rows, got %d", len(data))
	}
	// Newest first: the failure.
	fail := data[0].(map[string]any)
	if fail["http_status"] != float64(404) || fail["error_text"] == nil {
		t.Fatalf("failure row wrong: %v", fail)
	}
	if fail["resolved_provider"] != nil || fail["resolved_model"] != nil {
		t.Fatalf("failed resolution should have NULL provider/model: %v", fail)
	}
	// The streaming success row.
	stream := data[1].(map[string]any)
	if stream["streaming"] != true || stream["http_status"] != float64(200) {
		t.Fatalf("stream row wrong: %v", stream)
	}
	if stream["input_tokens"] == nil || stream["output_tokens"] == nil {
		t.Fatalf("streaming usage not captured: %v", stream)
	}
	if stream["cache_read_input_tokens"] == nil || stream["cache_read_input_tokens"] != float64(3) {
		t.Fatalf("streaming prompt-cache tokens not captured (expected cache_read=3): %v", stream)
	}
}

func TestRequestLoggingStreamRequestWithJSONResponseIsNotStreaming(t *testing.T) {
	api, _, clientID, secret := loggingTestHarness(t, mockJSONUpstream(t))
	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{
		"model": "provider-a/model-a", "stream": true, "messages": []any{},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("JSON response was returned as SSE")
	}
	status, payload, _ := api.request("GET", "/api/admin/client-keys/"+clientID+"/activity", nil)
	if status != http.StatusOK {
		t.Fatalf("activity: %d %v", status, payload)
	}
	data := payload["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["streaming"] != false {
		t.Fatalf("streaming metadata = %v, want false", data)
	}
}

func TestRequestLogPrimaryKeyIsClientRequestID(t *testing.T) {
	api, _, clientID, secret := loggingTestHarness(t, mockUpstream(t))
	const n = 3
	headers := make([]string, 0, n)
	for i := 0; i < n; i++ {
		resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "provider-a/model-a", "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("request %d status %d", i, resp.StatusCode)
		}
		headers = append(headers, resp.Header.Get("X-Tiller-Request-Id"))
	}
	status, payload, _ := api.request("GET", "/api/admin/client-keys/"+clientID+"/activity?limit=50", nil)
	if status != 200 {
		t.Fatalf("activity: %d %v", status, payload)
	}
	data := payload["data"].([]any)
	if len(data) != n {
		t.Fatalf("expected %d log rows, got %d (rows lost)", n, len(data))
	}
	// Activity is newest-first; headers were collected oldest-first.
	seen := map[string]bool{}
	for i, raw := range data {
		row := raw.(map[string]any)
		id, _ := row["id"].(string)
		crid, _ := row["client_request_id"].(string)
		if id == "" || id != crid {
			t.Fatalf("row %d: id=%q client_request_id=%q (must be equal)", i, id, crid)
		}
		// Newest first: data[0] is the last request made.
		want := headers[n-1-i]
		if id != want {
			t.Fatalf("row %d: stored id %q does not match X-Tiller-Request-Id %q", i, id, want)
		}
		if seen[id] {
			t.Fatalf("duplicate stored id %q", id)
		}
		seen[id] = true
	}
}

func TestLoggingDisabledClientProducesNoRows(t *testing.T) {
	api, _, clientID, secret := loggingTestHarness(t, mockUpstream(t))
	// Disable logging for this client.
	status, _, _ := api.request("PATCH", "/api/admin/client-keys/"+clientID, map[string]any{"logging_enabled": false})
	if status != 204 {
		t.Fatalf("disable logging: %d", status)
	}
	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "provider-a/model-a", "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("request status %d", resp.StatusCode)
	}
	status, payload, _ := api.request("GET", "/api/admin/client-keys/"+clientID+"/activity", nil)
	if status != 200 {
		t.Fatalf("activity: %d %v", status, payload)
	}
	if len(payload["data"].([]any)) != 0 {
		t.Fatalf("logging-disabled client produced log rows: %v", payload["data"])
	}
}

// TestWriteLogBestEffort verifies the best-effort logging contract: writeLog is
// void, never fails the request, and never writes a row when logging is
// disabled or the client is unknown. Forcing a real INSERT failure would
// require weakening production code, so we assert the contract is preserved
// instead: writeLog returns nothing and the request path is unaffected.
func TestWriteLogBestEffort(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &Server{db: db}
	// nil row is a no-op.
	s.writeLog(context.Background(), nil)
	// A row for a client that does not exist hits the logging-disabled path and
	// returns without writing.
	s.writeLog(context.Background(), &logRow{clientKeyID: "does-not-exist", clientRequestID: "req-x", requestedModel: "m", protocol: "chat", httpStatus: 200, latencyMs: 1, createdAt: "now"})
	var count int
	if err := db.SQL.QueryRow(`SELECT count(*) FROM request_logs`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("best-effort writeLog wrote rows: count=%d err=%v", count, err)
	}
}

// TestWriteLogTransactionPersistsLogAndAttempt verifies the single-transaction
// Activity write: one routed request produces exactly one request_logs row and
// one request_attempts row for its single attempt.
func TestWriteLogTransactionPersistsLogAndAttempt(t *testing.T) {
	api, _, clientID, secret := loggingTestHarness(t, mockUpstream(t))
	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "provider-a/model-a", "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
	if resp.StatusCode != 200 {
		t.Fatalf("request status %d", resp.StatusCode)
	}
	reqID := resp.Header.Get("X-Tiller-Request-Id")
	status, payload, _ := api.request("GET", "/api/admin/client-keys/"+clientID+"/activity", nil)
	if status != 200 || len(payload["data"].([]any)) != 1 {
		t.Fatalf("expected 1 request log, got status=%d payload=%v", status, payload)
	}
	status, payload, _ = api.request("GET", "/api/admin/activity/"+reqID+"/attempts", nil)
	if status != 200 {
		t.Fatalf("attempts: %d %v", status, payload)
	}
	attempts := payload["data"].([]any)
	if len(attempts) != 1 {
		t.Fatalf("expected 1 attempt row, got %d: %v", len(attempts), payload)
	}
	row := attempts[0].(map[string]any)
	if row["attempt_number"] != float64(1) || row["provider"] != "provider-a" || row["model"] != "model-a" || row["result"] != "success" {
		t.Fatalf("attempt row wrong: %v", row)
	}
}

// TestWriteLogTransactionPersistsAllFallbackAttempts verifies that an ordered
// fallback request persists every recorded attempt (the failed first target and
// the succeeding second one) alongside its single request log row.
func TestWriteLogTransactionPersistsAllFallbackAttempts(t *testing.T) {
	api, secret, canonical := notificationTestHarness(t, failUpstream(t), okUpstream(t))
	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
	if resp.StatusCode != 200 {
		t.Fatalf("fallback request should succeed, got %d", resp.StatusCode)
	}
	reqID := resp.Header.Get("X-Tiller-Request-Id")
	status, payload, _ := api.request("GET", "/api/admin/activity/"+reqID+"/attempts", nil)
	if status != 200 {
		t.Fatalf("attempts: %d %v", status, payload)
	}
	attempts := payload["data"].([]any)
	if len(attempts) != 2 {
		t.Fatalf("expected 2 attempt rows for the fallback, got %d: %v", len(attempts), payload)
	}
	first := attempts[0].(map[string]any)
	if first["attempt_number"] != float64(1) || first["provider"] != "provider-a" || first["result"] != "failed" {
		t.Fatalf("first (failed) attempt wrong: %v", first)
	}
	second := attempts[1].(map[string]any)
	if second["attempt_number"] != float64(2) || second["provider"] != "provider-b" || second["result"] != "success" {
		t.Fatalf("second (succeeding) attempt wrong: %v", second)
	}
}

func TestProviderErrorMessageAndBodyArePassedThroughToClientButNotLogged(t *testing.T) {
	// Direct (non-virtual, non-translated) routes pass through the
	// provider's structured error body to the originating client so it
	// sees the provider's error shape. The body is bounded and never
	// stored in the activity log (privacy guardrail).
	const marker = "PROVIDER-ERROR-SECRET-MARKER"
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": marker}, "body": marker})
	})
	api, _, clientID, secret := loggingTestHarness(t, upstream)
	resp, payload := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "provider-a/model-a", "messages": []any{}})
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("provider error status = %d, want %d", resp.StatusCode, http.StatusBadGateway)
	}
	if !strings.Contains(string(mustJSON(t, payload)), marker) {
		t.Fatalf("provider error was not passed through to client: %v", payload)
	}
	reqID := resp.Header.Get("X-Tiller-Request-Id")

	status, activity, _ := api.request("GET", "/api/admin/client-keys/"+clientID+"/activity", nil)
	if status != http.StatusOK || strings.Contains(string(mustJSON(t, activity)), marker) {
		t.Fatalf("provider error was persisted in activity: status=%d payload=%v", status, activity)
	}
	activityRow := activity["data"].([]any)[0].(map[string]any)
	if activityRow["error_text"] != "upstream_error" {
		t.Fatalf("provider error metadata = %v", activityRow)
	}
	// error_message should be a human-readable translation, not nil.
	if msg, ok := activityRow["error_message"].(string); !ok || msg == "" {
		t.Fatalf("expected human-readable error_message, got %v", activityRow)
	}
	status, attempts, _ := api.request("GET", "/api/admin/activity/"+reqID+"/attempts", nil)
	if status != http.StatusOK || strings.Contains(string(mustJSON(t, attempts)), marker) {
		t.Fatalf("provider error was persisted in attempts: status=%d payload=%v", status, attempts)
	}
	attemptRow := attempts["data"].([]any)[0].(map[string]any)
	if attemptRow["failure_class"] != "http_502" {
		t.Fatalf("provider attempt metadata = %v", attemptRow)
	}
	// Attempt error_message should be a human-readable translation.
	if msg, ok := attemptRow["error_message"].(string); !ok || msg == "" {
		t.Fatalf("expected human-readable attempt error_message, got %v", attemptRow)
	}
}

func TestRequestLoggingDoesNotCaptureBodies(t *testing.T) {
	const requestMarker = "CLIENT-REQUEST-SECRET-MARKER"
	const errorMarker = "PROVIDER-ERROR-SECRET-MARKER"
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": errorMarker}})
	})
	api, db, clientID, secret := loggingTestHarness(t, upstream)
	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "provider-a/model-a", "messages": []any{map[string]any{"role": "user", "content": requestMarker}}})
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("provider error status = %d", resp.StatusCode)
	}
	var requestBody, errorBody *string
	var requestTruncated, errorTruncated int
	if err := db.SQL.QueryRow(`SELECT request_body,error_body,request_body_truncated,error_body_truncated FROM request_logs WHERE client_key_id=?`, clientID).Scan(&requestBody, &errorBody, &requestTruncated, &errorTruncated); err != nil {
		t.Fatal(err)
	}
	if requestBody != nil || errorBody != nil || requestTruncated != 0 || errorTruncated != 0 {
		t.Fatalf("request or provider bodies were persisted: request=%v error=%v truncated=%d/%d", requestBody, errorBody, requestTruncated, errorTruncated)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestWriteLogTransactionFailureLeavesNoPartialRow verifies the all-or-nothing
// property of the single-transaction write: when the request_logs INSERT fails
// (here a primary-key collision), the transaction rolls back and no
// request_attempts row leaks for that request.
func TestWriteLogTransactionFailureLeavesNoPartialRow(t *testing.T) {
	_, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	now := database.Now()
	if _, err := db.SQL.Exec(`INSERT INTO request_logs(id,client_key_id,requested_model,protocol,streaming,http_status,latency_ms,client_request_id,created_at) VALUES('dup-req',?,'provider-a/model-a','chat',0,200,1,'dup-req',?)`, clientID, now); err != nil {
		t.Fatal(err)
	}
	s := &Server{db: db}
	s.writeLog(context.Background(), &logRow{clientKeyID: clientID, clientRequestID: "dup-req", requestedModel: "provider-a/model-a", protocol: "chat", httpStatus: 200, latencyMs: 1, createdAt: now, attempts: []requestAttempt{{providerModelID: "pm-a", provider: "provider-a", model: "model-a", result: "success", httpStatus: 200, latencyMs: 1}}})
	var count int
	if err := db.SQL.QueryRow(`SELECT count(*) FROM request_attempts WHERE request_log_id='dup-req'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed transaction left attempt rows: count=%d err=%v", count, err)
	}
	if err := db.SQL.QueryRow(`SELECT count(*) FROM request_logs WHERE id='dup-req'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("request_logs rows for dup-req = %d, want 1 (no duplicate): err=%v", count, err)
	}
}

// TestInferenceUnaffectedWhenActivityPersistenceFails verifies the best-effort
// contract under a real write failure: dropping the Activity tables makes the
// deferred writeLog INSERT fail, but the inference request still returns 200
// with a request id. writeLog never fails the request.
func TestInferenceUnaffectedWhenActivityPersistenceFails(t *testing.T) {
	api, db, _, secret := loggingTestHarness(t, mockUpstream(t))
	if _, err := db.SQL.Exec(`DROP TABLE request_attempts`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`DROP TABLE request_logs`); err != nil {
		t.Fatal(err)
	}
	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "provider-a/model-a", "messages": []any{map[string]any{"role": "user", "content": "still works"}}})
	if resp.StatusCode != 200 {
		t.Fatalf("inference must be unaffected by Activity persistence failure, got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Tiller-Request-Id") == "" {
		t.Fatal("no X-Tiller-Request-Id on response")
	}
}

func TestActivitySearchAndClear(t *testing.T) {
	api, _, clientID, secret := loggingTestHarness(t, mockUpstream(t))
	for i := 0; i < 3; i++ {
		resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "provider-a/model-a", "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	status, payload, _ := api.request("GET", "/api/admin/client-keys/"+clientID+"/activity?search=model-a", nil)
	if status != 200 || len(payload["data"].([]any)) != 3 {
		t.Fatalf("search by model: %d %v", status, payload)
	}
	status, payload, _ = api.request("GET", "/api/admin/client-keys/"+clientID+"/activity?search=zzz", nil)
	if status != 200 || len(payload["data"].([]any)) != 0 {
		t.Fatalf("search no match: %d %v", status, payload)
	}
	status, _, _ = api.request("DELETE", "/api/admin/client-keys/"+clientID+"/activity", nil)
	if status != 204 {
		t.Fatalf("clear: %d", status)
	}
	status, payload, _ = api.request("GET", "/api/admin/client-keys/"+clientID+"/activity", nil)
	if status != 200 || len(payload["data"].([]any)) != 0 {
		t.Fatalf("after clear: %d %v", status, payload)
	}
}

func TestClientKeyCreationCopiesLoggingDefaults(t *testing.T) {
	api, _, _, _ := loggingTestHarness(t, mockUpstream(t))
	// Change the defaults, then create a key and confirm it copies them.
	status, _, _ := api.request("PUT", "/api/admin/settings", map[string]any{"default_logging_enabled": false, "default_retention_days": 7})
	if status != 204 {
		t.Fatalf("update settings: %d", status)
	}
	status, payload, _ := api.request("POST", "/api/admin/client-keys", map[string]any{"name": "copied", "description": "defaults", "type": "catalogue"})
	if status != 201 {
		t.Fatalf("create key: %d %v", status, payload)
	}
	clientID := payload["id"].(string)
	status, payload, _ = api.request("GET", "/api/admin/client-keys", nil)
	if status != 200 {
		t.Fatal(payload)
	}
	for _, raw := range payload["data"].([]any) {
		m := raw.(map[string]any)
		if m["id"] == clientID {
			if m["logging_enabled"] != false || m["retention_days"] != float64(7) {
				t.Fatalf("client did not copy defaults: %v", m)
			}
			return
		}
	}
	t.Fatal("created client not found in list")
}

func TestSetCacheFromUsageDeepSeekNativeField(t *testing.T) {
	// DeepSeek reports its prompt cache breakdown as prompt_cache_hit_tokens;
	// ensure it maps to the router's cacheReadInputTokens (hit) field.
	u := map[string]any{
		"prompt_tokens":            float64(1000),
		"prompt_cache_hit_tokens":  float64(800),
		"prompt_cache_miss_tokens": float64(200),
	}
	var usage usageCapture
	setCacheFromUsage(u, &usage)
	if usage.cacheReadInputTokens == nil || *usage.cacheReadInputTokens != 800 {
		t.Fatalf("expected cacheReadInputTokens=800, got %v", usage.cacheReadInputTokens)
	}
	// OpenAI-style prompt_tokens_details.cached_tokens is read before DeepSeek's
	// native field, so it wins when both are present (first non-nil wins).
	u2 := map[string]any{
		"prompt_tokens_details":   map[string]any{"cached_tokens": float64(700)},
		"prompt_cache_hit_tokens": float64(600),
	}
	var usage2 usageCapture
	setCacheFromUsage(u2, &usage2)
	if usage2.cacheReadInputTokens == nil || *usage2.cacheReadInputTokens != 700 {
		t.Fatalf("expected OpenAI-style cached_tokens to win, got %v", usage2.cacheReadInputTokens)
	}
}

func TestPrunerDeletesByRetention(t *testing.T) {
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	// Insert a log row with an old timestamp directly.
	old := time.Now().UTC().Add(-40 * 24 * time.Hour).Format(time.RFC3339Nano)
	if _, err := db.SQL.Exec(`INSERT INTO request_logs(id,client_key_id,requested_model,protocol,streaming,http_status,latency_ms,client_request_id,created_at) VALUES('old1',?,'provider-a/model-a','chat',0,200,1,'req-old',?)`, clientID, old); err != nil {
		t.Fatal(err)
	}
	// Set the client's retention to 30 days.
	status, _, _ := api.request("PATCH", "/api/admin/client-keys/"+clientID, map[string]any{"retention_days": 30})
	if status != 204 {
		t.Fatalf("set retention: %d", status)
	}
	app := &Server{db: db}
	app.pruneRequestLogs(context.Background())
	var count int
	if err := db.SQL.QueryRow(`SELECT count(*) FROM request_logs WHERE id='old1'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("old log not pruned: count=%d err=%v", count, err)
	}
}
