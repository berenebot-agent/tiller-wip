package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/tiller-router/tiller-router/internal/providers"
)

func TestPreflightResponseLimitRejectsOverflowWithoutTruncatingAcceptedBodies(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "below", body: "123"},
		{name: "exact", body: "1234"},
		{name: "over", body: "12345", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{
				Header: make(http.Header),
				Body:   io.NopCloser(bytes.NewBufferString(tc.body)),
			}
			err := preflightResponseLimit(resp, 4)
			if tc.wantErr {
				if !errors.Is(err, errUpstreamResponseTooLarge) {
					t.Fatalf("expected oversized response error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.body {
				t.Fatalf("accepted response changed: got %q want %q", got, tc.body)
			}
		})
	}
}

func TestCompatibleProtocolPrefersNativeModelProtocol(t *testing.T) {
	if got := compatibleProtocol([]providers.Protocol{providers.ProtocolChat, providers.ProtocolResponses, providers.ProtocolMessages}, providers.ProtocolResponses, providers.ProtocolChat); got != providers.ProtocolResponses {
		t.Fatalf("native protocol was not selected: got %q", got)
	}
	if got := compatibleProtocol([]providers.Protocol{providers.ProtocolChat, providers.ProtocolResponses, providers.ProtocolMessages}, providers.ProtocolChat, providers.ProtocolResponses); got != providers.ProtocolChat {
		t.Fatalf("native Chat protocol was not selected: got %q", got)
	}
	if got := compatibleProtocol([]providers.Protocol{providers.ProtocolChat}, "", providers.ProtocolResponses); got != providers.ProtocolChat {
		t.Fatalf("provider translation protocol was not selected: got %q", got)
	}
}

func TestCrossProtocolResponsesRejectsStatefulFeatures(t *testing.T) {
	for _, field := range []string{"conversation", "previous_response_id", "store", "background", "files"} {
		body := []byte(`{"model":"virtual/coding","input":"hello","` + field + `":"value"}`)
		_, err := translateRequest(body, providers.ProtocolResponses, providers.ProtocolChat, "real-model")
		var unsupported unsupportedFeature
		if !errors.As(err, &unsupported) {
			t.Fatalf("%s was not rejected with unsupported_feature: %v", field, err)
		}
	}
}

func TestCrossProtocolStatefulFeatureReturnsMachineReadableError(t *testing.T) {
	api, _, _, secret := loggingTestHarness(t, mockUpstream(t))
	resp, payload := clientCall(t, api.base, secret, "/v1/responses", map[string]any{
		"model":                "provider-a/model-a",
		"input":                "hello",
		"previous_response_id": "resp_previous",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; payload=%v", resp.StatusCode, payload)
	}
	errPayload, _ := payload["error"].(map[string]any)
	if errPayload["code"] != "unsupported_feature" {
		t.Fatalf("error code = %v, want unsupported_feature; payload=%v", errPayload["code"], payload)
	}
}

func TestChatMessagesRoundTripPreservesToolIDsAndArguments(t *testing.T) {
	body := []byte(`{"model":"virtual/coding","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"call_123","type":"function","function":{"name":"lookup","arguments":"{\"city\":\"Perth\"}"}}]},{"role":"tool","tool_call_id":"call_123","content":"sunny"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`)
	messages, err := translateRequest(body, providers.ProtocolChat, providers.ProtocolMessages, "claude-real")
	if err != nil {
		t.Fatal(err)
	}
	chat, err := translateRequest(messages, providers.ProtocolMessages, providers.ProtocolChat, "openai-real")
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{`call_123`, `lookup`, `Perth`, `sunny`} {
		if !bytes.Contains(chat, []byte(needle)) {
			t.Fatalf("round trip lost %q: %s", needle, chat)
		}
	}
}

func TestChatToResponsesPreservesDeveloperAndInstructionContentArrays(t *testing.T) {
	body := []byte(`{"model":"virtual/coding","messages":[{"role":"system","content":[{"type":"text","text":"system rule"}]},{"role":"developer","content":[{"type":"text","text":"developer rule"}]},{"role":"user","content":"hello"}]}`)
	translated, err := translateRequest(body, providers.ProtocolChat, providers.ProtocolResponses, "real-model")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(translated, &got); err != nil {
		t.Fatal(err)
	}
	want := []any{
		map[string]any{"type": "input_text", "text": "system rule"},
		map[string]any{"type": "input_text", "text": "developer rule"},
	}
	if !reflect.DeepEqual(got["instructions"], want) {
		t.Fatalf("instructions = %#v, want %#v", got["instructions"], want)
	}
	input := got["input"].([]any)
	if len(input) != 1 || input[0].(map[string]any)["role"] != "user" {
		t.Fatalf("developer message leaked or user message missing: %#v", input)
	}
}

func TestChatToResponsesSupportsToolChoice(t *testing.T) {
	for _, tc := range []struct {
		choice string
		want   map[string]any
	}{
		{choice: "none", want: map[string]any{"type": "none"}},
		{choice: "auto", want: map[string]any{"type": "auto"}},
		{choice: "required", want: map[string]any{"type": "any"}},
	} {
		t.Run(tc.choice, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{
				"model":       "virtual/coding",
				"messages":    []any{map[string]any{"role": "user", "content": "hello"}},
				"tool_choice": tc.choice,
			})
			translated, err := translateRequest(body, providers.ProtocolChat, providers.ProtocolResponses, "real-model")
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			_ = json.Unmarshal(translated, &got)
			if !reflect.DeepEqual(got["tool_choice"], tc.want) {
				t.Fatalf("tool_choice = %#v, want %#v", got["tool_choice"], tc.want)
			}
		})
	}
}

func TestChatToResponsesRejectsUnsupportedRolesAndContentBlocks(t *testing.T) {
	for _, body := range []string{
		`{"model":"virtual/coding","messages":[{"role":"function","content":"unsupported"}]}`,
		`{"model":"virtual/coding","messages":[{"role":"user","content":[{"type":"audio","data":"unsupported"}]}]}`,
		`{"model":"virtual/coding","messages":[{"role":"user","content":42}]}`,
	} {
		_, err := translateRequest([]byte(body), providers.ProtocolChat, providers.ProtocolResponses, "real-model")
		var unsupported unsupportedFeature
		if !errors.As(err, &unsupported) {
			t.Fatalf("request was not rejected as unsupported: %v", err)
		}
	}
}

func FuzzProtocolRequestParser(f *testing.F) {
	f.Add([]byte(`{"model":"virtual/coding","messages":[]}`))
	f.Add([]byte(`{"input":"hello"}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		_, _ = translateRequest(body, providers.ProtocolChat, providers.ProtocolMessages, "target")
		_, _ = translateRequest(body, providers.ProtocolResponses, providers.ProtocolChat, "target")
	})
}

// TestOpenCodeFreeMuseSparkRoutesToResponsesAPI verifies that muse-spark-1.2 and
// muse-spark-1.3 contributor-free models are routed to the Responses API
// (/v1/responses with `input` field), not the Chat Completions API. The OpenCode
// relay 500s when these models are called via /v1/chat/completions — only the
// Responses endpoint serves them.
func TestOpenCodeFreeMuseSparkRoutesToResponsesAPI(t *testing.T) {
	var mu sync.Mutex
	recordPath := ""
	dedicated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": "list",
				"data": []any{
					map[string]any{"id": "muse-spark-1.2-contributor-free", "object": "model"},
					map[string]any{"id": "muse-spark-1.3-contributor-free", "object": "model"},
					map[string]any{"id": "mimo-v2.5-free", "object": "model"},
				},
			})
			return
		}
		mu.Lock()
		recordPath = r.URL.Path
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "resp",
			"object":  "response",
			"model":   "test",
			"output":  []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "ok"}}}},
			"usage":   map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
		})
	}))
	t.Cleanup(dedicated.Close)

	api, _, _, _ := loggingTestHarness(t, mockUpstream(t))

	status, payload, _ := api.request("POST", "/api/admin/providers", map[string]any{
		"name":       "opencode-free-muse",
		"type":       "opencode-free",
		"base_url":   dedicated.URL + "/v1",
		"protocols":  []string{"chat", "responses"},
	})
	if status != 201 {
		t.Fatalf("create provider: %d %v", status, payload)
	}
	providerID := payload["id"].(string)

	status, payload, _ = api.request("POST", "/api/admin/providers/"+providerID+"/refresh", nil)
	if status != 200 && status != 204 {
		t.Fatalf("refresh: %d %v", status, payload)
	}
	status, payload, _ = api.request("GET", "/api/admin/providers/"+providerID+"/models", nil)
	if status != 200 {
		t.Fatalf("list models: %d %v", status, payload)
	}
	modelIDs := map[string]string{}
	for _, raw := range payload["data"].([]any) {
		m := raw.(map[string]any)
		if upID, ok := m["upstream_model_id"].(string); ok {
			if id, ok := m["id"].(string); ok {
				modelIDs[upID] = id
			}
		}
	}

	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "muse test", "type": "catalogue"})
	if status != 201 {
		t.Fatalf("create key: %d %v", status, payload)
	}
	clientID := payload["id"].(string)
	clientSecret := payload["secret"].(string)

	perms := []any{}
	for _, modelID := range modelIDs {
		perms = append(perms, map[string]any{"kind": "real", "model_id": modelID, "enabled": true})
	}
	status, _, _ = api.request("PUT", "/api/admin/client-keys/"+clientID+"/permissions", map[string]any{"defaults": []any{}, "permissions": perms})
	if status != 204 {
		t.Fatalf("permissions: %d", status)
	}

	for upstreamID, wantPath := range map[string]string{
		"muse-spark-1.2-contributor-free": "/v1/responses",
		"muse-spark-1.3-contributor-free": "/v1/responses",
		"mimo-v2.5-free":                  "/v1/chat/completions",
	} {
		mu.Lock()
		recordPath = ""
		mu.Unlock()
		chatBody, _ := json.Marshal(map[string]any{
			"model":     "opencode-free-muse/" + upstreamID,
			"messages":  []any{map[string]any{"role": "user", "content": "hi"}},
			"max_tokens": 8,
			"stream":    false,
		})
		req, _ := http.NewRequest("POST", api.base+"/v1/chat/completions", bytes.NewReader(chatBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+clientSecret)
		resp, err := api.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		mu.Lock()
		gotPath := recordPath
		mu.Unlock()
		if gotPath != wantPath {
			t.Errorf("%s: upstream saw path %q, want %q", upstreamID, gotPath, wantPath)
		}
	}
	status, payload, _ = api.request("GET", "/api/admin/client-keys/"+clientID+"/activity?limit=50", nil)
	if status != 200 {
		t.Fatalf("activity: %d %v", status, payload)
	}
	for _, raw := range payload["data"].([]any) {
		if raw.(map[string]any)["streaming"] == true {
			t.Fatal("translated JSON response was marked streaming")
		}
	}
}
