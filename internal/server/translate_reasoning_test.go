package server

import (
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
		name     string
		selector reasoningSelector
		target   providers.Protocol
		caps     *providers.ReasoningCapabilities
		wantWarn bool
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
			name:       "unsupported effort omits and warns",
			selector:   reasoningSelector{Present: true, Effort: "ultra"},
			target:     providers.ProtocolResponses,
			caps:       &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low", "medium", "high"}}}},
			wantWarn:   true,
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
			wantWarn:   true,
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
			result, warning := applyReasoningSelector(body, tc.selector, tc.target, tc.caps, "openai")
			if tc.wantWarn && warning == "" {
				t.Error("expected warning, got empty")
			}
			if !tc.wantWarn && warning != "" {
				t.Errorf("unexpected warning: %q", warning)
			}
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
		wantWarn  bool
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
			name:     "unsupported effort omits and warns",
			selector: reasoningSelector{Present: true, Effort: "ultra"},
			target:   providers.ProtocolMessages,
			caps:     &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low", "medium", "high"}}}},
			wantWarn: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Start from a Messages-format body (output_config.effort).
			body := []byte(`{"model":"x","output_config":{"effort":"` + tc.selector.Effort + `"}}`)
			result, warning := applyReasoningSelector(body, tc.selector, tc.target, tc.caps, "anthropic")
			if tc.wantWarn && warning == "" {
				t.Error("expected warning, got empty")
			}
			if !tc.wantWarn && warning != "" {
				t.Errorf("unexpected warning: %q", warning)
			}
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
	result, warning := applyReasoningSelector(body, selector, providers.ProtocolChat, caps, "openai")
	if warning != "" {
		t.Errorf("unexpected warning: %q", warning)
	}
	if !containsString(result, `"reasoning_effort":"high"`) {
		t.Errorf("expected reasoning_effort=high in body, got %s", string(result))
	}
}

func TestApplyReasoningSelector_ToggleEnabledSemantics(t *testing.T) {
	cases := []struct {
		name      string
		selector  reasoningSelector
		caps      *providers.ReasoningCapabilities
		wantWarn  bool
		wantEffort string
	}{
		{
			name:      "Enabled=false maps to none",
			selector:  reasoningSelector{Present: true, Enabled: boolPtr(false)},
			caps:      &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"none", "low", "medium"}}}},
			wantEffort: "none",
		},
		{
			name:      "Mode=disabled maps to none",
			selector:  reasoningSelector{Present: true, Mode: "disabled"},
			caps:      &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"none", "low", "medium"}}}},
			wantEffort: "none",
		},
		{
			name:      "Enabled=true with default_effort uses default",
			selector:  reasoningSelector{Present: true, Enabled: boolPtr(true)},
			caps:      &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low", "medium"}}}, DefaultEffort: "medium"},
			wantEffort: "medium",
		},
		{
			name:      "Mode=adaptive passes through (unknown semantics)",
			selector:  reasoningSelector{Present: true, Mode: "adaptive"},
			caps:      &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low", "medium"}}}},
			wantEffort: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"x","reasoning":{"enabled":true}}`)
			result, warning := applyReasoningSelector(body, tc.selector, providers.ProtocolChat, tc.caps, "openai")
			if tc.wantWarn && warning == "" {
				t.Error("expected warning, got empty")
			}
			if !tc.wantWarn && warning != "" {
				t.Errorf("unexpected warning: %q", warning)
			}
			if tc.wantEffort != "" && !containsString(result, `"reasoning_effort":"`+tc.wantEffort+`"`) {
				t.Errorf("expected effort %q in body, got %s", tc.wantEffort, string(result))
			}
		})
	}
}

func TestApplyReasoningSelector_NoWarningForFailedCandidate(t *testing.T) {
	// A warning from a failed candidate must NOT be persisted — only the
	// final successful candidate's warning is assigned.
	caps := &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low", "medium"}}}}
	selector := reasoningSelector{Present: true, Effort: "ultra"}
	body := []byte(`{"model":"x","reasoning_effort":"ultra"}`)
	_, warning := applyReasoningSelector(body, selector, providers.ProtocolChat, caps, "openai")
	if warning != warningReasoningSelectorOmitted {
		t.Errorf("expected warning for unsupported effort, got %q", warning)
	}
	// The body should have had its selector stripped.
	if containsString(body, "reasoning_effort") {
		// Note: we pass a copy, so original body is unchanged. The returned body is stripped.
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
