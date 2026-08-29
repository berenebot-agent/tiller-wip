package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegistryIncludesApprovedProviders(t *testing.T) {
	for _, providerType := range []string{"openai", "anthropic", "openrouter", "ollama-local", "ollama-cloud", "deepseek", "zai", "gemini", "azure-openai", "bedrock-api-key", "groq", "mistral", "xai", "together", "fireworks", "cerebras", "perplexity", "nvidia-nim", "huggingface", "cloudflare-ai", "alibaba-qwen", "minimax", "generic-openai", "vllm", "lm-studio", "llama-cpp"} {
		if _, ok := Lookup(providerType); !ok {
			t.Errorf("missing provider type %s", providerType)
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

func TestPagedDiscovery(t *testing.T) {
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("credential header missing")
		}
		if r.URL.Query().Get("after") == "first" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "model-b"}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "model-a"}}, "has_more": true, "last_id": "first"})
	}))
	defer upstream.Close()
	models, err := NewRegistry().Discover(context.Background(), Instance{Type: "generic-openai", BaseURL: upstream.URL + "/v1", Credential: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(models) != 2 || models[0].ID != "model-a" || models[1].ID != "model-b" {
		t.Fatalf("unexpected discovery: requests=%d models=%v", requests, models)
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
