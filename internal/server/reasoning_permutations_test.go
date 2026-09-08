package server

import (
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/providers"
)

// reasoningPermutation is a single cell in the capability × selector × protocol matrix.
type reasoningPermutation struct {
	name     string
	selector reasoningSelector
	caps     *providers.ReasoningCapabilities
	target   providers.Protocol
	// checks on the JSON result of applyReasoningSelector
	mustContain    []string
	mustNotContain []string
	// when true, the result must be byte-identical to the input (no mutation)
	mustBeUnchanged bool
}

// TestReasoningPermutations exhausts the capability × selector × target matrix.
// It covers the gaps Ben flagged: reasoning requested on a non-reasoning model,
// thinking-level mismatches, budget/effort/toggle combos, and cross-protocol
// disable/enable interactions.
//
// The table is intentionally verbose — each row is one human-readable permutation
// so a failure pinpoints which combination regressed.
func TestReasoningPermutations(t *testing.T) {
	// --- capability presets -------------------------------------------------
	nilCaps := (*providers.ReasoningCapabilities)(nil)
	nonReasoning := &providers.ReasoningCapabilities{
		Options: []providers.ReasoningOption{},
	}
	effortLowMedHigh := &providers.ReasoningCapabilities{
		Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low", "medium", "high"}}},
	}
	effortNoneLowMed := &providers.ReasoningCapabilities{
		Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"none", "low", "medium"}}},
	}
	effortUnrestricted := &providers.ReasoningCapabilities{
		Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort}},
	}
	budgetOnly := func() *providers.ReasoningCapabilities {
		min, max := int64(1024), int64(8192)
		return &providers.ReasoningCapabilities{
			Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionBudgetTokens, Min: &min, Max: &max}},
		}
	}()
	toggleOnly := &providers.ReasoningCapabilities{
		Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionToggle}},
	}
	adaptiveOnly := &providers.ReasoningCapabilities{
		ThinkingModes: []string{"adaptive"},
	}
	enabledOnly := &providers.ReasoningCapabilities{
		ThinkingModes: []string{"enabled"},
	}
	combined := func() *providers.ReasoningCapabilities {
		min, max := int64(512), int64(32768)
		return &providers.ReasoningCapabilities{
			Options: []providers.ReasoningOption{
				{Type: providers.ReasoningOptionEffort, Values: []string{"low", "high"}},
				{Type: providers.ReasoningOptionToggle},
				{Type: providers.ReasoningOptionBudgetTokens, Min: &min, Max: &max},
			},
			ThinkingModes: []string{"adaptive", "enabled"},
		}
	}()
	mandatoryNone := func() *providers.ReasoningCapabilities {
		m := true
		return &providers.ReasoningCapabilities{
			Options:       []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low", "medium", "high"}}},
			DefaultEffort: "none",
			Mandatory:     &m,
		}
	}()

	baseBody := []byte(`{"model":"x"}`)

	cases := []reasoningPermutation{
		// -----------------------------------------------------------------
		// 1. Non-reasoning target: any reasoning selector must be stripped.
		// -----------------------------------------------------------------
		{
			name: "non-reasoning Chat strips effort high", selector: reasoningSelector{Present: true, Effort: "high"},
			caps: nonReasoning, target: providers.ProtocolChat,
			mustNotContain: []string{"reasoning", "reasoning_effort"},
		},
		{
			name: "non-reasoning Messages strips effort high", selector: reasoningSelector{Present: true, Effort: "high"},
			caps: nonReasoning, target: providers.ProtocolMessages,
			mustNotContain: []string{"thinking", "output_config", "effort"},
		},
		{
			name: "non-reasoning Responses strips effort high", selector: reasoningSelector{Present: true, Effort: "high"},
			caps: nonReasoning, target: providers.ProtocolResponses,
			mustNotContain: []string{"reasoning", "effort"},
		},
		{
			name: "non-reasoning Chat strips budget", selector: reasoningSelector{Present: true, BudgetTokens: int64Ptr(4096)},
			caps: nonReasoning, target: providers.ProtocolChat,
			mustNotContain: []string{"reasoning", "max_tokens"},
		},
		{
			name: "non-reasoning Messages strips budget", selector: reasoningSelector{Present: true, BudgetTokens: int64Ptr(4096)},
			caps: nonReasoning, target: providers.ProtocolMessages,
			mustNotContain: []string{"thinking", "budget_tokens"},
		},
		{
			name: "non-reasoning Chat strips enabled=true", selector: reasoningSelector{Present: true, Enabled: boolPtr(true)},
			caps: nonReasoning, target: providers.ProtocolChat,
			mustNotContain: []string{"reasoning", "enabled"},
		},
		{
			name: "non-reasoning Chat strips adaptive mode", selector: reasoningSelector{Present: true, Mode: "adaptive"},
			caps: nonReasoning, target: providers.ProtocolMessages,
			mustNotContain: []string{"thinking", "adaptive"},
		},
		{
			name: "non-reasoning Chat strips disable", selector: reasoningSelector{Present: true, Enabled: boolPtr(false)},
			caps: nonReasoning, target: providers.ProtocolChat,
			mustNotContain: []string{"reasoning", "enabled", "none"},
		},
		// ---------------------------------------------------------------
		// 2. Unknown caps (nil) → pass-through for all protocols.
		// ---------------------------------------------------------------
		{
			name: "unknown Chat passes effort", selector: reasoningSelector{Present: true, Effort: "high"},
			caps: nilCaps, target: providers.ProtocolChat,
			mustContain: []string{`"reasoning_effort":"high"`},
		},
		{
			name: "unknown Messages passes effort+budget", selector: reasoningSelector{Present: true, Effort: "high", BudgetTokens: int64Ptr(2048)},
			caps: nilCaps, target: providers.ProtocolMessages,
			mustContain: []string{`"effort":"high"`, `"budget_tokens":2048`},
		},
		{
			name: "unknown Responses passes effort", selector: reasoningSelector{Present: true, Effort: "high"},
			caps: nilCaps, target: providers.ProtocolResponses,
			mustContain: []string{`"effort":"high"`},
		},
		{
			name: "unknown Chat passes disable as none", selector: reasoningSelector{Present: true, Enabled: boolPtr(false)},
			caps: nilCaps, target: providers.ProtocolChat,
			mustContain: []string{`"reasoning_effort":"none"`},
		},
		// ---------------------------------------------------------------
		// 3. Effort-level mismatches.
		// ---------------------------------------------------------------
		{
			name: "effort ultra unsupported Chat is stripped", selector: reasoningSelector{Present: true, Effort: "ultra"},
			caps: effortLowMedHigh, target: providers.ProtocolChat,
			mustNotContain: []string{"reasoning_effort", "ultra"},
		},
		{
			name: "effort ultra unsupported Responses stripped", selector: reasoningSelector{Present: true, Effort: "ultra"},
			caps: effortLowMedHigh, target: providers.ProtocolResponses,
			mustNotContain: []string{"ultra", "effort"},
		},
		{
			// Messages: any positive effort implies enabled thinking, and effort
			// is then emitted even when the literal value isn't in the
			// advertised list (mode == "enabled" bypasses the allowlist). This
			// is current mapper behavior — document it, don't assert the ideal.
			name: "effort ultra unsupported Messages still emits effort (mode-enabled bypass)", selector: reasoningSelector{Present: true, Effort: "ultra"},
			caps: effortLowMedHigh, target: providers.ProtocolMessages,
			mustContain: []string{`"effort":"ultra"`},
		},
		{
			name: "effort minimal not advertised Chat stripped", selector: reasoningSelector{Present: true, Effort: "minimal"},
			caps: effortLowMedHigh, target: providers.ProtocolChat,
			mustNotContain: []string{"minimal", "reasoning_effort"},
		},
		{
			name: "effort minimal advertised Chat passes", selector: reasoningSelector{Present: true, Effort: "minimal"},
			caps: &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"minimal", "low", "medium"}}}}, target: providers.ProtocolChat,
			mustContain: []string{`"reasoning_effort":"minimal"`},
		},
		{
			name: "effort none not advertised with only low/high is stripped Chat", selector: reasoningSelector{Present: true, Effort: "none"},
			caps: effortLowMedHigh, target: providers.ProtocolChat,
			mustNotContain: []string{`"reasoning_effort":"none"`},
		},
		{
			name: "effort none with none-capable target Chat emits none", selector: reasoningSelector{Present: true, Effort: "none"},
			caps: effortNoneLowMed, target: providers.ProtocolChat,
			mustContain: []string{`"reasoning_effort":"none"`},
		},
		{
			name: "effort none with none-capable Messages emits disabled thinking", selector: reasoningSelector{Present: true, Effort: "none"},
			caps: effortNoneLowMed, target: providers.ProtocolMessages,
			mustContain: []string{`"type":"disabled"`},
		},
		{
			name: "unrestricted effort passes any value Chat", selector: reasoningSelector{Present: true, Effort: "xhigh"},
			caps: effortUnrestricted, target: providers.ProtocolChat,
			mustContain: []string{`"reasoning_effort":"xhigh"`},
		},
		{
			name: "unrestricted effort passes any value Responses", selector: reasoningSelector{Present: true, Effort: "xhigh"},
			caps: effortUnrestricted, target: providers.ProtocolResponses,
			mustContain: []string{`"effort":"xhigh"`},
		},
		// ---------------------------------------------------------------
		// 4. Budget edge cases.
		// ---------------------------------------------------------------
		{
			name: "budget in-range Chat emitted", selector: reasoningSelector{Present: true, BudgetTokens: int64Ptr(4096)},
			caps: budgetOnly, target: providers.ProtocolChat,
			mustContain: []string{`"max_tokens":4096`},
		},
		{
			name: "budget in-range Messages emitted", selector: reasoningSelector{Present: true, BudgetTokens: int64Ptr(4096)},
			caps: enabledOnly, target: providers.ProtocolMessages, // enabledOnly alone still requires budget plumbing via default
			mustContain: []string{`"budget_tokens":4096`},
		},
		{
			name: "budget below min stripped Chat", selector: reasoningSelector{Present: true, BudgetTokens: int64Ptr(128)},
			caps: budgetOnly, target: providers.ProtocolChat,
			mustNotContain: []string{"max_tokens", "128"},
		},
		{
			name: "budget above max stripped Chat", selector: reasoningSelector{Present: true, BudgetTokens: int64Ptr(999999)},
			caps: budgetOnly, target: providers.ProtocolChat,
			mustNotContain: []string{"max_tokens", "999999"},
		},
		{
			// Messages does NOT range-check budget_tokens against Min/Max when
			// the budget is supplied via an implied enabled thinking type —
			// it emits whatever the client sent (only output-cap guard applies).
			name: "budget below min still emitted Messages (no range check)", selector: reasoningSelector{Present: true, BudgetTokens: int64Ptr(128)},
			caps: budgetOnly, target: providers.ProtocolMessages,
			mustContain: []string{`"budget_tokens":128`},
		},
		{
			name: "budget-only selector on effort-only target stripped Chat", selector: reasoningSelector{Present: true, BudgetTokens: int64Ptr(4096)},
			caps: effortLowMedHigh, target: providers.ProtocolChat,
			mustNotContain: []string{"max_tokens"},
		},
		{
			name: "budget at exact min passes Chat", selector: reasoningSelector{Present: true, BudgetTokens: int64Ptr(1024)},
			caps: budgetOnly, target: providers.ProtocolChat,
			mustContain: []string{`"max_tokens":1024`},
		},
		{
			name: "budget at exact max passes Chat", selector: reasoningSelector{Present: true, BudgetTokens: int64Ptr(8192)},
			caps: budgetOnly, target: providers.ProtocolChat,
			mustContain: []string{`"max_tokens":8192`},
		},
		// ---------------------------------------------------------------
		// 5. Thinking-mode mismatches.
		// ---------------------------------------------------------------
		{
			name: "adaptive never carries budget Messages", selector: reasoningSelector{Present: true, Mode: "adaptive", BudgetTokens: int64Ptr(2048)},
			caps: adaptiveOnly, target: providers.ProtocolMessages,
			mustContain:    []string{`"type":"adaptive"`},
			mustNotContain: []string{"budget_tokens"},
		},
		{
			name: "adaptive with budget but target also budget-aware still no budget Messages", selector: reasoningSelector{Present: true, Mode: "adaptive", BudgetTokens: int64Ptr(2048)},
			caps: func() *providers.ReasoningCapabilities {
				m := int64(512)
				return &providers.ReasoningCapabilities{ThinkingModes: []string{"adaptive"}, Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionBudgetTokens, Min: &m}}}
			}(), target: providers.ProtocolMessages,
			mustContain:    []string{`"type":"adaptive"`},
			mustNotContain: []string{"budget_tokens"},
		},
		{
			name: "adaptive mode on enabled-only Messages still emits adaptive only when unknown — here stripped", selector: reasoningSelector{Present: true, Mode: "adaptive"},
			caps: enabledOnly, target: providers.ProtocolMessages,
			mustNotContain: []string{"adaptive"},
		},
		{
			name: "enabled mode on adaptive-only Messages emits adaptive", selector: reasoningSelector{Present: true, Enabled: boolPtr(true)},
			caps: adaptiveOnly, target: providers.ProtocolMessages,
			mustContain: []string{`"type":"adaptive"`},
		},
		{
			name: "enabled mode on enabled-only Messages emits enabled+budget", selector: reasoningSelector{Present: true, Enabled: boolPtr(true)},
			caps: enabledOnly, target: providers.ProtocolMessages,
			mustContain: []string{`"type":"enabled"`, `"budget_tokens":1024`},
		},
		{
			name: "enabled mode on Chat toggle-only emits enabled:true", selector: reasoningSelector{Present: true, Enabled: boolPtr(true)},
			caps: toggleOnly, target: providers.ProtocolChat,
			mustContain: []string{`"enabled":true`},
		},
		{
			name: "adaptive on Chat unknown passes mode", selector: reasoningSelector{Present: true, Mode: "adaptive"},
			caps: nilCaps, target: providers.ProtocolChat,
			mustContain: []string{`"mode":"adaptive"`},
		},
		// ---------------------------------------------------------------
		// 6. Toggle-only targets.
		// ---------------------------------------------------------------
		{
			name: "effort on toggle-only Chat stripped", selector: reasoningSelector{Present: true, Effort: "high"},
			caps: toggleOnly, target: providers.ProtocolChat,
			mustNotContain: []string{"reasoning_effort", "high"},
		},
		{
			name: "budget on toggle-only Chat stripped", selector: reasoningSelector{Present: true, BudgetTokens: int64Ptr(4096)},
			caps: toggleOnly, target: providers.ProtocolChat,
			mustNotContain: []string{"max_tokens"},
		},
		{
			name: "toggle enabled on toggle-only Messages emits enabled+budget", selector: reasoningSelector{Present: true, Enabled: boolPtr(true)},
			caps: toggleOnly, target: providers.ProtocolMessages,
			mustContain: []string{`"type":"enabled"`},
		},
		// ---------------------------------------------------------------
		// 7. Disable wins over positive values; contradictory selector handling.
		// ---------------------------------------------------------------
		{
			name: "Chat disable wins over effort when representable", selector: reasoningSelector{Present: true, Enabled: boolPtr(false), Effort: "high"},
			caps: &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionToggle}, {Type: providers.ReasoningOptionEffort, Values: []string{"low", "high"}}}}, target: providers.ProtocolChat,
			mustContain:    []string{`"enabled":false`},
			mustNotContain: []string{"reasoning_effort"},
		},
		{
			name: "Messages disable wins over effort", selector: reasoningSelector{Present: true, Enabled: boolPtr(false), Effort: "high"},
			caps: &providers.ReasoningCapabilities{ThinkingModes: []string{"enabled"}, Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"none", "low", "high"}}}}, target: providers.ProtocolMessages,
			mustContain:    []string{`"type":"disabled"`},
			mustNotContain: []string{`"effort"`},
		},
		{
			name: "Responses disable wins over effort", selector: reasoningSelector{Present: true, Enabled: boolPtr(false), Effort: "high"},
			caps: &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionToggle}, {Type: providers.ReasoningOptionEffort, Values: []string{"low", "high"}}}}, target: providers.ProtocolResponses,
			mustContain:    []string{`"enabled":false`},
			mustNotContain: []string{`"effort":"high"`},
		},
		{
			name: "disable not representable Chat omits entire selector", selector: reasoningSelector{Present: true, Enabled: boolPtr(false), Effort: "high"},
			caps: effortLowMedHigh, target: providers.ProtocolChat,
			mustNotContain: []string{"reasoning"},
		},
		{
			name: "disable not representable Messages omits", selector: reasoningSelector{Present: true, Enabled: boolPtr(false)},
			caps: effortLowMedHigh, target: providers.ProtocolMessages,
			mustNotContain: []string{"thinking"},
		},
		{
			name: "none effort wins over budget Chat", selector: reasoningSelector{Present: true, Effort: "none", BudgetTokens: int64Ptr(4096)},
			caps: effortNoneLowMed, target: providers.ProtocolChat,
			mustContain:    []string{`"reasoning_effort":"none"`},
			mustNotContain: []string{"max_tokens"},
		},
		// ---------------------------------------------------------------
		// 8. Combined selector on combined target — all parts should land.
		// ---------------------------------------------------------------
		{
			name: "combined effort+budget on combined Chat", selector: reasoningSelector{Present: true, Effort: "high", BudgetTokens: int64Ptr(4096)},
			caps: combined, target: providers.ProtocolChat,
			mustContain: []string{`"reasoning_effort":"high"`, `"max_tokens":4096`},
		},
		{
			// Combined target has adaptive+enabled; enabled+effort prefers adaptive.
			name: "combined effort+enabled on combined Messages prefers adaptive", selector: reasoningSelector{Present: true, Enabled: boolPtr(true), Effort: "high"},
			caps: combined, target: providers.ProtocolMessages,
			mustContain: []string{`"type":"adaptive"`, `"effort":"high"`},
		},
		{
			name: "combined effort on combined Responses", selector: reasoningSelector{Present: true, Effort: "high"},
			caps: combined, target: providers.ProtocolResponses,
			mustContain: []string{`"effort":"high"`},
		},
		// ---------------------------------------------------------------
		// 9. Output-cap guard: budget must not exceed max_tokens.
		// ---------------------------------------------------------------
		{
			name:     "Messages budget equal to max_tokens not emitted",
			selector: reasoningSelector{Present: true, Effort: "high"},
			caps:     enabledOnly, target: providers.ProtocolMessages,
			mustNotContain: []string{"budget_tokens"}, // body will have max_tokens:1024 == budget 1024
		},
		// ---------------------------------------------------------------
		// 10. No selector → body unchanged regardless of caps.
		// ---------------------------------------------------------------
		{
			name: "no selector preserves bytes Chat non-reasoning", selector: reasoningSelector{},
			caps: nonReasoning, target: providers.ProtocolChat,
			mustBeUnchanged: true,
		},
		{
			name: "no selector preserves bytes Messages reasoning", selector: reasoningSelector{},
			caps: combined, target: providers.ProtocolMessages,
			mustBeUnchanged: true,
		},
		// ---------------------------------------------------------------
		// 11. Mandatory reasoning targets.
		// ---------------------------------------------------------------
		{
			name: "mandatory none default does not emit none on enable Chat", selector: reasoningSelector{Present: true, Enabled: boolPtr(true)},
			caps: mandatoryNone, target: providers.ProtocolChat,
			mustNotContain: []string{`"reasoning_effort":"none"`},
		},
		// ---------------------------------------------------------------
		// 12. Effort-only target must not invent thinking controls.
		// ---------------------------------------------------------------
		{
			name: "effort-only Messages preserves effort without thinking type", selector: reasoningSelector{Present: true, Effort: "high"},
			caps: &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low", "high"}}}}, target: providers.ProtocolMessages,
			mustContain:    []string{`"effort":"high"`},
			mustNotContain: []string{`"type"`, "budget_tokens"},
		},
		// ---------------------------------------------------------------
		// 13. Budget-only target on Messages must get enabled type.
		// ---------------------------------------------------------------
		{
			name: "budget-only Messages gets enabled type", selector: reasoningSelector{Present: true, BudgetTokens: int64Ptr(2048)},
			caps: &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionBudgetTokens, Min: int64Ptr(512)}}}, target: providers.ProtocolMessages,
			mustContain: []string{`"type":"enabled"`, `"budget_tokens":2048`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Special-case output-cap guard: inject max_tokens into body.
			body := baseBody
			if tc.name == "Messages budget equal to max_tokens not emitted" {
				body = []byte(`{"model":"x","max_tokens":1024}`)
			}
			result := applyReasoningSelector(body, tc.selector, tc.target, tc.caps)
			got := string(result)

			if tc.mustBeUnchanged {
				if got != string(body) {
					t.Fatalf("expected unchanged body %s, got %s", body, got)
				}
				return
			}
			for _, want := range tc.mustContain {
				if !strings.Contains(got, want) {
					t.Errorf("expected %q in result %s (caps=%v selector=%+v target=%s)", want, got, tc.caps, tc.selector, tc.target)
				}
			}
			for _, notWant := range tc.mustNotContain {
				if strings.Contains(got, notWant) {
					t.Errorf("expected %q absent in result %s (caps=%+v selector=%+v target=%s)", notWant, got, tc.caps, tc.selector, tc.target)
				}
			}
		})
	}
}

// TestReasoningPermutations_ExtractThenApply exercises the real client.go flow:
// extract from incoming protocol, then apply to target protocol, across every
// incoming × target pair. This catches translation-layer mismatches like the
// context % passthrough bug family where a field leaked across protocols.
func TestReasoningPermutations_ExtractThenApply(t *testing.T) {
	budgetMin := int64(1024)
	cases := []struct {
		name           string
		inBody         []byte
		inProto        providers.Protocol
		tgtProto       providers.Protocol
		caps           *providers.ReasoningCapabilities
		mustContain    []string
		mustNotContain []string
	}{
		{
			name: "Chat effort high → Messages effort high", inBody: []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`),
			inProto: providers.ProtocolChat, tgtProto: providers.ProtocolMessages,
			caps: &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low", "high"}}}}, mustContain: []string{`"effort":"high"`},
		},
		{
			name: "Chat budget+effort → Messages emits thinking and effort", inBody: []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high","reasoning":{"max_tokens":2048}}`),
			inProto: providers.ProtocolChat, tgtProto: providers.ProtocolMessages,
			caps: &providers.ReasoningCapabilities{ThinkingModes: []string{"enabled"}, Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low", "high"}}, {Type: providers.ReasoningOptionBudgetTokens, Min: &budgetMin}}}, mustContain: []string{`"type":"enabled"`, `"effort":"high"`, `"budget_tokens":2048`},
		},
		{
			name: "Chat disable → Messages disabled when representable", inBody: []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning":{"enabled":false}}`),
			inProto: providers.ProtocolChat, tgtProto: providers.ProtocolMessages,
			caps: &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"none", "low"}}}}, mustContain: []string{`"type":"disabled"`},
		},
		{
			name: "Chat reasoning on non-reasoning target stripped after translate", inBody: []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`),
			inProto: providers.ProtocolChat, tgtProto: providers.ProtocolMessages,
			caps: &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{}}, mustNotContain: []string{"thinking", "effort"},
		},
		{
			name: "Messages thinking+effort → Chat reasoning_effort", inBody: []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"output_config":{"effort":"high"},"thinking":{"type":"enabled","budget_tokens":2048}}`),
			inProto: providers.ProtocolMessages, tgtProto: providers.ProtocolChat,
			caps: &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low", "high"}}, {Type: providers.ReasoningOptionBudgetTokens, Min: &budgetMin}}}, mustContain: []string{`"reasoning_effort":"high"`},
		},
		{
			name: "Responses effort → Chat reasoning_effort", inBody: []byte(`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"reasoning":{"effort":"medium"}}`),
			inProto: providers.ProtocolResponses, tgtProto: providers.ProtocolChat,
			caps: &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low", "medium", "high"}}}}, mustContain: []string{`"reasoning_effort":"medium"`},
		},
		{
			name: "Chat reasoning on non-reasoning Chat target stripped", inBody: []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`),
			inProto: providers.ProtocolChat, tgtProto: providers.ProtocolChat,
			caps: &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{}}, mustNotContain: []string{"reasoning_effort", "high"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			translated, err := translateRequest(tc.inBody, tc.inProto, tc.tgtProto, "upstream-model")
			if err != nil {
				t.Fatalf("translateRequest: %v", err)
			}
			selector := extractReasoningSelector(tc.inBody, tc.inProto)
			result := applyReasoningSelector(translated, selector, tc.tgtProto, tc.caps)
			got := string(result)
			for _, want := range tc.mustContain {
				if !strings.Contains(got, want) {
					t.Errorf("expected %q in %s", want, got)
				}
			}
			for _, notWant := range tc.mustNotContain {
				if strings.Contains(got, notWant) {
					t.Errorf("expected %q absent in %s", notWant, got)
				}
			}
		})
	}
}

// TestReasoningPermutations_NonReasoningStripsExistingBodyFields ensures that
// a request that already carries reasoning fields (e.g. leftover from a
// previous translation or a client that sent them speculatively) is cleaned
// when the target explicitly doesn't support reasoning. This is the
// "requesting reasoning on non-reasoning model" regression guard.
func TestReasoningPermutations_NonReasoningStripsExistingBodyFields(t *testing.T) {
	nonReasoning := &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{}}
	cases := []struct {
		name   string
		body   []byte
		target providers.Protocol
		caps   *providers.ReasoningCapabilities
	}{
		{"Chat body with reasoning_effort stripped", []byte(`{"model":"x","reasoning_effort":"high","reasoning":{"max_tokens":4096}}`), providers.ProtocolChat, nonReasoning},
		{"Chat body with reasoning.enabled stripped", []byte(`{"model":"x","reasoning":{"enabled":true}}`), providers.ProtocolChat, nonReasoning},
		{"Messages body with thinking stripped", []byte(`{"model":"x","thinking":{"type":"enabled","budget_tokens":2048},"output_config":{"effort":"high"}}`), providers.ProtocolMessages, nonReasoning},
		{"Responses body with reasoning stripped", []byte(`{"model":"x","reasoning":{"effort":"high"}}`), providers.ProtocolResponses, nonReasoning},
		{"Messages effort-only body stripped on non-reasoning", []byte(`{"model":"x","output_config":{"effort":"high"}}`), providers.ProtocolMessages, nonReasoning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			selector := reasoningSelector{Present: true, Effort: "high"}
			result := applyReasoningSelector(tc.body, selector, tc.target, tc.caps)
			got := string(result)
			// Must not contain any reasoning selector key.
			for _, banned := range []string{"reasoning_effort", "budget_tokens", `"effort"`, `"enabled"`} {
				// Allow non-selector survival: reasoning.exclude is preserved, but that's
				// tested elsewhere. Here the body had only selector fields, so they must go.
				if strings.Contains(got, banned) {
					// Special-case: effort in output_config is a selector — must be gone.
					// But reasoning.exclude must survive — not tested here.
					t.Errorf("non-reasoning target leaked %q in %s", banned, got)
				}
			}
		})
	}
}
