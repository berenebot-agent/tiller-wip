package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRegistryIncludesApprovedProviders(t *testing.T) {
	for _, providerType := range []string{"openai", "anthropic", "openrouter", "ollama-local", "ollama-cloud", "deepseek", "zai", "gemini", "azure-openai", "bedrock-api-key", "groq", "mistral", "xai", "together", "fireworks", "cerebras", "perplexity", "nvidia-nim", "huggingface", "cloudflare-ai", "alibaba-qwen", "minimax", "opencode-zen", "opencode-go", "opencode-free", "generic-openai", "vllm", "lm-studio", "llama-cpp"} {
		if _, ok := Lookup(providerType); !ok {
			t.Errorf("missing provider type %s", providerType)
		}
	}
}

func TestSetResponseHeaderTimeout(t *testing.T) {
	r := NewRegistry()
	transport, ok := r.HTTPClient().Transport.(*http.Transport)
	if !ok {
		t.Fatal("registry transport is not *http.Transport")
	}
	if transport.ResponseHeaderTimeout != 60*time.Second {
		t.Fatalf("default ResponseHeaderTimeout = %v, want 60s", transport.ResponseHeaderTimeout)
	}
	r.SetResponseHeaderTimeout(120 * time.Second)
	if transport.ResponseHeaderTimeout != 120*time.Second {
		t.Fatalf("ResponseHeaderTimeout after set = %v, want 120s", transport.ResponseHeaderTimeout)
	}
}

func TestOpenCodeNativeProtocols(t *testing.T) {
	zen := map[string]Protocol{
		"gpt-5.5":           ProtocolResponses,
		"claude-opus-4.6":   ProtocolMessages,
		"deepseek-v4-flash": ProtocolChat,
		"unknown-model":     ProtocolChat,
	}
	for modelID, want := range zen {
		if got := nativeProtocol("opencode-zen", modelID); got != want {
			t.Errorf("Zen model %q protocol = %q, want %q", modelID, got, want)
		}
	}
	if got := nativeProtocol("opencode-go", "any-model"); got != ProtocolChat {
		t.Fatalf("Go model protocol = %q, want %q", got, ProtocolChat)
	}
	if got := nativeProtocol("opencode-zen", "unknown-model"); got != ProtocolChat {
		t.Fatalf("unknown Zen model protocol = %q, want %q", got, ProtocolChat)
	}
	freeModels := map[string]Protocol{
		"muse-spark-1.2-contributor-free": ProtocolResponses,
		"muse-spark-1.3-contributor-free": ProtocolResponses,
		"nemotron-3-ultra-free":           ProtocolChat,
		"deepseek-v4-flash-free":          ProtocolChat,
		"mimo-v2.5-free":                  ProtocolChat,
		"unlisted-model-free":             ProtocolChat,
	}
	for modelID, want := range freeModels {
		if got := nativeProtocol("opencode-free", modelID); got != want {
			t.Errorf("opencode-free model %q protocol = %q, want %q", modelID, got, want)
		}
	}
	for modelID, want := range map[string]Protocol{
		"muse-spark-1.2-contributor-free": ProtocolResponses,
		"muse-spark-1.3-contributor-free": ProtocolResponses,
		"nemotron-3-ultra-free":           ProtocolChat,
		"deepseek-v4-flash-free":          ProtocolChat,
		"mimo-v2.5-free":                  ProtocolChat,
	} {
		if got := nativeProtocol("opencode-zen", modelID); got != want {
			t.Errorf("opencode-zen model %q protocol = %q, want %q", modelID, got, want)
		}
	}
}

func TestOpenCodeDescriptors(t *testing.T) {
	for _, test := range []struct {
		providerType string
		url          string
		credential   bool
		protocols    int
	}{
		{"opencode-zen", "https://opencode.ai/zen/v1", true, 3},
		{"opencode-go", "https://opencode.ai/zen/go/v1", true, 3},
		{"opencode-free", "https://opencode.ai/zen/v1", false, 2},
	} {
		descriptor, ok := Lookup(test.providerType)
		if !ok {
			t.Fatalf("missing descriptor %q", test.providerType)
		}
		if descriptor.DefaultBaseURL != test.url || descriptor.CredentialNeeded != test.credential || len(descriptor.Protocols) != test.protocols {
			t.Errorf("unexpected %q descriptor: %+v", test.providerType, descriptor)
		}
	}
}

func TestOpenCodeDiscoveryAssignsNativeProtocols(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("discovery path = %q, want /v1/models", r.URL.Path)
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("discovery credential missing")
			http.Error(w, "missing credential", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{"id": "gpt-5.5"},
			map[string]any{"id": "claude-opus-4.6"},
			map[string]any{"id": "deepseek-v4-flash"},
			map[string]any{"id": "unlisted-model"},
		}})
	}))
	defer upstream.Close()

	models, err := NewRegistry().Discover(context.Background(), Instance{Type: "opencode-zen", BaseURL: upstream.URL + "/v1", Credential: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]Protocol, len(models))
	for _, model := range models {
		got[model.ID] = model.NativeProtocol
	}
	want := map[string]Protocol{"gpt-5.5": ProtocolResponses, "claude-opus-4.6": ProtocolMessages, "deepseek-v4-flash": ProtocolChat, "unlisted-model": ProtocolChat}
	for modelID, protocol := range want {
		if got[modelID] != protocol {
			t.Errorf("model %q protocol = %q, want %q", modelID, got[modelID], protocol)
		}
	}
}

func TestOpenCodeFreeDiscoveryFiltersToFreeModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("discovery path = %q, want /v1/models", r.URL.Path)
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "" {
			t.Errorf("opencode-free should not send Authorization, got %q", r.Header.Get("Authorization"))
			http.Error(w, "unexpected credential", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{"id": "gpt-5.5"},
			map[string]any{"id": "deepseek-v4-flash-free"},
			map[string]any{"id": "muse-spark-1.2-contributor-free"},
			map[string]any{"id": "claude-opus-4.6"},
			map[string]any{"id": "mimo-v2.5-free"},
			map[string]any{"id": "plain-model"},
		}})
	}))
	defer upstream.Close()

	models, err := NewRegistry().Discover(context.Background(), Instance{Type: "opencode-free", BaseURL: upstream.URL + "/v1", Credential: ""})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 {
		t.Fatalf("expected 3 free models, got %d: %v", len(models), models)
	}
	ids := make(map[string]bool)
	for _, m := range models {
		ids[m.ID] = true
	}
	for _, want := range []string{"deepseek-v4-flash-free", "muse-spark-1.2-contributor-free", "mimo-v2.5-free"} {
		if !ids[want] {
			t.Errorf("missing free model %q in %v", want, ids)
		}
	}
	for _, notWant := range []string{"gpt-5.5", "claude-opus-4.6", "plain-model"} {
		if ids[notWant] {
			t.Errorf("non-free model %q should have been filtered", notWant)
		}
	}
}

func TestProviderProtocolEndpointsAndAuthentication(t *testing.T) {
	for _, descriptor := range Descriptors() {
		baseURL := descriptor.DefaultBaseURL
		if baseURL == "" {
			baseURL = "https://provider.example/v1"
		}
		for _, protocol := range descriptor.Protocols {
			endpoint, err := Endpoint(Instance{Type: descriptor.Type, BaseURL: baseURL}, protocol)
			if err != nil {
				t.Fatalf("%s %s endpoint: %v", descriptor.Type, protocol, err)
			}
			expectedSuffix := map[Protocol]string{ProtocolChat: "/chat/completions", ProtocolResponses: "/responses", ProtocolMessages: "/v1/messages"}[protocol]
			if strings.HasPrefix(descriptor.Type, "ollama-") && protocol == ProtocolChat {
				expectedSuffix = "/v1/chat/completions"
			}
			if !strings.HasSuffix(endpoint, expectedSuffix) {
				t.Errorf("%s %s endpoint %q does not end in %q", descriptor.Type, protocol, endpoint, expectedSuffix)
			}
		}
		req, _ := http.NewRequest(http.MethodGet, "https://provider.example/models", nil)
		ApplyRequestAuth(req, Instance{Type: descriptor.Type, Credential: "test-secret"})
		switch descriptor.Type {
		case "anthropic":
			if req.Header.Get("x-api-key") != "test-secret" || req.Header.Get("anthropic-version") == "" || req.Header.Get("Authorization") != "" {
				t.Errorf("unexpected Anthropic authentication headers: %v", req.Header)
			}
		case "azure-openai":
			if req.Header.Get("api-key") != "test-secret" || req.Header.Get("Authorization") != "" {
				t.Errorf("unexpected Azure authentication headers: %v", req.Header)
			}
		default:
			if req.Header.Get("Authorization") != "Bearer test-secret" {
				t.Errorf("%s missing bearer authentication", descriptor.Type)
			}
		}
	}
}

func TestAppendEndpointMergesQueries(t *testing.T) {
	tests := []struct {
		name, base, endpoint string
		want                 url.Values
	}{
		{name: "no queries", base: "https://example.com/v1", endpoint: "responses", want: url.Values{}},
		{name: "base query", base: "https://example.com/v1?api-version=2026-01-01", endpoint: "responses", want: url.Values{"api-version": {"2026-01-01"}}},
		{name: "multiple base values", base: "https://example.com/v1?a=1&a=2&b=hello+world", endpoint: "responses", want: url.Values{"a": {"1", "2"}, "b": {"hello world"}}},
		{name: "endpoint addition", base: "https://example.com/v1?api-version=2026-01-01", endpoint: "responses?after=cursor", want: url.Values{"api-version": {"2026-01-01"}, "after": {"cursor"}}},
		{name: "endpoint duplicate wins", base: "https://example.com/v1?mode=base&keep=yes", endpoint: "responses?mode=endpoint&mode=second", want: url.Values{"mode": {"endpoint", "second"}, "keep": {"yes"}}},
		{name: "encoded values", base: "https://example.com/v1?filter=a%2Fb%3Fc&space=hello%20world", endpoint: "responses?encoded=%5Bone%5D%26%5Btwo%5D", want: url.Values{"filter": {"a/b?c"}, "space": {"hello world"}, "encoded": {"[one]&[two]"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotURL, err := appendEndpoint(test.base, test.endpoint)
			if err != nil {
				t.Fatal(err)
			}
			got, err := url.Parse(gotURL)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Query(), test.want) {
				t.Fatalf("query = %#v, want %#v (url %q)", got.Query(), test.want, gotURL)
			}
		})
	}
}

func TestEndpointPreservesBaseQuery(t *testing.T) {
	endpoint, err := Endpoint(Instance{Type: "generic-openai", BaseURL: "https://example.com/v1?api-version=2026-01-01&tenant=one"}, ProtocolChat)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	want := url.Values{"api-version": {"2026-01-01"}, "tenant": {"one"}}
	if !reflect.DeepEqual(u.Query(), want) {
		t.Fatalf("endpoint query = %#v, want %#v", u.Query(), want)
	}
}

func TestPagedDiscovery(t *testing.T) {
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.URL.Query().Get("api-version"); got != "2026-01-01" {
			t.Errorf("base query api-version = %q, want 2026-01-01", got)
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("credential header missing")
		}
		if r.URL.Query().Get("after") == "first" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "model-b", "max_output_tokens": 4096}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "model-a"}}, "has_more": true, "last_id": "first"})
	}))
	defer upstream.Close()
	models, err := NewRegistry().Discover(context.Background(), Instance{Type: "generic-openai", BaseURL: upstream.URL + "/v1?api-version=2026-01-01", Credential: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(models) != 2 || models[0].ID != "model-a" || models[1].ID != "model-b" {
		t.Fatalf("unexpected discovery: requests=%d models=%v", requests, models)
	}
	if models[0].MaxOutputTokens != 0 || models[1].MaxOutputTokens != 4096 {
		t.Fatalf("unexpected output-token metadata: models=%v", models)
	}
}

func TestPagedDiscoveryCapturesCapabilities(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{
				"id":                   "model-a",
				"supported_parameters": []string{"tools", "reasoning", "structured_outputs"},
				"architecture":         map[string]any{"input_modalities": []string{"text", "image"}, "output_modalities": []string{"text"}},
			},
			map[string]any{"id": "model-b"},
		}})
	}))
	defer upstream.Close()
	models, err := NewRegistry().Discover(context.Background(), Instance{Type: "openrouter", BaseURL: upstream.URL + "/v1", Credential: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}
	a := models[0]
	if a.SupportsTools == nil || !*a.SupportsTools {
		t.Errorf("model-a supports_tools: %v", a.SupportsTools)
	}
	if a.SupportsVision == nil || !*a.SupportsVision {
		t.Errorf("model-a supports_vision: %v", a.SupportsVision)
	}
	if a.SupportsReasoning == nil || !*a.SupportsReasoning {
		t.Errorf("model-a supports_reasoning: %v", a.SupportsReasoning)
	}
	if a.SupportsStructuredOutput == nil || !*a.SupportsStructuredOutput {
		t.Errorf("model-a supports_structured_output: %v", a.SupportsStructuredOutput)
	}
	if len(a.InputModalities) != 2 || a.InputModalities[0] != "text" || a.InputModalities[1] != "image" {
		t.Errorf("model-a input_modalities: %v", a.InputModalities)
	}
	// model-b reports nothing -> all flags stay unknown (nil).
	b := models[1]
	if b.SupportsTools != nil || b.SupportsVision != nil || b.SupportsReasoning != nil || b.SupportsStructuredOutput != nil {
		t.Errorf("model-b flags should be unknown, got tools=%v vision=%v reasoning=%v structured=%v", b.SupportsTools, b.SupportsVision, b.SupportsReasoning, b.SupportsStructuredOutput)
	}
}

func TestOpenRouterDiscoveryCapturesTopProviderOutputLimit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{"id": "model-a", "top_provider": map[string]any{"max_completion_tokens": 8192}},
			map[string]any{"id": "model-b", "top_provider": map[string]any{}},
		}})
	}))
	defer upstream.Close()
	models, err := NewRegistry().Discover(context.Background(), Instance{Type: "openrouter", BaseURL: upstream.URL + "/api/v1", Credential: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].MaxOutputTokens != 8192 || models[1].MaxOutputTokens != 0 {
		t.Fatalf("unexpected OpenRouter output metadata: %+v", models)
	}
}

func TestOllamaDiscoveryCapturesContextLength(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{map[string]any{"name": "qwen3.5:397b"}, map[string]any{"name": "llama3:8b"}, map[string]any{"name": "deepseek-v4-flash:0731"}}})
		case "/api/show":
			var input map[string]string
			_ = json.NewDecoder(r.Body).Decode(&input)
			switch input["model"] {
			case "qwen3.5:397b":
				_ = json.NewEncoder(w).Encode(map[string]any{"model_info": map[string]any{"llama.context_length": 262144}, "parameters": map[string]any{"num_ctx": 4096}})
			case "llama3:8b":
				// No trained context reported; fall back to runtime num_ctx.
				_ = json.NewEncoder(w).Encode(map[string]any{"model_info": map[string]any{}, "parameters": map[string]any{"num_ctx": 8192}})
			case "deepseek-v4-flash:0731":
				_ = json.NewEncoder(w).Encode(map[string]any{"model_info": map[string]any{"deepseek.context_length": 1048576}, "parameters": map[string]any{}})
			default:
				http.Error(w, "unknown model", 404)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	models, err := NewRegistry().Discover(context.Background(), Instance{Type: "ollama-local", BaseURL: upstream.URL, Credential: ""})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 {
		t.Fatalf("unexpected model count: %v", models)
	}
	if models[0].ID != "qwen3.5:397b" || models[0].ContextLength != 262144 {
		t.Fatalf("qwen3.5:397b context not captured: %+v", models[0])
	}
	if models[1].ID != "llama3:8b" || models[1].ContextLength != 8192 {
		t.Fatalf("llama3:8b num_ctx fallback not captured: %+v", models[1])
	}
	if models[2].ID != "deepseek-v4-flash:0731" || models[2].ContextLength != 1048576 {
		t.Fatalf("deepseek-v4-flash:0731 architecture context not captured: %+v", models[2])
	}
}

func TestValidateBaseURL(t *testing.T) {
	for _, invalid := range []string{"file:///etc/passwd", "https://user:secret@example.com", "javascript:alert(1)", "https:///missing"} {
		if ValidateBaseURL(invalid) == nil {
			t.Errorf("accepted %q", invalid)
		}
	}
	if err := ValidateBaseURL("http://host.docker.internal:11434"); err != nil {
		t.Fatal(err)
	}
}
