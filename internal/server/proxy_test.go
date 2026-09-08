package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
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

func TestChatToResponsesFoldsSystemAndDeveloperIntoInstructions(t *testing.T) {
	body := []byte(`{"model":"virtual/coding","messages":[{"role":"user","content":"hello"},{"role":"system","content":[{"type":"text","text":"system rule"}]},{"role":"developer","content":[{"type":"text","text":"developer rule"}]}]}`)
	translated, err := translateRequest(body, providers.ProtocolChat, providers.ProtocolResponses, "real-model")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(translated, &got); err != nil {
		t.Fatal(err)
	}
	instructions, _ := got["instructions"].(string)
	if !strings.Contains(instructions, "system rule") || !strings.Contains(instructions, "developer rule") {
		t.Fatalf("instructions missing system/developer text: %q", instructions)
	}
	if !strings.HasPrefix(strings.TrimSpace(instructions), "system rule") {
		t.Fatalf("expected system text before developer in instructions: %q", instructions)
	}
	input := got["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input = %#v, want one user message", input)
	}
	item := input[0].(map[string]any)
	if item["type"] != "message" || item["role"] != "user" {
		t.Fatalf("input[0] = %#v, want typed user message", item)
	}
}

func TestChatToResponsesSupportsToolChoice(t *testing.T) {
	// The Responses relay accepts only the string "auto" for tool_choice.
	// "none" (do not call tools) is represented safely by omitting tools
	// entirely — returning tool_choice while tools are still present would
	// allow a call and silently weaken the contract. "required" and named
	// function choices have no relay equivalent and are rejected.
	t.Run("auto", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"model":       "virtual/coding",
			"messages":    []any{map[string]any{"role": "user", "content": "hello"}},
			"tool_choice": "auto",
		})
		translated, err := translateRequest(body, providers.ProtocolChat, providers.ProtocolResponses, "real-model")
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		_ = json.Unmarshal(translated, &got)
		if !reflect.DeepEqual(got["tool_choice"], "auto") {
			t.Fatalf("tool_choice = %#v, want \"auto\"", got["tool_choice"])
		}
	})

	t.Run("none-suppresses-tools", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"model":       "virtual/coding",
			"messages":    []any{map[string]any{"role": "user", "content": "hello"}},
			"tool_choice": "none",
			"tools":       []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}}},
		})
		translated, err := translateRequest(body, providers.ProtocolChat, providers.ProtocolResponses, "real-model")
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(translated, &got); err != nil {
			t.Fatal(err)
		}
		if _, present := got["tool_choice"]; present {
			t.Fatalf("tool_choice present = %#v, want omitted for none", got["tool_choice"])
		}
		if _, present := got["tools"]; present {
			t.Fatalf("tools present = %#v, want suppressed for none", got["tools"])
		}
	})

	for _, tc := range []struct {
		name, body string
	}{
		{"required", `{"model":"virtual/coding","messages":[{"role":"user","content":"hello"}],"tool_choice":"required"}`},
		{"named-function", `{"model":"virtual/coding","messages":[{"role":"user","content":"hello"}],"tool_choice":{"type":"function","function":{"name":"lookup"}}}`},
	} {
		t.Run("rejects-"+tc.name, func(t *testing.T) {
			_, err := translateRequest([]byte(tc.body), providers.ProtocolChat, providers.ProtocolResponses, "real-model")
			if err == nil {
				t.Fatalf("expected unsupportedFeature error for %s", tc.name)
			}
			var ufe unsupportedFeature
			if !errors.As(err, &ufe) {
				t.Fatalf("expected unsupportedFeature, got %T: %v", err, err)
			}
		})
	}
}

func TestChatToResponsesRejectsNonFunctionTools(t *testing.T) {
	// A Chat tool without the function wrapper, or with a non-function type,
	// must surface as an unsupportedFeature error rather than be converted
	// into a malformed Responses tool entry.
	cases := []string{
		// Non-function type.
		`{"model":"virtual/coding","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"code_interpreter"}]}`,
		// Function type with no nested function object.
		`{"model":"virtual/coding","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","name":"lookup"}]}`,
		// Function is not an object.
		`{"model":"virtual/coding","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":"lookup"}]}`,
	}
	for i, body := range cases {
		_, err := translateRequest([]byte(body), providers.ProtocolChat, providers.ProtocolResponses, "real-model")
		if err == nil {
			t.Fatalf("case %d: expected error, got nil", i)
		}
		var ufe unsupportedFeature
		if !errors.As(err, &ufe) {
			t.Fatalf("case %d: expected unsupportedFeature, got %T: %v", i, err, err)
		}
	}
}

func TestChatToResponsesOmitsEmptyAssistantTextWithToolCalls(t *testing.T) {
	body := []byte(`{"model":"virtual/coding","messages":[{"role":"user","content":"look up the weather"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_123","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"assistant","content":"I found it.","tool_calls":[{"id":"call_456","type":"function","function":{"name":"lookup","arguments":"{\"city\":\"Perth\"}"}}]},{"role":"tool","tool_call_id":"call_123","content":"sunny"}]}`)
	translated, err := translateRequest(body, providers.ProtocolChat, providers.ProtocolResponses, "real-model")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(translated, &got); err != nil {
		t.Fatal(err)
	}
	input := got["input"].([]any)
	if len(input) != 5 {
		t.Fatalf("input = %#v, want user, function_call, assistant message, function_call, function_call_output", input)
	}
	if input[1].(map[string]any)["type"] != "function_call" {
		t.Fatalf("input[1] = %#v, want function_call", input[1])
	}
	if input[2].(map[string]any)["type"] != "message" || input[2].(map[string]any)["role"] != "assistant" {
		t.Fatalf("input[2] = %#v, want assistant message", input[2])
	}
	content := input[2].(map[string]any)["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["type"] != "output_text" {
		t.Fatalf("input[2] content = %#v, want single output_text block", content)
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
			"id":     "resp",
			"object": "response",
			"model":  "test",
			"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "ok"}}}},
			"usage":  map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
		})
	}))
	t.Cleanup(dedicated.Close)

	api, _, _, _ := loggingTestHarness(t, mockUpstream(t))

	status, payload, _ := api.request("POST", "/api/admin/providers", map[string]any{
		"name":      "opencode-free-muse",
		"type":      "opencode-free",
		"base_url":  dedicated.URL + "/v1",
		"protocols": []string{"chat", "responses"},
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
			"model":      "opencode-free-muse/" + upstreamID,
			"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
			"max_tokens": 32,
			"stream":     false,
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

// TestFreeModelViaKeyedZenFailsLoud verifies the fail-loud guard: a free-tier
// model (mimo-v2.5-free) discovered through a keyed opencode-zen instance can
// never be served with a credential — the Zen relay rejects any unrecognized
// bearer with 401. A direct request fails with a 400 naming the keyless
// opencode-free provider (remediation); the attempt is never sent upstream.
func TestFreeModelViaKeyedZenFailsLoud(t *testing.T) {
	dedicated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": "list",
				"data": []any{
					map[string]any{"id": "mimo-v2.5-free", "object": "model"},
				},
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "should not reach upstream"}}},
		})
	}))
	t.Cleanup(dedicated.Close)

	api, _, _, _ := loggingTestHarness(t, mockUpstream(t))

	status, payload, _ := api.request("POST", "/api/admin/providers", map[string]any{
		"name":       "opencode-zen-keyed",
		"type":       "opencode-zen",
		"base_url":   dedicated.URL + "/v1",
		"credential": "zen-secret",
		"protocols":  []string{"chat", "responses", "messages"},
	})
	if status != 201 {
		t.Fatalf("create zen provider: %d %v", status, payload)
	}
	zenProviderID := payload["id"].(string)

	status, payload, _ = api.request("POST", "/api/admin/providers/"+zenProviderID+"/refresh", nil)
	if status != 200 && status != 204 {
		t.Fatalf("refresh zen provider: %d %v", status, payload)
	}
	status, payload, _ = api.request("GET", "/api/admin/providers/"+zenProviderID+"/models", nil)
	if status != 200 {
		t.Fatalf("list zen models: %d %v", status, payload)
	}
	var zenModelID string
	for _, raw := range payload["data"].([]any) {
		m := raw.(map[string]any)
		if m["upstream_model_id"] == "mimo-v2.5-free" {
			zenModelID = m["id"].(string)
		}
	}
	if zenModelID == "" {
		t.Fatalf("zen discovery did not surface the free model")
	}

	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "zen client", "type": "catalogue"})
	if status != 201 {
		t.Fatalf("create key: %d %v", status, payload)
	}
	clientID := payload["id"].(string)
	clientSecret := payload["secret"].(string)

	status, _, _ = api.request("PUT", "/api/admin/client-keys/"+clientID+"/permissions", map[string]any{
		"defaults":    []any{},
		"permissions": []any{map[string]any{"kind": "real", "model_id": zenModelID, "enabled": true}},
	})
	if status != 204 {
		t.Fatalf("permissions: %d", status)
	}

	chatBody, _ := json.Marshal(map[string]any{
		"model":      "opencode-zen-keyed/mimo-v2.5-free",
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		"max_tokens": 32,
		"stream":     false,
	})
	req, _ := http.NewRequest("POST", api.base+"/v1/chat/completions", bytes.NewReader(chatBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+clientSecret)
	resp, err := api.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	decoded := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&decoded)

	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400; payload=%v", resp.StatusCode, decoded)
	}
	errObj, _ := decoded["error"].(map[string]any)
	if errObj["code"] != "free_model_requires_keyless" {
		t.Fatalf("error code = %v, want free_model_requires_keyless; payload=%v", errObj["code"], decoded)
	}
}

// TestOpenCodeFreeMinOutputTokensRejectsExplicitLowLimit verifies the router
// never silently raises an explicit client max_tokens below the provider
// minimum (opencode-free requires >= 16): a direct request fails loud with a
// 400, while a request without the field gets the minimum supplied as the
// default and succeeds.
func TestOpenCodeFreeMinOutputTokensRejectsExplicitLowLimit(t *testing.T) {
	dedicated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": "list",
				"data":   []any{map[string]any{"id": "mimo-v2.5-free", "object": "model"}},
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "resp",
			"object":  "chat.completion",
			"model":   "test",
			"choices": []any{map[string]any{"message": map[string]any{"content": "ok"}}},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1},
		})
	}))
	t.Cleanup(dedicated.Close)

	api, _, _, _ := loggingTestHarness(t, mockUpstream(t))
	status, payload, _ := api.request("POST", "/api/admin/providers", map[string]any{
		"name":      "opencode-free-min",
		"type":      "opencode-free",
		"base_url":  dedicated.URL + "/v1",
		"protocols": []string{"chat", "responses"},
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
	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "min-test", "type": "catalogue"})
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

	call := func(body map[string]any) (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", api.base+"/v1/chat/completions", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+clientSecret)
		resp, err := api.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	model := "opencode-free-min/mimo-v2.5-free"
	// Explicit max_tokens below the provider minimum: loud 400, never silent.
	if status, out := call(map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "max_tokens": 8}); status != 400 {
		t.Fatalf("below-minimum max_tokens: expected 400, got %d (%v)", status, out)
	} else if errObj, ok := out["error"].(map[string]any); !ok || errObj["code"] != "unsupported_feature" {
		t.Fatalf("below-minimum max_tokens: expected unsupported_feature, got %v", out)
	}
	// No max_tokens: minimum supplied as default, request succeeds.
	if status, out := call(map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}); status != 200 {
		t.Fatalf("absent max_tokens: expected 200, got %d (%v)", status, out)
	}
	// At/above minimum: untouched, succeeds.
	if status, out := call(map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "max_tokens": 32}); status != 200 {
		t.Fatalf("above-minimum max_tokens: expected 200, got %d (%v)", status, out)
	}
}

// TestVirtualAllTargetsBelowMinOutputReturns400 verifies that when every
// target of a virtual model is skipped because the client's explicit
// max_tokens is below each provider minimum, the router returns 400
// unsupported_feature rather than a misleading 503 virtual_model_unavailable.
// Nothing is unavailable — the request simply cannot be represented.
func TestVirtualAllTargetsBelowMinOutputReturns400(t *testing.T) {
	// Each dedicated server acts as an opencode-free provider (>= 16 output
	// tokens). They expose distinct upstream model IDs so they can both be
	// attached as virtual targets. The -free suffix is required for the
	// opencode-free discovery filter to accept them.
	dedicatedA := httptest.NewServer(opusFreeHandler("llama-free"))
	t.Cleanup(dedicatedA.Close)
	dedicatedB := httptest.NewServer(opusFreeHandler("qwen-free"))
	t.Cleanup(dedicatedB.Close)

	api, _, _, _ := loggingTestHarness(t, mockUpstream(t))
	providerModelIDs := map[string]string{}
	for _, p := range []struct{ name, url string }{
		{"free-provider-a", dedicatedA.URL + "/v1"},
		{"free-provider-b", dedicatedB.URL + "/v1"},
	} {
		status, payload, _ := api.request("POST", "/api/admin/providers", map[string]any{
			"name":      p.name,
			"type":      "opencode-free",
			"base_url":  p.url,
			"protocols": []string{"chat", "responses"},
		})
		if status != 201 {
			t.Fatalf("create provider %s: %d %v", p.name, status, payload)
		}
		providerID := payload["id"].(string)
		status, payload, _ = api.request("POST", "/api/admin/providers/"+providerID+"/refresh", nil)
		if status != 200 && status != 204 {
			t.Fatalf("refresh %s: %d %v", p.name, status, payload)
		}
		status, payload, _ = api.request("GET", "/api/admin/providers/"+providerID+"/models", nil)
		if status != 200 {
			t.Fatalf("list models %s: %d %v", p.name, status, payload)
		}
		for _, raw := range payload["data"].([]any) {
			m := raw.(map[string]any)
			if upID, ok := m["upstream_model_id"].(string); ok {
				if id, ok := m["id"].(string); ok {
					providerModelIDs[upID] = id
				}
			}
		}
	}

	status, payload, _ := api.request("POST", "/api/admin/virtual-groups", map[string]any{"name": "min-group"})
	if status != 201 {
		t.Fatalf("create group: %d %v", status, payload)
	}
	groupID := payload["id"].(string)
	status, payload, _ = api.request("POST", "/api/admin/virtual-models", map[string]any{
		"group_id":     groupID,
		"name":         "min-vm",
		"routing_mode": "ordered_fallback",
		"targets": []any{
			map[string]any{"provider_model_id": providerModelIDs["llama-free"], "enabled": true},
			map[string]any{"provider_model_id": providerModelIDs["qwen-free"], "enabled": true},
		},
	})
	if status != 201 {
		t.Fatalf("create virtual: %d %v", status, payload)
	}
	virtualID := payload["id"].(string)

	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "min-vm-client", "type": "catalogue"})
	if status != 201 {
		t.Fatalf("create key: %d %v", status, payload)
	}
	clientID := payload["id"].(string)
	clientSecret := payload["secret"].(string)
	status, _, _ = api.request("PUT", "/api/admin/client-keys/"+clientID+"/permissions", map[string]any{
		"defaults":    []any{},
		"permissions": []any{map[string]any{"kind": "virtual", "model_id": virtualID, "enabled": true}},
	})
	if status != 204 {
		t.Fatalf("permissions: %d", status)
	}

	call := func(body map[string]any) (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", api.base+"/v1/chat/completions", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+clientSecret)
		resp, err := api.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// Every target has min 16; client asks for 8. Both targets are skipped.
	// Expect 400 unsupported_feature, not 503 virtual_model_unavailable.
	if status, out := call(map[string]any{
		"model":      "min-group/min-vm",
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		"max_tokens": 8,
	}); status != 400 {
		t.Fatalf("all-skipped virtual: expected 400, got %d (%v)", status, out)
	} else if errObj, ok := out["error"].(map[string]any); !ok || errObj["code"] != "unsupported_feature" {
		t.Fatalf("all-skipped virtual: expected unsupported_feature, got %v", out)
	}

	// A later target satisfying the request still wins (fallback preserved).
	if status, out := call(map[string]any{
		"model":      "min-group/min-vm",
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		"max_tokens": 32,
	}); status != 200 {
		t.Fatalf("satisfied virtual: expected 200, got %d (%v)", status, out)
	}
}

// recordingChatUpstream serves a catalogue with the given model IDs, records
// every chat-completions request body, and answers with minimal content.
func recordingChatUpstream(modelIDs []string, seen *[]string, mu *sync.Mutex) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			data := []any{}
			for _, id := range modelIDs {
				data = append(data, map[string]any{"id": id, "object": "model"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		*seen = append(*seen, string(body))
		mu.Unlock()
		var input map[string]any
		_ = json.Unmarshal(body, &input)
		model, _ := input["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "resp",
			"object":  "chat.completion",
			"model":   model,
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1},
		})
	})
}

func postChat(t *testing.T, api *testAPI, secret, model string, extra map[string]any) (int, map[string]any) {
	t.Helper()
	body := map[string]any{
		"model":    model,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	for k, v := range extra {
		body[k] = v
	}
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", api.base+"/v1/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := api.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestPlainChatDefaultDisableReachesUpstream verifies B2 end to end: a plain
// Chat request to a disablable target arrives upstream with an explicit
// disable, while explicit client selectors and unknown caps pass through.
func TestPlainChatDefaultDisableReachesUpstream(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	api, db, _, clientSecret := loggingTestHarness(t, recordingChatUpstream([]string{"model-a"}, &seen, &mu))

	setCaps := func(caps string) {
		t.Helper()
		if _, err := db.SQL.Exec(`UPDATE provider_models SET reasoning_capabilities=? WHERE upstream_model_id='model-a'`, caps); err != nil {
			t.Fatal(err)
		}
	}
	lastSeen := func() string {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if len(seen) == 0 {
			t.Fatal("upstream saw no requests")
		}
		return seen[len(seen)-1]
	}

	// Disablable target: plain chat gains reasoning_effort:none.
	setCaps(`{"options":[{"type":"effort","values":["none","low","high"]}]}`)
	if status, _ := postChat(t, api, clientSecret, "provider-a/model-a", nil); status != 200 {
		t.Fatalf("plain chat: status %d", status)
	}
	if got := lastSeen(); !strings.Contains(got, `"reasoning_effort":"none"`) {
		t.Fatalf("plain chat missing injected disable: %s", got)
	}

	// Explicit client selector wins and is never overwritten with none.
	if status, _ := postChat(t, api, clientSecret, "provider-a/model-a", map[string]any{"reasoning_effort": "high"}); status != 200 {
		t.Fatalf("explicit effort: status %d", status)
	}
	if got := lastSeen(); !strings.Contains(got, `"reasoning_effort":"high"`) || strings.Contains(got, `"none"`) {
		t.Fatalf("explicit selector not preserved: %s", got)
	}

	// Unknown caps: body passes through with no invented selector.
	if _, err := db.SQL.Exec(`UPDATE provider_models SET reasoning_capabilities=NULL WHERE upstream_model_id='model-a'`); err != nil {
		t.Fatal(err)
	}
	if status, _ := postChat(t, api, clientSecret, "provider-a/model-a", nil); status != 200 {
		t.Fatalf("unknown caps: status %d", status)
	}
	if got := lastSeen(); strings.Contains(got, "reasoning") {
		t.Fatalf("unknown caps must not gain a selector: %s", got)
	}
}

// TestMandatoryReasoningSkipsVirtualTarget verifies B2 honesty: a mandatory-
// reasoning target cannot serve plain chat, so a virtual route falls through
// to the next target, and a direct request fails loud with
// unsupported_feature instead of returning empty content.
func TestMandatoryReasoningSkipsVirtualTarget(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	api, db, _, _ := loggingTestHarness(t, recordingChatUpstream([]string{"model-a", "model-b"}, &seen, &mu))

	if _, err := db.SQL.Exec(`UPDATE provider_models SET reasoning_capabilities=? WHERE upstream_model_id='model-a'`, `{"options":[{"type":"effort","values":["none","low","high"]}]}`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`UPDATE provider_models SET reasoning_capabilities=? WHERE upstream_model_id='model-b'`, `{"options":[{"type":"effort","values":["high"]}],"mandatory":true}`); err != nil {
		t.Fatal(err)
	}
	var modelAID, modelBID string
	if err := db.SQL.QueryRow(`SELECT id FROM provider_models WHERE upstream_model_id='model-a'`).Scan(&modelAID); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL.QueryRow(`SELECT id FROM provider_models WHERE upstream_model_id='model-b'`).Scan(&modelBID); err != nil {
		t.Fatal(err)
	}

	status, payload, _ := api.request("POST", "/api/admin/virtual-groups", map[string]any{"name": "reason-vg"})
	if status != 201 {
		t.Fatalf("create group: %d %v", status, payload)
	}
	groupID := payload["id"].(string)
	status, payload, _ = api.request("POST", "/api/admin/virtual-models", map[string]any{
		"group_id":     groupID,
		"name":         "reason-vm",
		"routing_mode": "ordered_fallback",
		"targets": []any{
			map[string]any{"provider_model_id": modelBID, "enabled": true},
			map[string]any{"provider_model_id": modelAID, "enabled": true},
		},
	})
	if status != 201 {
		t.Fatalf("create virtual: %d %v", status, payload)
	}
	virtualID := payload["id"].(string)

	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "reason-vm-client", "type": "catalogue"})
	if status != 201 {
		t.Fatalf("create key: %d %v", status, payload)
	}
	vmClientID := payload["id"].(string)
	vmSecret := payload["secret"].(string)
	status, _, _ = api.request("PUT", "/api/admin/client-keys/"+vmClientID+"/permissions", map[string]any{
		"defaults":    []any{},
		"permissions": []any{map[string]any{"kind": "virtual", "model_id": virtualID, "enabled": true}},
	})
	if status != 204 {
		t.Fatalf("permissions: %d", status)
	}

	// Mandatory first target is skipped; the disablable second target serves.
	mu.Lock()
	seen = nil
	mu.Unlock()
	if status, out := postChat(t, api, vmSecret, "reason-vg/reason-vm", nil); status != 200 {
		t.Fatalf("virtual fallback: expected 200, got %d (%v)", status, out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("expected exactly one upstream call (mandatory skipped), got %d: %v", len(seen), seen)
	}
	if strings.Contains(seen[0], `"model":"model-b"`) {
		t.Fatalf("mandatory target must be skipped, but upstream saw: %s", seen[0])
	}
	if !strings.Contains(seen[0], `"model":"model-a"`) {
		t.Fatalf("fallback target not reached upstream: %s", seen[0])
	}
	if !strings.Contains(seen[0], `"reasoning_effort":"none"`) {
		t.Fatalf("fallback target missing injected disable: %s", seen[0])
	}

	// Direct request to the mandatory model fails loud, never hits upstream.
	before := len(seen)
	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "direct-client", "type": "catalogue"})
	if status != 201 {
		t.Fatalf("create direct key: %d %v", status, payload)
	}
	directID := payload["id"].(string)
	directSecret := payload["secret"].(string)
	status, _, _ = api.request("PUT", "/api/admin/client-keys/"+directID+"/permissions", map[string]any{
		"defaults":    []any{},
		"permissions": []any{map[string]any{"kind": "real", "model_id": modelBID, "enabled": true}},
	})
	if status != 204 {
		t.Fatalf("direct permissions: %d", status)
	}
	if status, out := postChat(t, api, directSecret, "provider-a/model-b", nil); status != 400 {
		t.Fatalf("direct mandatory: expected 400, got %d (%v)", status, out)
	} else if errObj, ok := out["error"].(map[string]any); !ok || errObj["code"] != "unsupported_feature" {
		t.Fatalf("direct mandatory: expected unsupported_feature, got %v", out)
	}
	if len(seen) != before {
		t.Fatalf("direct mandatory must not reach upstream")
	}
}

// TestExplicitSelectorSameProtocolChatValidated covers B3a: an explicit
// reasoning selector on a same-protocol Chat target is checked against that
// target's capabilities — advertised efforts are forwarded verbatim,
// unadvertised efforts are stripped, and unknown capabilities are never
// assumed to accept the value (selector stripped, provider default applies).
func TestExplicitSelectorSameProtocolChatValidated(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	api, db, _, clientSecret := loggingTestHarness(t, recordingChatUpstream([]string{"model-a"}, &seen, &mu))

	setCaps := func(caps string) {
		t.Helper()
		if _, err := db.SQL.Exec(`UPDATE provider_models SET reasoning_capabilities=? WHERE upstream_model_id='model-a'`, caps); err != nil {
			t.Fatal(err)
		}
	}
	lastSeen := func() string {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if len(seen) == 0 {
			t.Fatal("upstream saw no requests")
		}
		return seen[len(seen)-1]
	}

	// Known caps advertising minimal: forwarded verbatim.
	setCaps(`{"options":[{"type":"effort","values":["minimal","low","medium","high","xhigh"]}]}`)
	if status, _ := postChat(t, api, clientSecret, "provider-a/model-a", map[string]any{"reasoning_effort": "minimal"}); status != 200 {
		t.Fatalf("advertised minimal: status %d", status)
	}
	if got := lastSeen(); !strings.Contains(got, `"reasoning_effort":"minimal"`) {
		t.Fatalf("advertised effort was not forwarded: %s", got)
	}

	// Known caps without minimal: unadvertised effort stripped.
	setCaps(`{"options":[{"type":"effort","values":["low","medium","high","xhigh"]}]}`)
	if status, _ := postChat(t, api, clientSecret, "provider-a/model-a", map[string]any{"reasoning_effort": "minimal"}); status != 200 {
		t.Fatalf("unadvertised minimal: status %d", status)
	}
	if got := lastSeen(); strings.Contains(got, "reasoning") {
		t.Fatalf("unadvertised effort must be stripped, got: %s", got)
	}

	// Unknown caps: selector stripped (B3a) — no assumed acceptance.
	if _, err := db.SQL.Exec(`UPDATE provider_models SET reasoning_capabilities=NULL WHERE upstream_model_id='model-a'`); err != nil {
		t.Fatal(err)
	}
	for _, effort := range []string{"minimal", "high"} {
		if status, _ := postChat(t, api, clientSecret, "provider-a/model-a", map[string]any{"reasoning_effort": effort}); status != 200 {
			t.Fatalf("unknown caps effort %s: status %d", effort, status)
		}
		if got := lastSeen(); strings.Contains(got, "reasoning") {
			t.Fatalf("unknown caps must not receive effort %s: %s", effort, got)
		}
	}

	// A non-selector field like reasoning.exclude must survive the strip.
	raw, _ := json.Marshal(map[string]any{
		"model":     "provider-a/model-a",
		"messages":  []any{map[string]any{"role": "user", "content": "hi"}},
		"reasoning": map[string]any{"exclude": true},
	})
	req, _ := http.NewRequest("POST", api.base+"/v1/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+clientSecret)
	resp, err := api.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := lastSeen(); !strings.Contains(got, `"exclude":true`) {
		t.Fatalf("non-selector reasoning field must be preserved: %s", got)
	}

	// Explicit selector never triggers B2's default-disable injection.
	setCaps(`{"options":[{"type":"effort","values":["none","low","high"]}]}`)
	if status, _ := postChat(t, api, clientSecret, "provider-a/model-a", map[string]any{"reasoning_effort": "high"}); status != 200 {
		t.Fatalf("explicit high: status %d", status)
	}
	if got := lastSeen(); !strings.Contains(got, `"reasoning_effort":"high"`) || strings.Contains(got, `"none"`) {
		t.Fatalf("explicit selector must not be overwritten by default-disable: %s", got)
	}
}

// opusFreeHandler returns an http.Handler that serves a catalogue exposing the
// given model ID and answers chat completions — modelling an opencode-free
// provider (MinOutputTokens: 16) for tests.
func opusFreeHandler(modelID string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": "list",
				"data":   []any{map[string]any{"id": modelID, "object": "model"}},
			})
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "resp",
			"object":  "chat.completion",
			"model":   modelID,
			"choices": []any{map[string]any{"message": map[string]any{"content": "ok"}}},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1},
		})
	})
}

func TestCheckMinOutputTokens(t *testing.T) {
	// Absent field: minimum supplied, compatible.
	out, ok, err := checkMinOutputTokens([]byte(`{"model":"m"}`), 16, providers.ProtocolChat)
	if err != nil || !ok {
		t.Fatalf("absent field: ok=%v err=%v", ok, err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil || parsed["max_tokens"] != float64(16) {
		t.Fatalf("absent field: minimum not supplied: %s err=%v", out, err)
	}
	// Explicit below minimum: incompatible, body untouched.
	raw := []byte(`{"model":"m","max_tokens":8}`)
	out, ok, err = checkMinOutputTokens(raw, 16, providers.ProtocolChat)
	if err != nil || ok {
		t.Fatalf("below minimum: ok=%v err=%v, want incompatible", ok, err)
	}
	if string(out) != string(raw) {
		t.Fatalf("below minimum: body must be untouched, got %s", out)
	}
	// At/above minimum: untouched, compatible.
	for _, body := range []string{`{"model":"m","max_tokens":16}`, `{"model":"m","max_tokens":64}`} {
		out, ok, err := checkMinOutputTokens([]byte(body), 16, providers.ProtocolChat)
		if err != nil || !ok || string(out) != body {
			t.Fatalf("above minimum %s: ok=%v err=%v out=%s", body, ok, err, out)
		}
	}
	// Responses protocol uses max_output_tokens.
	out, ok, err = checkMinOutputTokens([]byte(`{"model":"m"}`), 16, providers.ProtocolResponses)
	if err != nil || !ok {
		t.Fatalf("responses absent: ok=%v err=%v", ok, err)
	}
	if err := json.Unmarshal(out, &parsed); err != nil || parsed["max_output_tokens"] != float64(16) {
		t.Fatalf("responses absent: minimum not supplied: %s", out)
	}
	if _, ok, _ := checkMinOutputTokens([]byte(`{"model":"m","max_output_tokens":4}`), 16, providers.ProtocolResponses); ok {
		t.Fatal("responses below minimum must be incompatible")
	}
}
