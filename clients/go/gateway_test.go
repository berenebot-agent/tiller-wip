package gateway_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gateway "github.com/tiller-router/tiller-router/clients/go"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, e := os.ReadFile("../fixtures/" + name)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func client(t *testing.T, s *httptest.Server, edit func(*gateway.Config)) *gateway.Client {
	t.Helper()
	cfg := gateway.Config{Profile: gateway.Tiller, BaseURL: s.URL + "/prefix/v1", APIKey: "test-secret", Timeout: time.Second}
	if edit != nil {
		edit(&cfg)
	}
	c, e := gateway.New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(c.Close)
	return c
}
func request() gateway.Request {
	return gateway.Request{Model: "virtual/free", Messages: []gateway.Message{{"role": "user", "content": "private-prompt"}}}
}

func TestCatalogFixturesAndCacheIsolation(t *testing.T) {
	tiller := fixture(t, "tiller-models.json")
	openrouter := fixture(t, "openrouter-models.json")
	var calls atomic.Int32
	var fail atomic.Bool
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if fail.Load() {
			w.WriteHeader(503)
			return
		}
		if r.URL.Path == "/prefix/v1/models/user" {
			w.Write(openrouter)
			return
		}
		if r.URL.Path != "/prefix/v1/models" {
			t.Errorf("wrong path %s", r.URL.Path)
		}
		w.Write(tiller)
	}))
	defer s.Close()
	c := client(t, s, nil)
	models, e := c.ListModels(context.Background())
	if e != nil || len(models) != 4 {
		t.Fatalf("catalog: %v", e)
	}
	if models[0].Tools != gateway.Supported || models[0].StructuredOutput != gateway.Unsupported || models[1].Vision != gateway.Unknown {
		t.Fatal("tri-state flags")
	}
	if models[1].ReasoningOptions.EffortsKnown || !models[2].ReasoningOptions.EffortsUnrestricted || models[3].ReasoningOptions.EffortsUnrestricted || !models[3].ReasoningOptions.EffortsKnown || len(models[3].ReasoningOptions.Efforts) != 0 {
		t.Fatal("effort semantics")
	}
	if len(gateway.FilterModels(models, gateway.Requirements{Tools: true, MinContext: 100})) != 1 {
		t.Fatal("filter")
	}
	stamp := c.CatalogCachedAt()
	models[0].ID = "mutated"
	*models[0].ContextLength = 1
	models[0].ReasoningOptions.Efforts[0] = "mutated"
	again, e := c.ListModels(context.Background())
	if e != nil || again[0].ID != "virtual/free" || *again[0].ContextLength != 131072 || again[0].ReasoningOptions.Efforts[0] != "low" || calls.Load() != 1 {
		t.Fatal("cache alias")
	}
	fail.Store(true)
	if _, e = c.RefreshModels(context.Background()); e == nil {
		t.Fatal("refresh failure hidden")
	}
	if c.CatalogCachedAt() != stamp {
		t.Fatal("refresh altered timestamp")
	}
	if _, e = c.ListModels(context.Background()); e != nil {
		t.Fatal(e)
	}
	fail.Store(false)
	other := client(t, s, func(cfg *gateway.Config) { cfg.APIKey = "other-principal" })
	if _, e = other.ListModels(context.Background()); e != nil || calls.Load() != 3 {
		t.Fatal("principal cache isolation")
	}
	or := client(t, s, func(cfg *gateway.Config) { cfg.Profile = gateway.OpenRouter })
	om, e := or.ListModels(context.Background())
	if e != nil || len(om) != 3 || om[0].Vision != gateway.Supported || om[1].Tools != gateway.Unsupported || om[2].Tools != gateway.Unknown || *om[0].MaxOutputTokens != 16000 {
		t.Fatal("OpenRouter normalization")
	}
}

func TestChatReplayExtensionsAndSafety(t *testing.T) {
	result := fixture(t, "chat-result.json")
	var received map[string]any
	var calls int
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer test-secret" || r.URL.Path != "/prefix/v1/chat/completions" {
			t.Error("routing/auth")
		}
		if e := json.NewDecoder(r.Body).Decode(&received); e != nil {
			t.Error(e)
		}
		w.Header().Set("X-Request-ID", "req-1")
		w.Write(result)
	}))
	defer s.Close()
	c := client(t, s, nil)
	req := request()
	req.Extra = map[string]any{"native_option": map[string]any{"enabled": true}}
	req.ResponseFormat = map[string]any{"type": "json_schema", "json_schema": map[string]any{"strict": true, "schema": map[string]any{"type": "object", "additionalProperties": false}}}
	req.Reasoning = map[string]any{"effort": "low"}
	out, e := c.Chat(context.Background(), req)
	if e != nil {
		t.Fatal(e)
	}
	if out.Usage == nil || *out.Usage.TotalTokens != 20 || out.RequestID != "req-1" || out.Choices[0].ToolCalls[0].Function.Arguments != `{"value":1}` {
		t.Fatal("result")
	}
	if _, ok := out.Choices[0].Message["reasoning_details"]; !ok {
		t.Fatal("opaque replay lost")
	}
	req.Messages = append(req.Messages, out.Choices[0].Message)
	if _, e = c.Chat(context.Background(), req); e != nil {
		t.Fatal(e)
	}
	if received["stream"] != false || received["native_option"] == nil {
		t.Fatal("wire fields")
	}
	for _, key := range []string{"model", "stream", "tools", "Authorization", "api_key", "response_format"} {
		req.Extra = map[string]any{key: "bad"}
		if _, e = c.Chat(context.Background(), req); e == nil {
			t.Fatal("extension conflict", key)
		}
	}
	if calls != 2 {
		t.Fatal("conflicting request sent")
	}
	if strings.Contains(fmt.Sprintf("%+v %#v %+v", req, out, out.Choices[0].Message), "private-prompt") {
		t.Fatal("representation leak")
	}
}

func TestErrorsRedirectsAndPagination(t *testing.T) {
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer target.Close()
	for _, mode := range []string{"error", "redirect", "pagination"} {
		t.Run(mode, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "error":
					w.Header().Set("X-Request-ID", "safe-id")
					w.Header().Set("Retry-After", "2")
					w.WriteHeader(429)
					io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","message":"test-secret private-prompt","type":"test-secret"}}`)
				case "redirect":
					http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
				case "pagination":
					fmt.Fprintf(w, `{"data":[],"links":{"next":%q}}`, target.URL)
				}
			}))
			defer s.Close()
			c := client(t, s, func(cfg *gateway.Config) { cfg.HTTPClient = s.Client() })
			_, e := c.RefreshModels(context.Background())
			if e == nil {
				t.Fatal("expected failure")
			}
			if strings.Contains(fmt.Sprintf("%+v %#v", e, e), "test-secret") || strings.Contains(e.Error(), "private-prompt") {
				t.Fatal("error leak")
			}
			if mode == "error" {
				ge, ok := e.(*gateway.Error)
				if !ok || ge.Category != "rate_limit" || ge.RetryAfter != "2" || ge.RequestID != "safe-id" {
					t.Fatal("error metadata")
				}
			}
		})
	}
	if leaked.Load() != 0 {
		t.Fatal("credential redirect/navigation leak")
	}
	for _, key := range []string{"bad\nkey", "bad\x00key", ""} {
		if _, e := gateway.New(gateway.Config{Profile: gateway.Tiller, BaseURL: target.URL, APIKey: key}); e == nil {
			t.Fatal("unsafe credential")
		}
	}
}

func TestStreamFixturesAndFailure(t *testing.T) {
	good := fixture(t, "chat-stream.sse")
	bad := fixture(t, "stream-error.sse")
	cases := []struct {
		name string
		body []byte
		edit func(*gateway.Config)
		fail bool
	}{
		{"good", good, nil, false},
		{"error", bad, nil, true},
		{"eof", []byte("data: {\"choices\":[]}\n\n"), nil, true},
		{"malformed", []byte("data: nope\n\n"), nil, true},
		{"line-limit", good, func(c *gateway.Config) { c.MaxLineBytes = 20 }, true},
		{"event-limit", good, func(c *gateway.Config) { c.MaxEventBytes = 30 }, true},
		{"argument-limit", good, func(c *gateway.Config) { c.MaxArgumentBytes = 3 }, true},
		{"multiline-crlf", []byte(": comment\r\ndata: {\"choices\": [\r\ndata: {\"index\":0,\"delta\":{\"content\":\"🌍\"}}]}\r\n\r\ndata: [DONE]\r\n\r\n"), nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				for _, b := range tc.body {
					w.Write([]byte{b})
					w.(http.Flusher).Flush()
				}
			}))
			defer s.Close()
			c := client(t, s, tc.edit)
			stream, e := c.Stream(context.Background(), request())
			if e != nil {
				t.Fatal(e)
			}
			defer stream.Close()
			var text, args string
			var done, usage, finish int
			var failed bool
			for {
				ev, e := stream.Next()
				if e == io.EOF {
					break
				}
				if e != nil {
					failed = true
					break
				}
				if len(ev.Raw) == 0 {
					t.Fatal("native frame missing")
				}
				switch ev.Kind {
				case gateway.TextEvent:
					text += ev.Text
				case gateway.ToolEvent:
					args += ev.Tool.Function.Arguments
				case gateway.FinishEvent:
					finish++
				case gateway.UsageEvent:
					usage++
				case gateway.CompletionEvent:
					done++
				}
			}
			if failed != tc.fail {
				t.Fatalf("failure=%v expected=%v", failed, tc.fail)
			}
			if !failed && done != 1 {
				t.Fatal("completion semantics")
			}
			if tc.name == "good" && (text != "Hello 🌍" || args != `{"value":1}` || finish != 2 || usage != 1) {
				t.Fatal("event ordering/data")
			}
		})
	}
}

func TestReviewErrorMetadataAndMappings(t *testing.T) {
	for _, tc := range []struct {
		status         int
		code, category string
	}{
		{400, "context_limit_exceeded", "context_limit"}, {400, "unsupported_parameter", "unsupported_feature"},
		{400, "invalid_api_key", "authentication"}, {408, "", "timeout"}, {422, "", "invalid_request"}, {504, "", "timeout"},
		{401, "unsupported_parameter", "authentication"},
	} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Request-ID", "prefix-test-secret-suffix")
			w.Header().Set("Request-Id", "safe-fallback")
			w.Header().Set("Retry-After", "test-secret")
			w.WriteHeader(tc.status)
			fmt.Fprintf(w, `{"error":{"code":%q,"type":%q,"message":"private-prompt test-secret"}}`, tc.code, tc.code)
		}))
		c := client(t, s, nil)
		_, err := c.Chat(context.Background(), request())
		e, ok := err.(*gateway.Error)
		if !ok || e.Category != tc.category || e.RequestID != "safe-fallback" || e.RetryAfter != "" {
			t.Fatalf("mapping/metadata status=%d: %v", tc.status, err)
		}
		s.Close()
	}
	for _, key := range []string{"has space", "nonasciié"} {
		if _, err := gateway.New(gateway.Config{Profile: gateway.Tiller, BaseURL: "http://localhost/v1", APIKey: key}); err == nil {
			t.Fatal("unsafe key accepted")
		}
	}
}

func TestReviewNormalizationAndBudgets(t *testing.T) {
	body := `{"data":[{"id":"explicit","supports_tools":null,"supports_vision":"bad","supports_structured_output":null,"architecture":{"input_modalities":["image"]},"supported_parameters":["tools","structured_outputs","response_format"]},{"id":"derived","max_output_tokens":200,"supported_parameters":["response_format"],"reasoning":{"budget_min":10,"budget_max":20,"supports_max_tokens":true,"supports_toggle":false}},{"id":"budget","reasoning_options":[{"type":"budget_tokens","min":1,"max":3},{"type":"toggle"}]}]}`
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
	defer s.Close()
	c := client(t, s, func(cfg *gateway.Config) { cfg.Profile = gateway.OpenRouter })
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if models[0].Tools != gateway.Unknown || models[0].Vision != gateway.Unknown || models[0].StructuredOutput != gateway.Unknown {
		t.Fatal("explicit unknown derived")
	}
	if models[1].StructuredOutput != gateway.Unsupported || models[1].JSONObject != gateway.Supported || models[1].ReasoningOptions.SupportsMaxTokens != gateway.Supported || models[1].ReasoningOptions.Toggle != gateway.Unsupported || *models[1].ReasoningOptions.BudgetMin != 10 {
		t.Fatal("capability distinction")
	}
	if models[2].ReasoningOptions.SupportsMaxTokens != gateway.Supported || models[2].ReasoningOptions.Toggle != gateway.Supported {
		t.Fatal("option metadata")
	}
	if len(gateway.FilterModels(models, gateway.Requirements{MinOutput: 100})) != 1 {
		t.Fatal("output filter")
	}
}

func TestReviewRequestCatalogAndArgumentBounds(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.RawQuery == "" {
			io.WriteString(w, `{"data":[],"links":{"next":"?page=2"}}`)
			return
		}
		io.WriteString(w, `{"data":[],"padding":"abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz"}`)
	}))
	defer s.Close()
	c := client(t, s, func(cfg *gateway.Config) { cfg.MaxBodyBytes = 100 })
	if _, err := c.RefreshModels(context.Background()); err == nil {
		t.Fatal("aggregate catalog limit")
	}
	if calls.Load() != 2 {
		t.Fatal("aggregate test must fetch the second page")
	}
	before := calls.Load()
	r := request()
	r.Messages[0]["content"] = strings.Repeat("x", 200)
	if _, err := c.Chat(context.Background(), r); err == nil || calls.Load() != before {
		t.Fatal("request limit")
	}
	streamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"123\"}},{\"index\":1,\"function\":{\"arguments\":\"456\"}}]}}]}\n\n")
	}))
	defer streamServer.Close()
	streamClient := client(t, streamServer, func(cfg *gateway.Config) { cfg.MaxArgumentBytes = 5 })
	stream, err := streamClient.Stream(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := stream.Next(); err == nil {
		t.Fatal("aggregate argument limit")
	}
}

func TestReviewAssistantValidation(t *testing.T) {
	for _, tc := range []struct{ message, finish, category string }{
		{`{"role":"user","content":"bad"}`, "stop", "malformed_response"},
		{`{"role":"assistant","content":42}`, "stop", "malformed_response"},
		{`{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":"not-json"}}]}`, "tool_calls", "malformed_response"},
		{`{"role":"assistant","content":"failed"}`, "error", "http"},
	} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"choices":[{"message":%s,"finish_reason":%q}]}`, tc.message, tc.finish)
		}))
		c := client(t, s, nil)
		_, err := c.Chat(context.Background(), request())
		e, ok := err.(*gateway.Error)
		if !ok || e.Category != tc.category {
			t.Fatal("assistant validation", err)
		}
		s.Close()
	}
}

func TestFinalCommonNormalizationTable(t *testing.T) {
	for _, tc := range []struct {
		fields                string
		tools, schema, object gateway.Support
	}{
		{`"supported_parameters":["tools","structured_outputs","response_format"]`, gateway.Supported, gateway.Supported, gateway.Supported},
		{`"supported_parameters":["response_format"]`, gateway.Unsupported, gateway.Unsupported, gateway.Supported},
		{`"supported_parameters":null`, gateway.Unknown, gateway.Unknown, gateway.Unknown},
		{`"supports_tools":null,"supports_structured_output":"invalid","supports_json_object":null,"supported_parameters":["tools","structured_outputs","response_format"]`, gateway.Unknown, gateway.Unknown, gateway.Unknown},
		{`"supports_tools":0,"supports_structured_output":false,"supports_json_object":true`, gateway.Unsupported, gateway.Unsupported, gateway.Supported},
	} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"data":[{"id":"common",%s}]}`, tc.fields)
		}))
		c := client(t, s, func(cfg *gateway.Config) { cfg.Profile = gateway.OpenRouter })
		models, err := c.ListModels(context.Background())
		if err != nil || models[0].Tools != tc.tools || models[0].StructuredOutput != tc.schema || models[0].JSONObject != tc.object {
			t.Fatal("normalization table", err)
		}
		s.Close()
	}
}

func TestFinalUsageNormalization(t *testing.T) {
	for _, tc := range []struct {
		payload     string
		read, write *int
	}{
		{`{}`, nil, nil},
		{`{"prompt_tokens_details":{"cached_tokens":3},"input_tokens_details":{"cached_tokens":7},"cache_read_input_tokens":9,"cache_creation_input_tokens":2,"native":{"value":true}}`, intPointer(3), intPointer(2)},
		{`{"input_tokens_details":{"cached_tokens":0}}`, intPointer(0), nil},
		{`{"cache_read_input_tokens":4,"cache_creation_input_tokens":0}`, intPointer(4), intPointer(0)},
	} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":%s}`, tc.payload)
		}))
		c := client(t, s, nil)
		result, err := c.Chat(context.Background(), request())
		if err != nil {
			t.Fatal(err)
		}
		u := result.Usage
		if u == nil || !sameCount(u.CacheReadTokens, tc.read) || !sameCount(u.CacheWriteTokens, tc.write) || u.TotalTokens != nil || string(u.Raw) != tc.payload {
			t.Fatal("usage normalization")
		}
		s.Close()
	}
}

func intPointer(n int) *int    { return &n }
func sameCount(a, b *int) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

func TestFinalConfigAndWirePreservation(t *testing.T) {
	cfg := gateway.Config{Profile: gateway.Tiller, BaseURL: "http://localhost/v1", APIKey: "private-secret"}
	serialized, err := json.Marshal(cfg)
	if err != nil || strings.Contains(string(serialized), cfg.APIKey) {
		t.Fatal("config JSON leak")
	}
	for _, path := range []string{"models\\user", "models/%2e%2e/user", "models/%252e%252e/user", "models/%5cuser", "models/%0auser", "models/../user", "models\tuser"} {
		cfg.CatalogPath = path
		if _, err := gateway.New(cfg); err == nil {
			t.Fatal("unsafe path", path)
		}
	}
	image := map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,example", "detail": "low"}, "native_image": true}
	schema := map[string]any{"type": "json_schema", "json_schema": map[string]any{"strict": true, "schema": map[string]any{"type": "object", "additionalProperties": false, "native_schema": []any{"kept"}}}}
	r := request()
	r.Messages = []gateway.Message{{"role": "user", "content": []any{image}}, {"role": "tool", "tool_call_id": "call-1", "content": "tool-result", "native_result": true}}
	r.ResponseFormat = schema
	r.Extra = map[string]any{"native_request": map[string]any{"value": true}}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var wire map[string]json.RawMessage
		if json.NewDecoder(req.Body).Decode(&wire) != nil {
			t.Error("wire decode")
		}
		wantMessages, _ := json.Marshal(r.Messages)
		wantSchema, _ := json.Marshal(schema)
		if string(wire["messages"]) != string(wantMessages) || string(wire["response_format"]) != string(wantSchema) || len(wire["native_request"]) == 0 {
			t.Error("native fields changed")
		}
		io.WriteString(w, `{"native_response":true,"choices":[{"message":{"role":"assistant","content":"OK","opaque":{"integer":9007199254740993}},"finish_reason":"stop"}]}`)
	}))
	defer s.Close()
	c := client(t, s, nil)
	result, err := c.Chat(context.Background(), r)
	if err != nil || !strings.Contains(string(result.Raw), "native_response") || !strings.Contains(string(result.Choices[0].Native), "9007199254740993") {
		t.Fatal("native response lost", err)
	}
	replayed, _ := json.Marshal(result.Choices[0].Message)
	if !strings.Contains(string(replayed), "9007199254740993") {
		t.Fatal("replay numeric precision")
	}
	r.Extra = map[string]any{"base_url": "bad"}
	if _, err := c.Chat(context.Background(), r); err == nil {
		t.Fatal("base_url extension accepted")
	}
	r.Extra = nil
	r.Model = " \t\n"
	if _, err := c.Chat(context.Background(), r); err == nil {
		t.Fatal("blank model accepted")
	}
}

func TestFinalCloseAndDoneDoesNotMaskFailure(t *testing.T) {
	for _, frame := range []string{`not-json`, `{"choices":[{"delta":{},"finish_reason":"error"}]}`} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Accept") != "text/event-stream" {
				t.Error("stream Accept")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", frame)
		}))
		c := client(t, s, nil)
		stream, err := c.Stream(context.Background(), request())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Next(); err == nil {
			t.Fatal("DONE masked failure")
		}
		stream.Close()
		c.Close()
		if _, err := c.ListModels(context.Background()); err == nil {
			t.Fatal("closed catalog accepted")
		}
		if _, err := c.Chat(context.Background(), request()); err == nil {
			t.Fatal("closed chat accepted")
		}
		if _, err := c.Stream(context.Background(), request()); err == nil {
			t.Fatal("closed stream accepted")
		}
		s.Close()
	}
	closed := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(closed)
	}))
	defer s.Close()
	c := client(t, s, nil)
	stream, err := c.Stream(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	c.Close()
	if _, err := stream.Next(); err == nil {
		t.Fatal("client Close did not cancel stream")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close left upstream active")
	}
}

func TestStreamCancellationClosesUpstream(t *testing.T) {
	closed := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(closed)
	}))
	defer s.Close()
	c := client(t, s, nil)
	ctx, cancel := context.WithCancel(context.Background())
	stream, e := c.Stream(ctx, request())
	if e != nil {
		t.Fatal(e)
	}
	result := make(chan error, 1)
	go func() { _, e := stream.Next(); result <- e }()
	cancel()
	select {
	case e := <-result:
		ge, ok := e.(*gateway.Error)
		if !ok || ge.Category != "cancelled" {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel hung")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("upstream not closed")
	}
}
