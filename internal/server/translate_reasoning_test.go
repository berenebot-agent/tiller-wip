package server

import (
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/providers"
)

func int64PtrEqual(a, b *int64) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func boolPtrEqual(a, b *bool) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func TestExtractChatReasoning(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want reasoningSelector
	}{
		{
			name: "top-level reasoning_effort",
			body: map[string]any{"reasoning_effort": "high"},
			want: reasoningSelector{Present: true, Effort: "high"},
		},
		{
			name: "nested reasoning.effort",
			body: map[string]any{"reasoning": map[string]any{"effort": "medium"}},
			want: reasoningSelector{Present: true, Effort: "medium"},
		},
		{
			name: "top-level takes precedence over nested",
			body: map[string]any{"reasoning_effort": "high", "reasoning": map[string]any{"effort": "low"}},
			want: reasoningSelector{Present: true, Effort: "high"},
		},
		{
			name: "nested reasoning.max_tokens",
			body: map[string]any{"reasoning": map[string]any{"max_tokens": float64(8192)}},
			want: reasoningSelector{Present: true, BudgetTokens: int64Ptr(8192)},
		},
		{
			name: "nested reasoning.enabled=true",
			body: map[string]any{"reasoning": map[string]any{"enabled": true}},
			want: reasoningSelector{Present: true, Enabled: boolPtr(true)},
		},
		{
			name: "nested reasoning.enabled=false",
			body: map[string]any{"reasoning": map[string]any{"enabled": false}},
			want: reasoningSelector{Present: true, Enabled: boolPtr(false)},
		},
		{
			name: "no reasoning controls",
			body: map[string]any{"model": "gpt-5"},
			want: reasoningSelector{},
		},
		{
			name: "response-display field ignored (reasoning.summary)",
			body: map[string]any{"reasoning": map[string]any{"summary": "auto"}},
			want: reasoningSelector{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractChatReasoning(tc.body)
			if got.Present != tc.want.Present {
				t.Errorf("Present = %v, want %v", got.Present, tc.want.Present)
			}
			if got.Effort != tc.want.Effort {
				t.Errorf("Effort = %q, want %q", got.Effort, tc.want.Effort)
			}
			if !int64PtrEqual(got.BudgetTokens, tc.want.BudgetTokens) {
				t.Errorf("BudgetTokens = %v, want %v", got.BudgetTokens, tc.want.BudgetTokens)
			}
			if !boolPtrEqual(got.Enabled, tc.want.Enabled) {
				t.Errorf("Enabled = %v, want %v", got.Enabled, tc.want.Enabled)
			}
		})
	}
}

func TestExtractResponsesReasoning(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want reasoningSelector
	}{
		{
			name: "reasoning.effort",
			body: map[string]any{"reasoning": map[string]any{"effort": "low"}},
			want: reasoningSelector{Present: true, Effort: "low"},
		},
		{
			name: "summary ignored",
			body: map[string]any{"reasoning": map[string]any{"summary": "concise"}},
			want: reasoningSelector{},
		},
		{
			name: "no reasoning",
			body: map[string]any{"input": "hello"},
			want: reasoningSelector{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractResponsesReasoning(tc.body)
			if got.Present != tc.want.Present {
				t.Errorf("Present = %v, want %v", got.Present, tc.want.Present)
			}
			if got.Effort != tc.want.Effort {
				t.Errorf("Effort = %q, want %q", got.Effort, tc.want.Effort)
			}
		})
	}
}

func TestExtractMessagesReasoning(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want reasoningSelector
	}{
		{
			name: "output_config.effort",
			body: map[string]any{"output_config": map[string]any{"effort": "high"}},
			want: reasoningSelector{Present: true, Effort: "high"},
		},
		{
			name: "thinking.type=disabled",
			body: map[string]any{"thinking": map[string]any{"type": "disabled"}},
			want: reasoningSelector{Present: true, Mode: "disabled"},
		},
		{
			name: "thinking.type=adaptive",
			body: map[string]any{"thinking": map[string]any{"type": "adaptive"}},
			want: reasoningSelector{Present: true, Mode: "adaptive"},
		},
		{
			name: "thinking.budget_tokens",
			body: map[string]any{"thinking": map[string]any{"budget_tokens": float64(4096)}},
			want: reasoningSelector{Present: true, BudgetTokens: int64Ptr(4096)},
		},
		{
			name: "thinking.display ignored",
			body: map[string]any{"thinking": map[string]any{"display": "expanded"}},
			want: reasoningSelector{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractMessagesReasoning(tc.body)
			if got.Present != tc.want.Present {
				t.Errorf("Present = %v, want %v", got.Present, tc.want.Present)
			}
			if got.Effort != tc.want.Effort {
				t.Errorf("Effort = %q, want %q", got.Effort, tc.want.Effort)
			}
			if got.Mode != tc.want.Mode {
				t.Errorf("Mode = %q, want %q", got.Mode, tc.want.Mode)
			}
			if !int64PtrEqual(got.BudgetTokens, tc.want.BudgetTokens) {
				t.Errorf("BudgetTokens = %v, want %v", got.BudgetTokens, tc.want.BudgetTokens)
			}
		})
	}
}

func TestApplyReasoningSelector_ChatToResponses(t *testing.T) {
	cases := []struct {
		name       string
		selector   reasoningSelector
		target     providers.Protocol
		caps       *providers.ReasoningCapabilities
		wantEffort string
	}{
		{
			name:       "exact effort maps directly",
			selector:   reasoningSelector{Present: true, Effort: "high"},
			target:     providers.ProtocolResponses,
			caps:       &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low", "medium", "high"}}}},
			wantEffort: "high",
		},
		{
			name:       "unsupported effort is omitted silently",
			selector:   reasoningSelector{Present: true, Effort: "ultra"},
			target:     providers.ProtocolResponses,
			caps:       &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low", "medium", "high"}}}},
			wantEffort: "",
		},
		{
			name:       "unknown support passes through",
			selector:   reasoningSelector{Present: true, Effort: "high"},
			target:     providers.ProtocolResponses,
			caps:       nil,
			wantEffort: "high",
		},
		{
			name:       "minimal only when advertised",
			selector:   reasoningSelector{Present: true, Effort: "minimal"},
			target:     providers.ProtocolResponses,
			caps:       &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low", "medium", "high"}}}},
			wantEffort: "",
		},
		{
			name:       "minimal advertised maps directly",
			selector:   reasoningSelector{Present: true, Effort: "minimal"},
			target:     providers.ProtocolResponses,
			caps:       &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"minimal", "low", "medium"}}}},
			wantEffort: "minimal",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"x","reasoning_effort":"high"}`)
			if tc.selector.Effort != "high" {
				body = []byte(`{"model":"x","reasoning_effort":"` + tc.selector.Effort + `"}`)
			}
			result := applyReasoningSelector(body, tc.selector, tc.target, tc.caps)
			if tc.wantEffort != "" {
				if !containsString(result, `"reasoning":{"effort":"`+tc.wantEffort+`"}`) && !containsString(result, `"reasoning": {"effort": "`+tc.wantEffort+`"}`) {
					t.Errorf("expected effort %q in body, got %s", tc.wantEffort, string(result))
				}
			}
		})
	}
}

func TestApplyReasoningSelector_ChatToMessages(t *testing.T) {
	cases := []struct {
		name      string
		selector  reasoningSelector
		target    providers.Protocol
		caps      *providers.ReasoningCapabilities
		wantField string
	}{
		{
			name:      "exact effort maps to output_config.effort",
			selector:  reasoningSelector{Present: true, Effort: "medium"},
			target:    providers.ProtocolMessages,
			caps:      &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low", "medium", "high"}}}},
			wantField: `"effort":"medium"`,
		},
		{
			name:      "none maps to thinking.disabled when supported",
			selector:  reasoningSelector{Present: true, Effort: "none"},
			target:    providers.ProtocolMessages,
			caps:      &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"none", "low", "medium"}}}},
			wantField: `"type":"disabled"`,
		},
		{
			name:     "unsupported effort is omitted silently",
			selector: reasoningSelector{Present: true, Effort: "ultra"},
			target:   providers.ProtocolMessages,
			caps:     &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low", "medium", "high"}}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Start from a Messages-format body (output_config.effort).
			body := []byte(`{"model":"x","output_config":{"effort":"` + tc.selector.Effort + `"}}`)
			result := applyReasoningSelector(body, tc.selector, tc.target, tc.caps)
			if tc.wantField != "" && !containsString(result, tc.wantField) {
				t.Errorf("expected %q in body, got %s", tc.wantField, string(result))
			}
		})
	}
}

func TestApplyReasoningSelector_MessagesToChat(t *testing.T) {
	caps := &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low", "medium", "high"}}}}
	body := []byte(`{"model":"x","output_config":{"effort":"high"}}`)
	selector := extractMessagesReasoning(map[string]any{
		"output_config": map[string]any{"effort": "high"},
	})
	result := applyReasoningSelector(body, selector, providers.ProtocolChat, caps)
	if !containsString(result, `"reasoning_effort":"high"`) {
		t.Errorf("expected reasoning_effort=high in body, got %s", string(result))
	}
}

func TestApplyReasoningSelector_ToggleEnabledSemantics(t *testing.T) {
	cases := []struct {
		name       string
		selector   reasoningSelector
		caps       *providers.ReasoningCapabilities
		wantEffort string
	}{
		{
			name:       "Enabled=false maps to none",
			selector:   reasoningSelector{Present: true, Enabled: boolPtr(false)},
			caps:       &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"none", "low", "medium"}}}},
			wantEffort: "none",
		},
		{
			name:       "Mode=disabled maps to none",
			selector:   reasoningSelector{Present: true, Mode: "disabled"},
			caps:       &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"none", "low", "medium"}}}},
			wantEffort: "none",
		},
		{
			name:       "Enabled=true with default_effort uses default",
			selector:   reasoningSelector{Present: true, Enabled: boolPtr(true)},
			caps:       &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low", "medium"}}}, DefaultEffort: "medium"},
			wantEffort: "medium",
		},
		{
			name:       "Mode=adaptive passes through (unknown semantics)",
			selector:   reasoningSelector{Present: true, Mode: "adaptive"},
			caps:       &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low", "medium"}}}},
			wantEffort: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"x","reasoning":{"enabled":true}}`)
			result := applyReasoningSelector(body, tc.selector, providers.ProtocolChat, tc.caps)
			if tc.wantEffort != "" && !containsString(result, `"reasoning_effort":"`+tc.wantEffort+`"`) {
				t.Errorf("expected effort %q in body, got %s", tc.wantEffort, string(result))
			}
		})
	}
}

func TestApplyReasoningSelector_SilentlyOmitsUnsupportedEffort(t *testing.T) {
	caps := &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low", "medium"}}}}
	selector := reasoningSelector{Present: true, Effort: "ultra"}
	body := []byte(`{"model":"x","reasoning_effort":"ultra"}`)
	result := applyReasoningSelector(body, selector, providers.ProtocolChat, caps)
	if containsString(result, "reasoning_effort") {
		t.Errorf("unsupported effort survived: %s", result)
	}
}

func TestApplyReasoningSelector_BudgetApplication(t *testing.T) {
	min, max := int64(1024), int64(8192)
	cases := []struct {
		name, body, field string
		target            providers.Protocol
		selector          reasoningSelector
		caps              *providers.ReasoningCapabilities
		wantField         bool
	}{
		{
			name: "Chat budget in range", body: `{"model":"x"}`, field: `"max_tokens":4096`, target: providers.ProtocolChat,
			selector: reasoningSelector{Present: true, BudgetTokens: int64Ptr(4096)},
			caps:     &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionBudgetTokens, Min: &min, Max: &max}}}, wantField: true,
		},
		{
			name: "Messages budget in range", body: `{"model":"x"}`, field: `"budget_tokens":4096`, target: providers.ProtocolMessages,
			selector: reasoningSelector{Present: true, BudgetTokens: int64Ptr(4096)},
			caps:     &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionBudgetTokens, Min: &min, Max: &max}}}, wantField: true,
		},
		{
			name: "out of range budget is omitted", body: `{"model":"x","reasoning":{"max_tokens":128}}`, field: `"max_tokens"`, target: providers.ProtocolChat,
			selector: reasoningSelector{Present: true, BudgetTokens: int64Ptr(128)},
			caps:     &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionBudgetTokens, Min: &min, Max: &max}}}, wantField: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := applyReasoningSelector([]byte(tc.body), tc.selector, tc.target, tc.caps)
			if containsString(result, tc.field) != tc.wantField {
				t.Fatalf("budget field presence = %v, want %v: %s", containsString(result, tc.field), tc.wantField, result)
			}
		})
	}
}

func TestApplyReasoningSelector_NoSelectorPreservesBytes(t *testing.T) {
	body := []byte(`{ "model": "x", "messages": [{"role":"user","content":"hi"}] }`)
	result := applyReasoningSelector(body, reasoningSelector{}, providers.ProtocolChat, &providers.ReasoningCapabilities{})
	if string(result) != string(body) {
		t.Fatalf("body changed without a selector: got %q, want %q", result, body)
	}
}

func TestApplyReasoningSelector_T7AcrossProtocols(t *testing.T) {
	type protocolCase struct {
		name     string
		target   providers.Protocol
		caps     *providers.ReasoningCapabilities
		body     string
		selector reasoningSelector
		check    func(string) bool
	}
	effortCaps := func() *providers.ReasoningCapabilities {
		return &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low", "high"}}}}
	}
	tests := []protocolCase{
		{name: "Chat disable wins over effort", target: providers.ProtocolChat, body: `{"model":"x"}`, selector: reasoningSelector{Present: true, Enabled: boolPtr(false), Effort: "high"}, caps: &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionToggle}, {Type: providers.ReasoningOptionEffort, Values: []string{"low", "high"}}}}, check: func(s string) bool {
			return strings.Contains(s, `"enabled":false`) && !strings.Contains(s, `reasoning_effort`)
		}},
		{name: "Responses disable wins over effort", target: providers.ProtocolResponses, body: `{"model":"x"}`, selector: reasoningSelector{Present: true, Enabled: boolPtr(false), Effort: "high"}, caps: &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionToggle}, {Type: providers.ReasoningOptionEffort, Values: []string{"low", "high"}}}}, check: func(s string) bool { return strings.Contains(s, `"enabled":false`) && !strings.Contains(s, `"effort"`) }},
		{name: "Messages disable wins over effort", target: providers.ProtocolMessages, body: `{"model":"x"}`, selector: reasoningSelector{Present: true, Enabled: boolPtr(false), Effort: "high"}, caps: &providers.ReasoningCapabilities{ThinkingModes: []string{"enabled"}, Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"none", "low", "high"}}}}, check: func(s string) bool {
			return strings.Contains(s, `"type":"disabled"`) && !strings.Contains(s, `"effort"`)
		}},
		{name: "Chat enable and effort", target: providers.ProtocolChat, body: `{"model":"x"}`, selector: reasoningSelector{Present: true, Enabled: boolPtr(true), Effort: "high"}, caps: &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionToggle}, {Type: providers.ReasoningOptionEffort, Values: []string{"low", "high"}}}}, check: func(s string) bool {
			return strings.Contains(s, `"enabled":true`) && strings.Contains(s, `"reasoning_effort":"high"`)
		}},
		{name: "Responses enable and effort", target: providers.ProtocolResponses, body: `{"model":"x"}`, selector: reasoningSelector{Present: true, Enabled: boolPtr(true), Effort: "high"}, caps: &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionToggle}, {Type: providers.ReasoningOptionEffort, Values: []string{"low", "high"}}}}, check: func(s string) bool {
			return strings.Contains(s, `"enabled":true`) && strings.Contains(s, `"effort":"high"`)
		}},
		{name: "Messages enable and effort", target: providers.ProtocolMessages, body: `{"model":"x"}`, selector: reasoningSelector{Present: true, Enabled: boolPtr(true), Effort: "high"}, caps: &providers.ReasoningCapabilities{ThinkingModes: []string{"enabled"}, Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low", "high"}}}}, check: func(s string) bool {
			return strings.Contains(s, `"type":"enabled"`) && strings.Contains(s, `"effort":"high"`)
		}},
		{name: "Chat disable alone", target: providers.ProtocolChat, body: `{"model":"x"}`, selector: reasoningSelector{Present: true, Enabled: boolPtr(false)}, caps: effortCaps(), check: func(s string) bool { return !strings.Contains(s, "reasoning") }},
		{name: "Responses disable alone", target: providers.ProtocolResponses, body: `{"model":"x"}`, selector: reasoningSelector{Present: true, Enabled: boolPtr(false)}, caps: effortCaps(), check: func(s string) bool { return !strings.Contains(s, "reasoning") }},
		{name: "Messages disable alone", target: providers.ProtocolMessages, body: `{"model":"x"}`, selector: reasoningSelector{Present: true, Enabled: boolPtr(false)}, caps: effortCaps(), check: func(s string) bool { return !strings.Contains(s, "thinking") }},
		{name: "Chat effort alone", target: providers.ProtocolChat, body: `{"model":"x"}`, selector: reasoningSelector{Present: true, Effort: "high"}, caps: effortCaps(), check: func(s string) bool { return strings.Contains(s, `"reasoning_effort":"high"`) }},
		{name: "Responses effort alone", target: providers.ProtocolResponses, body: `{"model":"x"}`, selector: reasoningSelector{Present: true, Effort: "high"}, caps: effortCaps(), check: func(s string) bool { return strings.Contains(s, `"effort":"high"`) }},
		{name: "Messages effort alone", target: providers.ProtocolMessages, body: `{"model":"x"}`, selector: reasoningSelector{Present: true, Effort: "high"}, caps: effortCaps(), check: func(s string) bool { return strings.Contains(s, `"effort":"high"`) }},
		{name: "Chat unsupported disable plus effort defaults", target: providers.ProtocolChat, body: `{"model":"x","reasoning":{"enabled":true,"max_tokens":512}}`, selector: reasoningSelector{Present: true, Enabled: boolPtr(false), Effort: "high"}, caps: effortCaps(), check: func(s string) bool { return !strings.Contains(s, "reasoning") }},
		{name: "Responses unsupported disable plus effort defaults", target: providers.ProtocolResponses, body: `{"model":"x","reasoning":{"enabled":true,"effort":"low"}}`, selector: reasoningSelector{Present: true, Enabled: boolPtr(false), Effort: "high"}, caps: effortCaps(), check: func(s string) bool { return !strings.Contains(s, "reasoning") }},
		{name: "Messages unsupported disable plus effort defaults", target: providers.ProtocolMessages, body: `{"model":"x","thinking":{"type":"enabled","budget_tokens":512}}`, selector: reasoningSelector{Present: true, Enabled: boolPtr(false), Effort: "high"}, caps: effortCaps(), check: func(s string) bool { return !strings.Contains(s, "thinking") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := applyReasoningSelector([]byte(tc.body), tc.selector, tc.target, tc.caps)
			if !tc.check(string(result)) {
				t.Fatalf("mapped body did not satisfy selector semantics: %s", result)
			}
		})
	}
}

func TestApplyReasoningSelector_ToggleOnlyTarget(t *testing.T) {
	// A model whose only capability is Toggle (no effort, no budget) must NOT
	// be treated as explicitly non-reasoning.
	caps := &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionToggle}}}
	selector := reasoningSelector{Present: true, Enabled: boolPtr(true)}
	body := []byte(`{"model":"x","reasoning":{"enabled":true}}`)
	result := applyReasoningSelector(body, selector, providers.ProtocolChat, caps)
	// Body should be preserved (pass-through).
	if !containsString(result, "enabled") {
		t.Errorf("expected body to be preserved for toggle-only target, got %s", string(result))
	}
}

func TestApplyReasoningSelector_AdaptivePreserved(t *testing.T) {
	caps := &providers.ReasoningCapabilities{ThinkingModes: []string{"adaptive"}}
	selector := reasoningSelector{Present: true, Mode: "adaptive"}
	body := []byte(`{"model":"x","thinking":{"type":"adaptive"}}`)
	result := applyReasoningSelector(body, selector, providers.ProtocolMessages, caps)
	if !containsString(result, "adaptive") {
		t.Errorf("expected adaptive thinking preserved, got %s", string(result))
	}
}

func TestApplyReasoningSelector_ChatPreservesNonSelectorFields(t *testing.T) {
	// stripReasoningSelector for Chat must preserve reasoning.exclude.
	caps := &providers.ReasoningCapabilities{} // explicitly unsupported
	selector := reasoningSelector{Present: true, Effort: "high"}
	body := []byte(`{"model":"x","reasoning_effort":"high","reasoning":{"exclude":true}}`)
	result := applyReasoningSelector(body, selector, providers.ProtocolChat, caps)
	if !containsString(result, `"exclude":true`) {
		t.Errorf("expected reasoning.exclude preserved, got %s", string(result))
	}
	if containsString(result, "reasoning_effort") {
		t.Errorf("expected reasoning_effort stripped, got %s", string(result))
	}
}

func TestApplyReasoningSelector_OpenRouterMandatoryNone(t *testing.T) {
	// OpenRouter: mandatory=true + default_effort=none means reasoning is
	// required; don't use "none" to force-enable when user explicitly enables.
	caps := &providers.ReasoningCapabilities{
		Options:       []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low", "medium", "high"}}},
		DefaultEffort: "none",
		Mandatory:     boolPtr(true),
	}
	selector := reasoningSelector{Present: true, Enabled: boolPtr(true)}
	body := []byte(`{"model":"x","reasoning":{"enabled":true}}`)
	result := applyReasoningSelector(body, selector, providers.ProtocolChat, caps)
	// Should not emit reasoning_effort=none (mandatory prevents none).
	if containsString(result, "reasoning_effort") {
		t.Errorf("expected no effort emitted for mandatory+none default, got %s", string(result))
	}
}

func TestApplyReasoningSelector_TranslateRequestThenMap(t *testing.T) {
	// Full path: Chat→Messages via translateRequest then applyReasoningSelector.
	chatBody := []byte(`{"model":"client-x","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`)
	translated, err := translateRequest(chatBody, providers.ProtocolChat, providers.ProtocolMessages, "claude-opus-5")
	if err != nil {
		t.Fatalf("translateRequest: %v", err)
	}
	caps := &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low", "medium", "high"}}}}
	result := applyReasoningSelector(translated, extractReasoningSelector(chatBody, providers.ProtocolChat), providers.ProtocolMessages, caps)
	if !containsString(result, `"effort":"high"`) {
		t.Errorf("expected effort=high in Messages body, got %s", string(result))
	}
}

func TestApplyReasoningSelector_TranslateEnabledToAdaptiveMessages(t *testing.T) {
	chatBody := []byte(`{"model":"client-x","messages":[{"role":"user","content":"hi"}],"reasoning":{"enabled":true}}`)
	translated, err := translateRequest(chatBody, providers.ProtocolChat, providers.ProtocolMessages, "claude-opus-5")
	if err != nil {
		t.Fatalf("translateRequest: %v", err)
	}
	caps := &providers.ReasoningCapabilities{ThinkingModes: []string{"adaptive"}}
	result := applyReasoningSelector(translated, extractReasoningSelector(chatBody, providers.ProtocolChat), providers.ProtocolMessages, caps)
	if !containsString(result, `"type":"adaptive"`) {
		t.Fatalf("expected adaptive thinking in Messages body, got %s", string(result))
	}
}

func TestApplyReasoningSelector_MaterializesTranslatedControls(t *testing.T) {
	tests := []struct {
		name       string
		selector   reasoningSelector
		body       []byte
		target     providers.Protocol
		caps       *providers.ReasoningCapabilities
		wantFields []string
	}{
		{
			name:       "Messages adaptive to Messages",
			selector:   reasoningSelector{Present: true, Mode: "adaptive"},
			body:       []byte(`{"model":"x"}`),
			target:     providers.ProtocolMessages,
			caps:       &providers.ReasoningCapabilities{ThinkingModes: []string{"adaptive"}},
			wantFields: []string{`"type":"adaptive"`},
		},
		{
			name:       "enabled to adaptive-only Messages",
			selector:   reasoningSelector{Present: true, Enabled: boolPtr(true)},
			body:       []byte(`{"model":"x"}`),
			target:     providers.ProtocolMessages,
			caps:       &providers.ReasoningCapabilities{ThinkingModes: []string{"adaptive"}},
			wantFields: []string{`"type":"adaptive"`},
		},
		{
			name:       "enabled to toggle-only Chat",
			selector:   reasoningSelector{Present: true, Enabled: boolPtr(true)},
			body:       []byte(`{"model":"x"}`),
			target:     providers.ProtocolChat,
			caps:       &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionToggle}}},
			wantFields: []string{`"enabled":true`},
		},
		{
			name:     "adaptive to Chat is omitted",
			selector: reasoningSelector{Present: true, Mode: "adaptive"},
			body:     []byte(`{"model":"x"}`),
			target:   providers.ProtocolChat,
			caps:     &providers.ReasoningCapabilities{ThinkingModes: []string{"adaptive"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := applyReasoningSelector(tc.body, tc.selector, tc.target, tc.caps)
			for _, field := range tc.wantFields {
				if !containsString(result, field) {
					t.Errorf("expected %s in %s", field, result)
				}
			}
		})
	}
}

func TestApplyReasoningSelectorRejectsIncompatibleMechanism(t *testing.T) {
	caps := &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionToggle}}}
	result := applyReasoningSelector([]byte(`{"model":"x","reasoning_effort":"high"}`), reasoningSelector{Present: true, Effort: "high"}, providers.ProtocolChat, caps)
	if containsString(result, "reasoning_effort") {
		t.Fatalf("incompatible effort survived: %s", result)
	}
}

func TestStripReasoningSelectorMessagesRemovesEmptyObjects(t *testing.T) {
	result := stripReasoningSelector([]byte(`{"model":"x","output_config":{"effort":"high"},"thinking":{"type":"enabled"}}`), providers.ProtocolMessages)
	if containsString(result, "output_config") || containsString(result, "thinking") {
		t.Fatalf("empty selector objects survived: %s", result)
	}
}

func TestMergeReasoningCapabilitiesPreservesModesAndUnrestrictedEffort(t *testing.T) {
	merged := mergeReasoningCapabilities(
		&providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low"}}}, ThinkingModes: []string{"enabled"}},
		&providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort}}, ThinkingModes: []string{"adaptive"}},
	)
	if merged == nil || len(merged.Options) != 1 || len(merged.Options[0].Values) != 0 {
		t.Fatalf("merged effort = %#v", merged)
	}
	if len(merged.ThinkingModes) != 2 || merged.ThinkingModes[0] != "adaptive" || merged.ThinkingModes[1] != "enabled" {
		t.Fatalf("merged thinking modes = %v", merged.ThinkingModes)
	}
}

func TestTranslateNonstreamReasoning_MessagesUpstream_ClientChat(t *testing.T) {
	body := []byte(`{
		"id": "msg_123",
		"type": "message",
		"role": "assistant",
		"model": "claude-real",
		"content": [
			{"type": "thinking", "thinking": "let me think about this"},
			{"type": "text", "text": "the answer is 42"}
		],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 10, "output_tokens": 20}
	}`)
	translated, err := translateNonstreamResponse(body, providers.ProtocolChat, providers.ProtocolMessages, "client-x")
	if err != nil {
		t.Fatalf("translateNonstreamResponse: %v", err)
	}
	if !containsString(translated, `"reasoning_content":"let me think about this"`) {
		t.Errorf("reasoning_content lost in Messages upstream->Chat client: %s", translated)
	}
	if !containsString(translated, `"text":"the answer is 42"`) {
		t.Errorf("text content lost in Messages upstream->Chat client: %s", translated)
	}
}

func TestTranslateNonstreamReasoning_MessagesUpstream_ClientResponses(t *testing.T) {
	body := []byte(`{
		"id": "msg_123",
		"type": "message",
		"role": "assistant",
		"model": "claude-real",
		"content": [
			{"type": "thinking", "thinking": "let me think about this"},
			{"type": "text", "text": "the answer is 42"}
		],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 10, "output_tokens": 20}
	}`)
	translated, err := translateNonstreamResponse(body, providers.ProtocolResponses, providers.ProtocolMessages, "client-x")
	if err != nil {
		t.Fatalf("translateNonstreamResponse: %v", err)
	}
	if !containsString(translated, `"type":"reasoning"`) {
		t.Errorf("reasoning output lost in Messages upstream->Responses client: %s", translated)
	}
	if !containsString(translated, `let me think about this`) {
		t.Errorf("reasoning text lost in Messages upstream->Responses client: %s", translated)
	}
	if !containsString(translated, `"text":"the answer is 42"`) {
		t.Errorf("text content lost in Messages upstream->Responses client: %s", translated)
	}
}

func TestTranslateNonstreamReasoning_ResponsesUpstream_ClientChat(t *testing.T) {
	body := []byte(`{
		"id": "resp_123",
		"object": "response",
		"created_at": 1700000000,
		"status": "completed",
		"model": "gpt-real",
		"output": [
			{
				"id": "msg_123",
				"type": "message",
				"role": "assistant",
				"status": "completed",
				"content": [
					{"type": "output_text", "text": "the answer is 42", "annotations": []}
				]
			},
			{
				"id": "rs_123",
				"type": "reasoning",
				"summary": [{"type": "summary_text", "text": "let me think about this"}]
			}
		],
		"usage": {"input_tokens": 10, "output_tokens": 20, "total_tokens": 30}
	}`)
	translated, err := translateNonstreamResponse(body, providers.ProtocolChat, providers.ProtocolResponses, "client-x")
	if err != nil {
		t.Fatalf("translateNonstreamResponse: %v", err)
	}
	if !containsString(translated, `"reasoning_content":"let me think about this"`) {
		t.Errorf("reasoning_content lost in Responses upstream->Chat client: %s", translated)
	}
	if !containsString(translated, `"text":"the answer is 42"`) {
		t.Errorf("text content lost in Responses upstream->Chat client: %s", translated)
	}
}

func TestTranslateNonstreamReasoning_ResponsesUpstream_ClientMessages(t *testing.T) {
	body := []byte(`{
		"id": "resp_123",
		"object": "response",
		"created_at": 1700000000,
		"status": "completed",
		"model": "gpt-real",
		"output": [
			{
				"id": "msg_123",
				"type": "message",
				"role": "assistant",
				"status": "completed",
				"content": [
					{"type": "output_text", "text": "the answer is 42", "annotations": []}
				]
			},
			{
				"id": "rs_123",
				"type": "reasoning",
				"summary": [{"type": "summary_text", "text": "let me think about this"}]
			}
		],
		"usage": {"input_tokens": 10, "output_tokens": 20, "total_tokens": 30}
	}`)
	translated, err := translateNonstreamResponse(body, providers.ProtocolMessages, providers.ProtocolResponses, "client-x")
	if err != nil {
		t.Fatalf("translateNonstreamResponse: %v", err)
	}
	if !containsString(translated, `"type":"thinking"`) {
		t.Errorf("thinking block lost in Responses upstream->Messages client: %s", translated)
	}
	if !containsString(translated, `let me think about this`) {
		t.Errorf("reasoning text lost in Responses upstream->Messages client: %s", translated)
	}
	if !containsString(translated, `"text":"the answer is 42"`) {
		t.Errorf("text content lost in Responses upstream->Messages client: %s", translated)
	}
}

func TestTranslateNonstreamReasoning_ChatReasoningUpstream_ClientResponses(t *testing.T) {
	body := []byte(`{
		"id": "chat_123",
		"object": "chat.completion",
		"created": 1700000000,
		"model": "gpt-real",
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"content": "the answer is 42",
				"reasoning_content": "let me think about this"
			},
			"finish_reason": "stop"
		}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30}
	}`)
	translated, err := translateNonstreamResponse(body, providers.ProtocolResponses, providers.ProtocolChat, "client-x")
	if err != nil {
		t.Fatalf("translateNonstreamResponse: %v", err)
	}
	if !containsString(translated, `"type":"reasoning"`) {
		t.Errorf("reasoning output lost in Chat upstream->Responses client: %s", translated)
	}
	if !containsString(translated, `let me think about this`) {
		t.Errorf("reasoning text lost in Chat upstream->Responses client: %s", translated)
	}
	if !containsString(translated, `"text":"the answer is 42"`) {
		t.Errorf("text content lost in Chat upstream->Responses client: %s", translated)
	}
}

func TestTranslateNonstreamReasoning_ChatReasoningUpstream_ClientMessages(t *testing.T) {
	body := []byte(`{
		"id": "chat_123",
		"object": "chat.completion",
		"created": 1700000000,
		"model": "gpt-real",
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"content": "the answer is 42",
				"reasoning_content": "let me think about this"
			},
			"finish_reason": "stop"
		}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30}
	}`)
	translated, err := translateNonstreamResponse(body, providers.ProtocolMessages, providers.ProtocolChat, "client-x")
	if err != nil {
		t.Fatalf("translateNonstreamResponse: %v", err)
	}
	if !containsString(translated, `"type":"thinking"`) {
		t.Errorf("thinking block lost in Chat upstream->Messages client: %s", translated)
	}
	if !containsString(translated, `let me think about this`) {
		t.Errorf("reasoning text lost in Chat upstream->Messages client: %s", translated)
	}
	if !containsString(translated, `"text":"the answer is 42"`) {
		t.Errorf("text content lost in Chat upstream->Messages client: %s", translated)
	}
}

func TestTranslateNonstreamReasoning_NoReasoningPreservesText(t *testing.T) {
	body := []byte(`{
		"id": "chat_123",
		"object": "chat.completion",
		"created": 1700000000,
		"model": "gpt-real",
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"content": "the answer is 42"
			},
			"finish_reason": "stop"
		}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30}
	}`)
	translated, err := translateNonstreamResponse(body, providers.ProtocolMessages, providers.ProtocolChat, "client-x")
	if err != nil {
		t.Fatalf("translateNonstreamResponse: %v", err)
	}
	if containsString(translated, "reasoning") || containsString(translated, "thinking") {
		t.Errorf("spurious reasoning in Chat upstream->Messages client without reasoning: %s", translated)
	}
	if !containsString(translated, `"text":"the answer is 42"`) {
		t.Errorf("text content lost: %s", translated)
	}
}

func containsString(b []byte, s string) bool {
	for i := 0; i+len(s) <= len(b); i++ {
		if string(b[i:i+len(s)]) == s {
			return true
		}
	}
	return false
}
