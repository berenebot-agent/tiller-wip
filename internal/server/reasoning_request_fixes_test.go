package server

import (
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/providers"
)

func TestReasoningRequestFixes_UnknownDisableUsesStandardSelectors(t *testing.T) {
	selector := reasoningSelector{Present: true, Enabled: boolPtr(false)}
	chat := applyReasoningSelector([]byte(`{"model":"x"}`), selector, providers.ProtocolChat, nil)
	if string(chat) != `{"model":"x","reasoning_effort":"none"}` {
		t.Fatalf("Chat disable = %s", chat)
	}
	responses := applyReasoningSelector([]byte(`{"model":"x"}`), selector, providers.ProtocolResponses, nil)
	if string(responses) != `{"model":"x","reasoning":{"effort":"none"}}` {
		t.Fatalf("Responses disable = %s", responses)
	}
	if strings.Contains(string(responses), `"enabled"`) || strings.Contains(string(responses), `"mode"`) {
		t.Fatalf("Responses disable invented non-standard selector: %s", responses)
	}
}

func TestReasoningRequestFixes_UnknownPositiveEffortEnablesMessagesWithBudget(t *testing.T) {
	selector := reasoningSelector{Present: true, Effort: "high"}
	result := applyReasoningSelector([]byte(`{"model":"x"}`), selector, providers.ProtocolMessages, nil)
	if !strings.Contains(string(result), `"type":"enabled"`) || !strings.Contains(string(result), `"budget_tokens":1024`) {
		t.Fatalf("positive effort did not produce valid enabled thinking: %s", result)
	}
}

func TestReasoningRequestFixes_AdaptiveNeverCarriesBudget(t *testing.T) {
	min := int64(1024)
	caps := &providers.ReasoningCapabilities{
		ThinkingModes: []string{"adaptive"},
		Options:       []providers.ReasoningOption{{Type: providers.ReasoningOptionBudgetTokens, Min: &min}},
	}
	result := applyReasoningSelector([]byte(`{"model":"x"}`), reasoningSelector{Present: true, Mode: "adaptive", BudgetTokens: int64Ptr(2048)}, providers.ProtocolMessages, caps)
	if !strings.Contains(string(result), `"type":"adaptive"`) || strings.Contains(string(result), "budget_tokens") {
		t.Fatalf("adaptive thinking had invalid budget: %s", result)
	}
}

func TestReasoningRequestFixes_DoesNotRaiseExplicitOutputCap(t *testing.T) {
	min := int64(1024)
	caps := &providers.ReasoningCapabilities{
		ThinkingModes: []string{"enabled"},
		Options:       []providers.ReasoningOption{{Type: providers.ReasoningOptionBudgetTokens, Min: &min}},
	}
	body := []byte(`{"model":"x","max_output_tokens":512}`)
	result := applyReasoningSelector(body, reasoningSelector{Present: true, Effort: "high"}, providers.ProtocolMessages, caps)
	if string(result) != string(body) {
		t.Fatalf("invalid default budget changed request or raised cap: %s", result)
	}
}

func TestReasoningRequestFixes_EnabledWithoutBudgetMetadataUsesDefault(t *testing.T) {
	caps := &providers.ReasoningCapabilities{ThinkingModes: []string{"enabled"}}
	result := applyReasoningSelector([]byte(`{"model":"x"}`), reasoningSelector{Present: true, Effort: "high"}, providers.ProtocolMessages, caps)
	if !strings.Contains(string(result), `"type":"enabled"`) || !strings.Contains(string(result), `"budget_tokens":1024`) || !strings.Contains(string(result), `"effort":"high"`) {
		t.Fatalf("enabled-only target did not get valid default and effort: %s", result)
	}
}

func TestReasoningRequestFixes_BudgetOnlyGetsEnabledType(t *testing.T) {
	min := int64(512)
	caps := &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionBudgetTokens, Min: &min}}}
	result := applyReasoningSelector([]byte(`{"model":"x"}`), reasoningSelector{Present: true, BudgetTokens: int64Ptr(1024)}, providers.ProtocolMessages, caps)
	if !strings.Contains(string(result), `"type":"enabled"`) || !strings.Contains(string(result), `"budget_tokens":1024`) {
		t.Fatalf("budget-only target produced orphan budget: %s", result)
	}
}

func TestReasoningRequestFixes_OutputCapUsesMessagesMaxTokensStrictly(t *testing.T) {
	caps := &providers.ReasoningCapabilities{ThinkingModes: []string{"enabled"}}
	body := []byte(`{"model":"x","max_tokens":1024}`)
	result := applyReasoningSelector(body, reasoningSelector{Present: true, Effort: "high"}, providers.ProtocolMessages, caps)
	if string(result) != string(body) {
		t.Fatalf("budget equal to max_tokens should not be emitted: %s", result)
	}
}

func TestReasoningRequestFixes_UnknownChatEnableDoesNotInventNestedToggle(t *testing.T) {
	result := applyReasoningSelector([]byte(`{"model":"x"}`), reasoningSelector{Present: true, Enabled: boolPtr(true)}, providers.ProtocolChat, nil)
	if strings.Contains(string(result), `"enabled"`) || strings.Contains(string(result), `"mode"`) {
		t.Fatalf("unknown Chat capability invented selector: %s", result)
	}
}

func TestReasoningRequestFixes_TranslatedEffortRetainedWithEnabledMessages(t *testing.T) {
	request := []byte(`{"model":"client","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`)
	translated, err := translateRequest(request, providers.ProtocolChat, providers.ProtocolMessages, "claude")
	if err != nil {
		t.Fatal(err)
	}
	caps := &providers.ReasoningCapabilities{ThinkingModes: []string{"enabled"}}
	result := applyReasoningSelector(translated, extractReasoningSelector(request, providers.ProtocolChat), providers.ProtocolMessages, caps)
	if !strings.Contains(string(result), `"type":"enabled"`) || !strings.Contains(string(result), `"effort":"high"`) {
		t.Fatalf("translated effort was not retained: %s", result)
	}
}

func TestReasoningRequestFixes_EnabledTrueAdaptiveOnlyPreservesEffort(t *testing.T) {
	caps := &providers.ReasoningCapabilities{ThinkingModes: []string{"adaptive"}}
	result := applyReasoningSelector([]byte(`{"model":"x"}`), reasoningSelector{Present: true, Enabled: boolPtr(true), Effort: "high"}, providers.ProtocolMessages, caps)
	if !strings.Contains(string(result), `"type":"adaptive"`) || !strings.Contains(string(result), `"effort":"high"`) {
		t.Fatalf("adaptive-only enabled request lost selector fields: %s", result)
	}
	if strings.Contains(string(result), "budget_tokens") {
		t.Fatalf("adaptive thinking carried an invalid budget: %s", result)
	}
}

func TestReasoningRequestFixes_LegacyToggleOnlyEnablesMessagesWithBudget(t *testing.T) {
	caps := &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionToggle}}}
	result := applyReasoningSelector([]byte(`{"model":"x"}`), reasoningSelector{Present: true, Enabled: boolPtr(true)}, providers.ProtocolMessages, caps)
	if !strings.Contains(string(result), `"type":"enabled"`) || !strings.Contains(string(result), `"budget_tokens":1024`) {
		t.Fatalf("toggle-only Messages target did not get a valid enabled selector: %s", result)
	}
}

func TestReasoningRequestFixes_EffortOnlyKnownMessagesPreservesEffort(t *testing.T) {
	caps := &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low", "high"}}}}
	result := applyReasoningSelector([]byte(`{"model":"x"}`), reasoningSelector{Present: true, Effort: "high"}, providers.ProtocolMessages, caps)
	if !strings.Contains(string(result), `"effort":"high"`) {
		t.Fatalf("supported effort was dropped: %s", result)
	}
	if strings.Contains(string(result), `"type"`) || strings.Contains(string(result), "budget_tokens") {
		t.Fatalf("effort-only target received unsupported thinking controls: %s", result)
	}
}

func TestReasoningRequestFixes_NativeSignedRequestIsBytePreserving(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high","signature":"opaque-signed-state"}`)
	result, err := translateRequest(body, providers.ProtocolChat, providers.ProtocolChat, "ignored")
	if err != nil {
		t.Fatalf("native translation: %v", err)
	}
	if string(result) != string(body) {
		t.Fatalf("native signed request changed: got %s, want %s", result, body)
	}
}

func TestReasoningRequestFixes_NativeBodyIsBytePreserving(t *testing.T) {
	body := []byte(`{ "model": "x", "reasoning_effort": "high" }`)
	result, err := translateRequest(body, providers.ProtocolChat, providers.ProtocolChat, "ignored")
	if err != nil || string(result) != string(body) {
		t.Fatalf("native request changed: %s, err=%v", result, err)
	}
}

// TestPlainChatDefaultDisableInjectsNoneWhenSupported verifies B2: a target
// advertising effort "none" gets an explicit reasoning_effort:none for a
// plain-chat request body.
func TestPlainChatDefaultDisableInjectsNoneWhenSupported(t *testing.T) {
	caps := &providers.ReasoningCapabilities{
		Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"none", "low", "high"}}},
	}
	out, ok := injectChatDisable([]byte(`{"model":"m","messages":[]}`), caps)
	if !ok {
		t.Fatal("expected disable injection for none-capable target")
	}
	if !strings.Contains(string(out), `"reasoning_effort":"none"`) {
		t.Fatalf("disable not injected: %s", out)
	}
}

// TestPlainChatDefaultDisableFallsBackToToggle verifies B2 toggle-only
// targets get reasoning.enabled:false instead of effort:none.
func TestPlainChatDefaultDisableFallsBackToToggle(t *testing.T) {
	caps := &providers.ReasoningCapabilities{
		Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionToggle}},
	}
	out, ok := injectChatDisable([]byte(`{"model":"m"}`), caps)
	if !ok {
		t.Fatal("expected disable injection for toggle-only target")
	}
	if !strings.Contains(string(out), `"enabled":false`) || strings.Contains(string(out), "reasoning_effort") {
		t.Fatalf("toggle disable malformed: %s", out)
	}
}

func TestPlainChatDefaultDisableLeavesUnknownAndMandatoryAlone(t *testing.T) {
	body := []byte(`{"model":"m"}`)
	// Unknown capabilities: preserve provider default, never invent a selector.
	if out, ok := injectChatDisable(body, nil); ok || string(out) != string(body) {
		t.Fatalf("unknown caps must pass through unchanged: %s ok=%v", out, ok)
	}
	// Mandatory reasoning: the caller skips the target instead of disabling.
	mandatory := true
	caps := &providers.ReasoningCapabilities{
		Mandatory: &mandatory,
		Options:   []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"none", "high"}}},
	}
	if !isMandatoryReasoning(caps) {
		t.Fatal("mandatory target not detected")
	}
	if out, ok := injectChatDisable(body, caps); ok || string(out) != string(body) {
		t.Fatalf("mandatory caps must not be disabled: %s ok=%v", out, ok)
	}
	// Known caps with no disable mechanism: provider default preserved.
	empty := &providers.ReasoningCapabilities{
		Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low", "high"}}},
	}
	if out, ok := injectChatDisable(body, empty); ok || string(out) != string(body) {
		t.Fatalf("non-disablable caps must pass through unchanged: %s ok=%v", out, ok)
	}
	// Invalid JSON: safe no-op.
	bad := []byte(`{invalid`)
	if out, ok := injectChatDisable(bad, &providers.ReasoningCapabilities{
		Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionToggle}},
	}); ok || string(out) != string(bad) {
		t.Fatalf("invalid body must pass through unchanged: %s ok=%v", out, ok)
	}
}
