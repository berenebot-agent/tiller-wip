package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func boolPtr(v bool) *bool { return &v }

func TestEnrichFillsUnknownFields(t *testing.T) {
	r := NewRegistry()
	r.modelsDev = modelsDevDataset{
		"deepseek": {Models: map[string]modelsDevModel{
			"deepseek-v4-flash": {
				Reasoning:        boolPtr(true),
				ToolCall:         boolPtr(true),
				StructuredOutput: boolPtr(true),
				Modalities:       modelsDevModalities{Input: []string{"text", "image"}, Output: []string{"text"}},
				Limit:            modelsDevLimit{Context: 1000000, Output: 384000},
			},
		}},
	}
	got := r.enrich([]Model{{ID: "deepseek-v4-flash"}}, "deepseek")
	if len(got) != 1 {
		t.Fatalf("expected 1 model, got %d", len(got))
	}
	m := got[0]
	if m.ContextLength != 1000000 {
		t.Errorf("context = %d, want 1000000", m.ContextLength)
	}
	if m.MaxOutputTokens != 384000 {
		t.Errorf("max output = %d, want 384000", m.MaxOutputTokens)
	}
	if m.SupportsTools == nil || !*m.SupportsTools {
		t.Errorf("tools = %v, want true", m.SupportsTools)
	}
	if m.SupportsVision == nil || !*m.SupportsVision {
		t.Errorf("vision = %v, want true", m.SupportsVision)
	}
	if m.SupportsReasoning == nil || !*m.SupportsReasoning {
		t.Errorf("reasoning = %v, want true", m.SupportsReasoning)
	}
	if m.SupportsStructuredOutput == nil || !*m.SupportsStructuredOutput {
		t.Errorf("structured = %v, want true", m.SupportsStructuredOutput)
	}
	if len(m.InputModalities) != 2 || m.InputModalities[0] != "text" || m.InputModalities[1] != "image" {
		t.Errorf("input modalities = %v", m.InputModalities)
	}
	if len(m.OutputModalities) != 1 || m.OutputModalities[0] != "text" {
		t.Errorf("output modalities = %v", m.OutputModalities)
	}
}

func TestEnrichDoesNotOverrideProviderReported(t *testing.T) {
	r := NewRegistry()
	r.modelsDev = modelsDevDataset{
		"deepseek": {Models: map[string]modelsDevModel{
			"deepseek-v4-flash": {
				Reasoning:        boolPtr(false),
				ToolCall:         boolPtr(false),
				StructuredOutput: boolPtr(false),
				Modalities:       modelsDevModalities{Input: []string{"text"}, Output: []string{"text"}},
				Limit:            modelsDevLimit{Context: 2000000, Output: 100000},
			},
		}},
	}
	models := []Model{{
		ID:                       "deepseek-v4-flash",
		ContextLength:            1000000,
		MaxOutputTokens:          50000,
		SupportsTools:            boolPtr(true),
		SupportsVision:           boolPtr(false),
		SupportsReasoning:        boolPtr(true),
		SupportsStructuredOutput: boolPtr(true),
		InputModalities:          []string{"text", "image"},
		OutputModalities:         []string{"text"},
	}}
	got := r.enrich(models, "deepseek")[0]
	if got.ContextLength != 1000000 {
		t.Errorf("context overridden: %d", got.ContextLength)
	}
	if got.MaxOutputTokens != 50000 {
		t.Errorf("max output overridden: %d", got.MaxOutputTokens)
	}
	if got.SupportsTools == nil || !*got.SupportsTools {
		t.Errorf("tools overridden: %v", got.SupportsTools)
	}
	if got.SupportsVision == nil || *got.SupportsVision {
		t.Errorf("vision overridden: %v", got.SupportsVision)
	}
	if got.SupportsReasoning == nil || !*got.SupportsReasoning {
		t.Errorf("reasoning overridden: %v", got.SupportsReasoning)
	}
	if got.SupportsStructuredOutput == nil || !*got.SupportsStructuredOutput {
		t.Errorf("structured overridden: %v", got.SupportsStructuredOutput)
	}
	if len(got.InputModalities) != 2 {
		t.Errorf("input modalities overridden: %v", got.InputModalities)
	}
	if len(got.OutputModalities) != 1 {
		t.Errorf("output modalities overridden: %v", got.OutputModalities)
	}
}

func TestEnrichPreservesTriState(t *testing.T) {
	r := NewRegistry()
	r.modelsDev = modelsDevDataset{
		"deepseek": {Models: map[string]modelsDevModel{
			// Only tool_call reported; everything else stays unknown.
			"deepseek-v4-flash": {ToolCall: boolPtr(true)},
		}},
	}
	got := r.enrich([]Model{{ID: "deepseek-v4-flash"}}, "deepseek")[0]
	if got.ContextLength != 0 {
		t.Errorf("context should stay unknown, got %d", got.ContextLength)
	}
	if got.MaxOutputTokens != 0 {
		t.Errorf("max output should stay unknown, got %d", got.MaxOutputTokens)
	}
	if got.SupportsReasoning != nil {
		t.Errorf("reasoning should stay unknown, got %v", got.SupportsReasoning)
	}
	if got.SupportsStructuredOutput != nil {
		t.Errorf("structured should stay unknown, got %v", got.SupportsStructuredOutput)
	}
	if got.SupportsVision != nil {
		t.Errorf("vision should stay unknown, got %v", got.SupportsVision)
	}
	if len(got.InputModalities) != 0 || len(got.OutputModalities) != 0 {
		t.Errorf("modalities should stay unknown, got %v/%v", got.InputModalities, got.OutputModalities)
	}
	if got.SupportsTools == nil || !*got.SupportsTools {
		t.Errorf("tools should be filled, got %v", got.SupportsTools)
	}
}

func TestEnrichDisabledIsNoOp(t *testing.T) {
	r := NewRegistry()
	r.modelsDevEnabled = false
	r.modelsDev = modelsDevDataset{
		"deepseek": {Models: map[string]modelsDevModel{
			"deepseek-v4-flash": {Limit: modelsDevLimit{Context: 1000000}},
		}},
	}
	got := r.enrich([]Model{{ID: "deepseek-v4-flash"}}, "deepseek")[0]
	if got.ContextLength != 0 {
		t.Errorf("enrich should be a no-op when disabled, got context %d", got.ContextLength)
	}
}

func TestEnrichUnknownProviderOrModel(t *testing.T) {
	r := NewRegistry()
	r.modelsDev = modelsDevDataset{
		"deepseek": {Models: map[string]modelsDevModel{
			"deepseek-v4-flash": {Limit: modelsDevLimit{Context: 1000000}},
		}},
	}
	// Unknown provider type -> unchanged.
	if got := r.enrich([]Model{{ID: "deepseek-v4-flash"}}, "unknown-type"); got[0].ContextLength != 0 {
		t.Errorf("unknown provider should not enrich, got context %d", got[0].ContextLength)
	}
	// Known provider but unknown model -> unchanged.
	if got := r.enrich([]Model{{ID: "not-in-modelsdev"}}, "deepseek"); got[0].ContextLength != 0 {
		t.Errorf("unknown model should not enrich, got context %d", got[0].ContextLength)
	}
}

func TestModelsDevProviderKeyMapping(t *testing.T) {
	for providerType, want := range map[string]string{
		"openrouter": "openrouter", "deepseek": "deepseek", "nvidia-nim": "nvidia",
		"zai": "zhipuai", "gemini": "google", "alibaba-qwen": "alibaba",
		"fireworks": "fireworks-ai", "azure-openai": "azure", "opencode-zen": "opencode",
		"opencode-go": "opencode", "opencode-free": "opencode", "openai": "openai", "anthropic": "anthropic",
		"groq": "groq", "mistral": "mistral", "xai": "xai", "cerebras": "cerebras",
		"perplexity": "perplexity", "minimax": "minimax", "huggingface": "huggingface",
	} {
		if got := modelsDevProviderKey[providerType]; got != want {
			t.Errorf("provider %q maps to %q, want %q", providerType, got, want)
		}
	}
}

func TestRefreshModelsDevGracefulDegradation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models-dev.json")
	oldURL := modelsDevURL
	defer func() { modelsDevURL = oldURL }()

	// 1. Failing fetch with no cache -> error, no in-memory data, no cache file.
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer down.Close()
	modelsDevURL = down.URL
	r := NewRegistry()
	if err := r.RefreshModelsDev(context.Background(), path); err == nil {
		t.Fatal("expected error from failing fetch")
	}
	r.mu.Lock()
	if r.modelsDev != nil {
		t.Fatal("in-memory data should be nil after failed fetch")
	}
	r.mu.Unlock()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("cache file should not exist after failed fetch")
	}

	// 2. Successful fetch -> in-memory data set and cache written.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"deepseek": map[string]any{
				"models": map[string]any{
					"deepseek-v4-flash": map[string]any{
						"reasoning": true, "tool_call": true, "structured_output": true,
						"modalities": map[string]any{"input": []string{"text"}, "output": []string{"text"}},
						"limit":      map[string]any{"context": 1000000, "output": 384000},
					},
				},
			},
		})
	}))
	defer up.Close()
	modelsDevURL = up.URL
	if err := r.RefreshModelsDev(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	data := r.modelsDev
	r.mu.Unlock()
	if data == nil {
		t.Fatal("in-memory data should be set after successful fetch")
	}
	if _, ok := data["deepseek"]; !ok {
		t.Fatal("expected deepseek provider in data")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cache file should exist: %v", err)
	}

	// 3. LoadModelsDevCache from the written file.
	r2 := NewRegistry()
	r2.LoadModelsDevCache(path)
	r2.mu.Lock()
	data2 := r2.modelsDev
	r2.mu.Unlock()
	if data2 == nil {
		t.Fatal("LoadModelsDevCache should load the cache file")
	}
	if _, ok := data2["deepseek"]; !ok {
		t.Fatal("expected deepseek provider in loaded cache")
	}
}

func TestLoadModelsDevCacheMissingIsNotError(t *testing.T) {
	r := NewRegistry()
	r.LoadModelsDevCache(filepath.Join(t.TempDir(), "does-not-exist.json"))
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.modelsDev != nil {
		t.Fatal("missing cache should leave in-memory data nil")
	}
}

// glmDataset is a minimal models.dev dataset mirroring the canonical zai entry
// for glm-5.3-flash so Ollama tests assert meaningful values.
func glmDataset() modelsDevDataset {
	return modelsDevDataset{
		"zai": {Models: map[string]modelsDevModel{
			"glm-5.3-flash": {
				Reasoning:        boolPtr(true),
				ToolCall:         boolPtr(true),
				StructuredOutput: boolPtr(true),
				Modalities:       modelsDevModalities{Input: []string{"text", "image", "video", "pdf"}, Output: []string{"text"}},
				Limit:            modelsDevLimit{Context: 1000000, Output: 131072},
			},
		}},
	}
}

func TestOllamaPlainName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"glm-5.3-flash", "glm-5.3-flash"},
		{"glm-5.3-flash:latest", "glm-5.3-flash"},
		{"zai/glm-5.3-flash", "glm-5.3-flash"},
		{"zai/glm-5.3-flash:latest", "glm-5.3-flash"},
		{"deepseek-v4-flash:8b-q4_0", "deepseek-v4-flash"},
	}
	for _, c := range cases {
		if got := ollamaPlainName(c.in); got != c.want {
			t.Errorf("ollamaPlainName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestOllamaLookupCanonicalLab(t *testing.T) {
	data := glmDataset()
	// Exact plain-name hit under the inferred canonical lab (zai for GLM).
	got := ollamaLookup(data, "glm-5.3-flash:latest")
	if got.Limit.Context != 1000000 || got.Limit.Output != 131072 {
		t.Errorf("ollama glm hit = ctx %d out %d, want 1000000/131072", got.Limit.Context, got.Limit.Output)
	}
	if got.ToolCall == nil || !*got.ToolCall || got.Reasoning == nil || !*got.Reasoning {
		t.Errorf("ollama glm should report tools+reasoning, got %+v", got)
	}
}

func TestOllamaEnrichFillsUnknownFields(t *testing.T) {
	r := NewRegistry()
	r.modelsDev = glmDataset()
	// Plain name, with a tag: enrichment must land on the canonical zai entry.
	got := r.enrich([]Model{{ID: "glm-5.3-flash:latest"}}, "ollama-local")
	if len(got) != 1 {
		t.Fatalf("expected 1 model, got %d", len(got))
	}
	m := got[0]
	if m.ContextLength != 1000000 {
		t.Errorf("context = %d, want 1000000", m.ContextLength)
	}
	if m.MaxOutputTokens != 131072 {
		t.Errorf("max output = %d, want 131072", m.MaxOutputTokens)
	}
	if m.SupportsTools == nil || !*m.SupportsTools {
		t.Errorf("tools = %v, want true", m.SupportsTools)
	}
	if m.SupportsReasoning == nil || !*m.SupportsReasoning {
		t.Errorf("reasoning = %v, want true", m.SupportsReasoning)
	}
	if m.SupportsVision == nil || !*m.SupportsVision {
		t.Errorf("vision = %v, want true", m.SupportsVision)
	}
}

func TestOllamaEnrichUnknownFamilyOrModel(t *testing.T) {
	r := NewRegistry()
	r.modelsDev = glmDataset()
	// Unknown family must not enrich.
	if got := r.enrich([]Model{{ID: "some-rando-model:latest"}}, "ollama-local"); got[0].ContextLength != 0 {
		t.Errorf("unknown family should not enrich, got context %d", got[0].ContextLength)
	}
	// Known family but unknown model name must not enrich.
	if got := r.enrich([]Model{{ID: "glm-not-a-real-model:latest"}}, "ollama-local"); got[0].ContextLength != 0 {
		t.Errorf("unknown model should not enrich, got context %d", got[0].ContextLength)
	}
}

func TestOllamaEnrichDoesNotOverrideProviderReported(t *testing.T) {
	r := NewRegistry()
	r.modelsDev = glmDataset()
	got := r.enrich([]Model{{ID: "glm-5.3-flash", ContextLength: 1000000, MaxOutputTokens: 99999}}, "ollama-local")[0]
	// Provider-reported max output (99999) must be kept, not replaced by the
	// models.dev 131072.
	if got.MaxOutputTokens != 99999 {
		t.Errorf("provider-reported max output overridden: got %d, want 99999", got.MaxOutputTokens)
	}
	if got.ContextLength != 1000000 {
		t.Errorf("context = %d, want 1000000", got.ContextLength)
	}
}

func TestOllamaEnrichLeavesOtherProvidersUntouched(t *testing.T) {
	r := NewRegistry()
	r.modelsDev = glmDataset()
	// A non-Ollama provider whose type is not in the provider map stays
	// unenriched (the prefix check must not leak into other types).
	got := r.enrich([]Model{{ID: "glm-5.3-flash"}}, "generic-openai")
	if len(got) != 1 || got[0].ContextLength != 0 {
		t.Fatalf("unmapped non-ollama provider should not enrich, got %+v", got[0])
	}
	// A type that merely *contains* "ollama" elsewhere must not trigger the
	// plain-name path.
	if got := r.enrich([]Model{{ID: "glm-5.3-flash"}}, "not-ollama-local"); got[0].ContextLength != 0 {
		t.Fatalf("non-ollama-prefixed type should not enrich, got %+v", got[0])
	}
	// The exact deepseek path still behaves via the provider map.
	r.modelsDev["deepseek"] = modelsDevProvider{Models: map[string]modelsDevModel{
		"deepseek-v4-flash": {Limit: modelsDevLimit{Context: 1000000, Output: 384000}, ToolCall: boolPtr(true)},
	}}
	got = r.enrich([]Model{{ID: "deepseek-v4-flash"}}, "deepseek")
	if got[0].ContextLength != 1000000 || got[0].MaxOutputTokens != 384000 {
		t.Errorf("exact deepseek path should still enrich, got %+v", got[0])
	}
}

func TestOllamaEnrichBothProviderTypes(t *testing.T) {
	r := NewRegistry()
	r.modelsDev = glmDataset()
	for _, typ := range []string{"ollama-local", "ollama-cloud"} {
		got := r.enrich([]Model{{ID: "glm-5.3-flash:latest"}}, typ)
		if got[0].ContextLength != 1000000 {
			t.Errorf("type %q: context = %d, want 1000000", typ, got[0].ContextLength)
		}
	}
}

func int64Ptr(v int64) *int64 { return &v }

func TestParseModelsDevReasoningOptions(t *testing.T) {
	cases := []struct {
		name string
		raw  []map[string]any
		want *ReasoningCapabilities
	}{
		{
			name: "effort values in canonical order",
			raw: []map[string]any{
				{"type": "effort", "values": []any{"high", "low", "medium", "xhigh", "max", "none"}},
			},
			want: &ReasoningCapabilities{Options: []ReasoningOption{
				{Type: ReasoningOptionEffort, Values: []string{"none", "low", "medium", "high", "xhigh", "max"}},
			}},
		},
		{
			name: "toggle option",
			raw:  []map[string]any{{"type": "toggle"}},
			want: &ReasoningCapabilities{Options: []ReasoningOption{{Type: ReasoningOptionToggle}}},
		},
		{
			name: "budget_tokens with min/max as integers",
			raw: []map[string]any{
				{"type": "budget_tokens", "min": float64(1024), "max": float64(262144)},
			},
			want: &ReasoningCapabilities{Options: []ReasoningOption{
				{Type: ReasoningOptionBudgetTokens, Min: int64Ptr(1024), Max: int64Ptr(262144)},
			}},
		},
		{
			name: "unknown effort values appended after known",
			raw: []map[string]any{
				{"type": "effort", "values": []any{"high", "ultra", "low", "mega"}},
			},
			want: &ReasoningCapabilities{Options: []ReasoningOption{
				{Type: ReasoningOptionEffort, Values: []string{"low", "high", "ultra", "mega"}},
			}},
		},
		{
			name: "duplicate effort values de-duplicated",
			raw: []map[string]any{
				{"type": "effort", "values": []any{"high", "low", "high", "medium"}},
			},
			want: &ReasoningCapabilities{Options: []ReasoningOption{
				{Type: ReasoningOptionEffort, Values: []string{"low", "medium", "high"}},
			}},
		},
		{
			name: "malformed option skipped, valid sibling retained",
			raw: []map[string]any{
				{"type": "effort"}, // missing values
				{"type": "toggle"},
			},
			want: &ReasoningCapabilities{Options: []ReasoningOption{{Type: ReasoningOptionToggle}}},
		},
		{
			name: "explicit empty list is known no-selector",
			raw:  []map[string]any{},
			want: &ReasoningCapabilities{Options: []ReasoningOption{}},
		},
		{
			name: "budget_tokens without min/max is still valid (unknown limits)",
			raw:  []map[string]any{{"type": "budget_tokens"}},
			want: &ReasoningCapabilities{Options: []ReasoningOption{
				{Type: ReasoningOptionBudgetTokens},
			}},
		},
		{
			name: "only effort-with-no-values is malformed -> nil",
			raw:  []map[string]any{{"type": "effort"}},
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseModelsDevReasoningOptions(tc.raw)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected %+v, got nil", tc.want)
			}
			if len(got.Options) != len(tc.want.Options) {
				t.Fatalf("options = %+v, want %+v", got.Options, tc.want.Options)
			}
			for i, opt := range got.Options {
				if opt.Type != tc.want.Options[i].Type {
					t.Errorf("option[%d].Type = %q, want %q", i, opt.Type, tc.want.Options[i].Type)
				}
				if !slicesEqual(opt.Values, tc.want.Options[i].Values) {
					t.Errorf("option[%d].Values = %v, want %v", i, opt.Values, tc.want.Options[i].Values)
				}
				if !int64PtrEqual(opt.Min, tc.want.Options[i].Min) {
					t.Errorf("option[%d].Min = %v, want %v", i, opt.Min, tc.want.Options[i].Min)
				}
				if !int64PtrEqual(opt.Max, tc.want.Options[i].Max) {
					t.Errorf("option[%d].Max = %v, want %v", i, opt.Max, tc.want.Options[i].Max)
				}
			}
		})
	}
}

func TestModelsDevReasoningOptionsPresence(t *testing.T) {
	for name, raw := range map[string]string{
		"absent": `{}`,
		"empty":  `{"reasoning_options":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var model modelsDevModel
			if err := json.Unmarshal([]byte(raw), &model); err != nil {
				t.Fatal(err)
			}
			caps := func() *ReasoningCapabilities {
				if model.ReasoningOptions == nil {
					return nil
				}
				return parseModelsDevReasoningOptions(*model.ReasoningOptions)
			}()
			if name == "absent" && caps != nil {
				t.Fatalf("absent reasoning_options should be unknown, got %+v", caps)
			}
			if name == "empty" && caps == nil {
				t.Fatal("explicit empty reasoning_options should be known")
			}
			if name == "empty" && len(caps.Options) != 0 {
				t.Fatalf("explicit empty reasoning_options = %+v", caps.Options)
			}
		})
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func int64PtrEqual(a, b *int64) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func TestEnrichMergesReasoningCapabilities(t *testing.T) {
	r := NewRegistry()
	r.modelsDev = modelsDevDataset{
		"deepseek": {Models: map[string]modelsDevModel{
			"deepseek-v4-flash": {
				Reasoning: boolPtr(true),
				ReasoningOptions: func() *[]map[string]any {
					v := []map[string]any{
						{"type": "effort", "values": []any{"low", "medium", "high"}},
					}
					return &v
				}(),
			},
		}},
	}
	got := r.enrich([]Model{{ID: "deepseek-v4-flash"}}, "deepseek")
	if got[0].ReasoningCapabilities == nil {
		t.Fatal("expected reasoning capabilities, got nil")
	}
	if len(got[0].ReasoningCapabilities.Options) != 1 {
		t.Fatalf("expected 1 option, got %d", len(got[0].ReasoningCapabilities.Options))
	}
	if got[0].ReasoningCapabilities.Options[0].Type != ReasoningOptionEffort {
		t.Errorf("option type = %q, want effort", got[0].ReasoningCapabilities.Options[0].Type)
	}
	want := []string{"low", "medium", "high"}
	if !slicesEqual(got[0].ReasoningCapabilities.Options[0].Values, want) {
		t.Errorf("effort values = %v, want %v", got[0].ReasoningCapabilities.Options[0].Values, want)
	}
}

func TestEnrichDoesNotOverrideProviderReportedReasoning(t *testing.T) {
	r := NewRegistry()
	r.modelsDev = modelsDevDataset{
		"deepseek": {Models: map[string]modelsDevModel{
			"deepseek-v4-flash": {
				Reasoning: boolPtr(true),
				ReasoningOptions: func() *[]map[string]any {
					v := []map[string]any{
						{"type": "effort", "values": []any{"low", "medium", "high"}},
					}
					return &v
				}(),
			},
		}},
	}
	providerCaps := &ReasoningCapabilities{Options: []ReasoningOption{
		{Type: ReasoningOptionEffort, Values: []string{"none", "low", "medium", "high", "xhigh", "max"}},
	}}
	got := r.enrich([]Model{{ID: "deepseek-v4-flash", ReasoningCapabilities: providerCaps}}, "deepseek")
	if len(got[0].ReasoningCapabilities.Options) != 1 || !slicesEqual(got[0].ReasoningCapabilities.Options[0].Values, providerCaps.Options[0].Values) {
		t.Errorf("provider-reported reasoning capabilities overridden: got %+v, want %+v", got[0].ReasoningCapabilities, providerCaps)
	}
}

func TestEnrichReasoningFillsMissingDirectMechanisms(t *testing.T) {
	r := NewRegistry()
	options := []map[string]any{{"type": "budget_tokens", "min": float64(1024), "max": float64(8192)}}
	r.modelsDev = modelsDevDataset{"deepseek": {Models: map[string]modelsDevModel{
		"deepseek-v4-flash": {ReasoningOptions: &options},
	}}}
	provider := &ReasoningCapabilities{Options: []ReasoningOption{{Type: ReasoningOptionEffort, Values: []string{"low"}}}}
	got := r.enrich([]Model{{ID: "deepseek-v4-flash", ReasoningCapabilities: provider}}, "deepseek")[0].ReasoningCapabilities
	if len(got.Options) != 2 || got.Options[0].Type != ReasoningOptionEffort || got.Options[1].Type != ReasoningOptionBudgetTokens {
		t.Fatalf("expected provider effort plus models.dev budget, got %+v", got.Options)
	}
}

func TestEnrichReasoningKeepsGatewayProviderMetadata(t *testing.T) {
	r := NewRegistry()
	options := []map[string]any{{"type": "budget_tokens"}}
	r.modelsDev = modelsDevDataset{"openrouter": {Models: map[string]modelsDevModel{
		"model": {ReasoningOptions: &options},
	}}}
	provider := &ReasoningCapabilities{Options: []ReasoningOption{{Type: ReasoningOptionEffort, Values: []string{"low"}}}}
	got := r.enrich([]Model{{ID: "model", ReasoningCapabilities: provider}}, "openrouter")[0].ReasoningCapabilities
	if len(got.Options) != 1 || got.Options[0].Type != ReasoningOptionEffort {
		t.Fatalf("gateway metadata should remain provider-only, got %+v", got.Options)
	}
}

func TestEnrichReasoningPreservesExplicitEmptyFallback(t *testing.T) {
	r := NewRegistry()
	options := []map[string]any{}
	r.modelsDev = modelsDevDataset{"deepseek": {Models: map[string]modelsDevModel{
		"deepseek-v4-flash": {ReasoningOptions: &options},
	}}}
	provider := &ReasoningCapabilities{}
	got := r.enrich([]Model{{ID: "deepseek-v4-flash", ReasoningCapabilities: provider}}, "deepseek")[0].ReasoningCapabilities
	if got == nil || got.Options == nil || len(got.Options) != 0 {
		t.Fatalf("explicit empty models.dev options should remain known-empty, got %+v", got)
	}
}
