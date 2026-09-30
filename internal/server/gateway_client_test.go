package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	gateway "github.com/tiller-router/tiller-router/clients/go"
	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
)

type gatewayClientFixture struct {
	client    *gateway.Client
	base      string
	requests  chan map[string]any
	cancelled chan struct{}
}

func newGatewayClientFixture(t *testing.T, capabilities bool) *gatewayClientFixture {
	t.Helper()
	f := &gatewayClientFixture{requests: make(chan map[string]any, 16), cancelled: make(chan struct{}, 1)}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer gateway-provider-fixture" {
			t.Error("upstream did not receive the stored provider credential")
		}
		if r.URL.Path == "/v1/models" {
			model := map[string]any{"id": "gateway-model", "supported_endpoints": []string{"/v1/chat/completions"}}
			if capabilities {
				model["supported_parameters"] = []string{"tools", "structured_outputs", "response_format"}
				model["architecture"] = map[string]any{"input_modalities": []string{"text", "image"}, "output_modalities": []string{"text"}}
				model["context_length"] = 32768
				model["max_output_tokens"] = 4096
			}
			if err := json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{model, map[string]any{"id": "hidden-model"}}}); err != nil {
				t.Error(err)
			}
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected upstream endpoint %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		var input map[string]any
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.requests <- input
		if input["model"] != "gateway-model" {
			t.Errorf("upstream model = %v, want gateway-model", input["model"])
		}
		messages, _ := input["messages"].([]any)
		last, _ := messages[len(messages)-1].(map[string]any)
		if input["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			frames := []string{
				`{"id":"stream-fixture","object":"chat.completion.chunk","model":"gateway-model","native_frame":"preserved","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_lookup","type":"function","function":{"name":"lookup","arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
				`{"id":"stream-fixture","object":"chat.completion.chunk","model":"gateway-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Perth\"}"}}]},"finish_reason":"tool_calls"}]}`,
				`{"id":"stream-fixture","object":"chat.completion.chunk","model":"gateway-model","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":4}}}`,
			}
			for i, frame := range frames {
				if _, err := fmt.Fprintf(w, "data: %s\n\n", frame); err != nil {
					return
				}
				flusher.Flush()
				if i == 0 && last["content"] == "cancel-fixture" {
					select {
					case <-r.Context().Done():
						f.cancelled <- struct{}{}
					case <-time.After(5 * time.Second):
					}
					return
				}
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		message := map[string]any{"role": "assistant", "content": nil, "native_assistant": map[string]any{"signature": "fixture"}, "tool_calls": []any{map[string]any{"id": "call_lookup", "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"city":"Perth"}`}}}}
		finish := "tool_calls"
		if last["role"] == "tool" {
			message = map[string]any{"role": "assistant", "content": "Sunny in Perth"}
			finish = "stop"
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"id": "chat-fixture", "object": "chat.completion", "model": "gateway-model", "native_response": "preserved", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": map[string]any{"prompt_tokens": 12, "completion_tokens": 3, "total_tokens": 15}}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(upstream.Close)
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	app := newTestServer(t, config.Config{TillerUser: "admin", TillerUserPassword: "gateway-admin-fixture", DataDir: t.TempDir(), ListenAddr: ":8080"}, db)
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)
	f.base = router.URL + "/v1"
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	api := &testAPI{t: t, base: router.URL, client: &http.Client{Jar: jar}, server: app}
	admin := func(method, path string, body any, want int) map[string]any {
		t.Helper()
		status, payload, _ := api.request(method, path, body)
		if status != want {
			t.Fatalf("%s %s: status %d, want %d", method, path, status, want)
		}
		return payload
	}
	login := admin("POST", "/api/admin/session", map[string]any{"username": "admin", "password": "gateway-admin-fixture"}, 200)
	api.csrf = login["csrf_token"].(string)
	provider := admin("POST", "/api/admin/providers", map[string]any{"name": "gateway-provider", "type": "generic-openai", "base_url": upstream.URL + "/v1", "credential": "gateway-provider-fixture"}, 201)
	providerID := provider["id"].(string)
	models := admin("GET", "/api/admin/providers/"+providerID+"/models", nil, 200)
	var targetID string
	for _, raw := range models["data"].([]any) {
		model := raw.(map[string]any)
		if model["upstream_model_id"] == "gateway-model" {
			targetID = model["id"].(string)
		}
	}
	if targetID == "" {
		t.Fatal("live upstream discovery omitted gateway-model")
	}
	group := admin("POST", "/api/admin/virtual-groups", map[string]any{"name": "virtual"}, 201)
	virtual := admin("POST", "/api/admin/virtual-models", map[string]any{"group_id": group["id"], "name": "free", "target_provider_id": providerID, "target_model_id": targetID}, 201)
	key := admin("POST", "/api/admin/client-keys", map[string]any{"name": "gateway integration", "type": "catalogue"}, 201)
	admin("PUT", "/api/admin/client-keys/"+key["id"].(string)+"/permissions", map[string]any{"defaults": []any{}, "permissions": []any{map[string]any{"kind": "virtual", "model_id": virtual["id"], "enabled": true}}}, 204)
	f.client, err = gateway.New(gateway.Config{Profile: gateway.Tiller, BaseURL: f.base, APIKey: key["secret"].(string), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.client.Close)
	return f
}

func (f *gatewayClientFixture) received(t *testing.T) map[string]any {
	t.Helper()
	select {
	case request := <-f.requests:
		return request
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not receive request")
		return nil
	}
}

func gatewayClientEqualJSON(t *testing.T, got, want any) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var gotValue, wantValue any
	if err := json.Unmarshal(gotJSON, &gotValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(wantJSON, &wantValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("native JSON changed: got %s, want %s", gotJSON, wantJSON)
	}
}

func TestGatewayClientCatalog(t *testing.T) {
	for _, known := range []bool{true, false} {
		t.Run(fmt.Sprintf("capabilities-known-%t", known), func(t *testing.T) {
			f := newGatewayClientFixture(t, known)
			models, err := f.client.ListModels(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(models) != 1 || models[0].ID != "virtual/free" {
				t.Fatalf("catalog leaked real models or lost canonical virtual ID: %v", models)
			}
			m := models[0]
			want := gateway.Unknown
			if known {
				want = gateway.Supported
				if m.ContextLength == nil || *m.ContextLength != 32768 || m.MaxOutputTokens == nil || *m.MaxOutputTokens != 4096 {
					t.Fatal("catalog lost live model limits")
				}
			}
			if m.Tools != want || m.Vision != want || m.StructuredOutput != want {
				t.Fatalf("capabilities tools=%v vision=%v structured=%v, want %v", m.Tools, m.Vision, m.StructuredOutput, want)
			}
			filtered := gateway.FilterModels(models, gateway.Requirements{Tools: true, Vision: true, StructuredOutput: true})
			if known && len(filtered) != 1 || !known && len(filtered) != 0 {
				t.Fatal("capability filter treated unknown as supported")
			}
			bad, err := gateway.New(gateway.Config{Profile: gateway.Tiller, BaseURL: f.base, APIKey: "invalid-fixture-key"})
			if err != nil {
				t.Fatal(err)
			}
			defer bad.Close()
			_, err = bad.ListModels(context.Background())
			var sdkErr *gateway.Error
			if !errors.As(err, &sdkErr) || sdkErr.Status != 401 || sdkErr.Category != "authentication" {
				t.Fatalf("unauthenticated catalog error = %v", err)
			}
			for _, model := range []string{"gateway-provider/gateway-model", "gateway-provider/hidden-model", "free"} {
				_, err = f.client.Chat(context.Background(), gateway.Request{Model: model, Messages: []gateway.Message{{"role": "user", "content": "hidden"}}})
				if !errors.As(err, &sdkErr) || sdkErr.Status != 404 || sdkErr.Category != "model_unavailable" {
					t.Fatalf("unpermitted/noncanonical model %q error = %v", model, err)
				}
			}
			select {
			case <-f.requests:
				t.Fatal("unpermitted request reached upstream")
			default:
			}
		})
	}
}

func TestGatewayClientChatToolRoundTrip(t *testing.T) {
	f := newGatewayClientFixture(t, true)
	strict := true
	schema := map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []string{"city"}, "additionalProperties": false}
	request := gateway.Request{
		Model:          "virtual/free",
		Messages:       []gateway.Message{{"role": "user", "content": []any{map[string]any{"type": "text", "text": "Weather?"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,aGVsbG8=", "detail": "high"}}}}},
		Tools:          []gateway.Tool{{Type: "function", Function: gateway.ToolFunction{Name: "lookup", Parameters: schema, Strict: &strict}}},
		ToolChoice:     map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}},
		ResponseFormat: map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "weather", "strict": true, "schema": schema}},
		Extra:          map[string]any{"native_extension": map[string]any{"opaque": []string{"keep", "me"}}},
	}
	result, err := f.client.Chat(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	input := f.received(t)
	for name, want := range map[string]any{"messages": request.Messages, "tools": request.Tools, "tool_choice": request.ToolChoice, "response_format": request.ResponseFormat, "native_extension": request.Extra["native_extension"]} {
		gatewayClientEqualJSON(t, input[name], want)
	}
	if result.Model != "virtual/free" || len(result.Choices) != 1 || result.Choices[0].FinishReason != "tool_calls" {
		t.Fatal("tool response lost virtual alias or finish reason")
	}
	choice := result.Choices[0]
	if len(choice.ToolCalls) != 1 || choice.ToolCalls[0].ID != "call_lookup" || choice.ToolCalls[0].Function.Name != "lookup" || choice.ToolCalls[0].Function.Arguments != `{"city":"Perth"}` {
		t.Fatal("tool response lost ID, name, or JSON arguments")
	}
	gatewayClientEqualJSON(t, choice.Message["native_assistant"], map[string]any{"signature": "fixture"})
	var raw map[string]any
	if err := json.Unmarshal(result.Raw, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["native_response"] != "preserved" || result.Usage == nil || result.Usage.TotalTokens == nil || *result.Usage.TotalTokens != 15 {
		t.Fatal("response lost native fields or usage")
	}
	request.Messages = append(request.Messages, choice.Message, gateway.Message{"role": "tool", "tool_call_id": choice.ToolCalls[0].ID, "content": "sunny"})
	result, err = f.client.Chat(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	gatewayClientEqualJSON(t, f.received(t)["messages"], request.Messages)
	if result.Model != "virtual/free" || len(result.Choices) != 1 || result.Choices[0].Content != "Sunny in Perth" || result.Choices[0].FinishReason != "stop" {
		t.Fatal("assistant/tool-result roundtrip did not return final text")
	}
}

func TestGatewayClientStream(t *testing.T) {
	f := newGatewayClientFixture(t, true)
	stream, err := f.client.Stream(context.Background(), gateway.Request{Model: "virtual/free", Messages: []gateway.Message{{"role": "user", "content": "stream-fixture"}}, Tools: []gateway.Tool{{Type: "function", Function: gateway.ToolFunction{Name: "lookup", Parameters: map[string]any{"type": "object"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	gatewayClientEqualJSON(t, f.received(t)["stream_options"], map[string]any{"include_usage": true})
	var fragments []string
	var kinds []gateway.EventKind
	var frames int
	for {
		event, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch event.Kind {
		case gateway.FrameEvent:
			frames++
			var frame map[string]any
			if err := json.Unmarshal(event.Raw, &frame); err != nil {
				t.Fatal(err)
			}
			if frame["model"] != "virtual/free" {
				t.Fatal("stream leaked upstream model instead of virtual alias")
			}
			if frames == 1 && frame["native_frame"] != "preserved" {
				t.Fatal("stream lost native extension")
			}
		case gateway.ToolEvent:
			if event.Tool == nil || event.Tool.Index != 0 || event.ChoiceIndex != 0 {
				t.Fatal("stream lost tool/choice index")
			}
			if len(fragments) == 0 && (event.Tool.ID != "call_lookup" || event.Tool.Function.Name != "lookup") {
				t.Fatal("stream lost tool ID or name")
			}
			fragments = append(fragments, event.Tool.Function.Arguments)
		case gateway.FinishEvent:
			if event.FinishReason != "tool_calls" {
				t.Fatal("stream lost finish reason")
			}
		case gateway.UsageEvent:
			u := event.Usage
			if u == nil || u.PromptTokens == nil || *u.PromptTokens != 12 || u.CompletionTokens == nil || *u.CompletionTokens != 3 || u.TotalTokens == nil || *u.TotalTokens != 15 || u.CacheReadTokens == nil || *u.CacheReadTokens != 4 {
				t.Fatal("stream lost usage accounting")
			}
		}
		kinds = append(kinds, event.Kind)
	}
	if !reflect.DeepEqual(fragments, []string{`{"city":`, `"Perth"}`}) {
		t.Fatalf("tool fragments = %q", fragments)
	}
	wantKinds := []gateway.EventKind{gateway.FrameEvent, gateway.ToolEvent, gateway.FrameEvent, gateway.ToolEvent, gateway.FinishEvent, gateway.FrameEvent, gateway.UsageEvent, gateway.CompletionEvent}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Fatalf("event order = %v, want %v", kinds, wantKinds)
	}
}

func TestGatewayClientStreamCancellation(t *testing.T) {
	f := newGatewayClientFixture(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := f.client.Stream(ctx, gateway.Request{Model: "virtual/free", Messages: []gateway.Message{{"role": "user", "content": "cancel-fixture"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	f.received(t)
	if _, err := stream.Next(); err != nil {
		t.Fatal(err)
	}
	cancel()
	_, err = stream.Next()
	var sdkErr *gateway.Error
	if !errors.As(err, &sdkErr) || sdkErr.Category != "cancelled" {
		t.Fatalf("cancelled stream error = %v", err)
	}
	select {
	case <-f.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("SDK cancellation did not reach upstream through Tiller")
	}
}
