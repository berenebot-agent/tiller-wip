package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tiller-router/tiller-router/internal/providers"
)

type unsupportedFeature struct{ feature string }

func (u unsupportedFeature) Error() string {
	return fmt.Sprintf("%s is only supported through a native Responses provider", u.feature)
}

// reasoningSelector is a request-local representation of the client's
// reasoning choice. It is never serialized directly.
type reasoningSelector struct {
	Present      bool
	Enabled      *bool
	Mode         string
	Effort       string
	BudgetTokens *int64
}

// translateRequest translates a request body from one protocol to another.
// When the target protocol differs, reasoning controls are extracted before
// conversion and must be re-applied by the caller via applyReasoningSelector.
func translateRequest(body []byte, from, to providers.Protocol, model string, modelMaxOutputTokens ...int64) ([]byte, error) {
	if from == to {
		return body, nil
	}
	var source map[string]any
	if err := json.Unmarshal(body, &source); err != nil {
		return nil, err
	}
	if err := validateReasoningRepresentability(source, from, to); err != nil {
		return nil, err
	}
	if from == providers.ProtocolResponses {
		for _, key := range []string{"conversation", "previous_response_id", "store", "background", "files"} {
			if value, ok := source[key]; ok && value != nil && value != false && value != "" {
				return nil, unsupportedFeature{key}
			}
		}
		if tools, ok := source["tools"].([]any); ok {
			for _, raw := range tools {
				tool, _ := raw.(map[string]any)
				if tool["type"] != "function" {
					return nil, unsupportedFeature{"provider-hosted tools"}
				}
			}
		}
	}
	chat, err := requestToChat(source, from)
	if err != nil {
		return nil, err
	}
	chat["model"] = model
	if to == providers.ProtocolChat {
		return json.Marshal(chat)
	}
	if to == providers.ProtocolMessages {
		if len(modelMaxOutputTokens) > 0 && modelMaxOutputTokens[0] > 0 {
			chat["max_output_tokens"] = modelMaxOutputTokens[0]
		}
		messages, err := chatToMessagesRequest(chat)
		if err != nil {
			return nil, err
		}
		return json.Marshal(messages)
	}
	if to == providers.ProtocolResponses {
		translated, err := chatToResponsesRequest(chat)
		if err != nil {
			return nil, err
		}
		return json.Marshal(translated)
	}
	return nil, errors.New("unsupported protocol translation")
}

// extractReasoningSelector extracts a canonical reasoning selector from the
// incoming request body according to its protocol. Returns a zero-value
// selector with Present=false when no reasoning control is found.
func extractReasoningSelector(body []byte, protocol providers.Protocol) reasoningSelector {
	var source map[string]any
	if err := json.Unmarshal(body, &source); err != nil {
		return reasoningSelector{}
	}
	switch protocol {
	case providers.ProtocolChat:
		return extractChatReasoning(source)
	case providers.ProtocolResponses:
		return extractResponsesReasoning(source)
	case providers.ProtocolMessages:
		return extractMessagesReasoning(source)
	}
	return reasoningSelector{}
}

// extractChatReasoning extracts reasoning controls from Chat Completions.
// Determinism: top-level reasoning_effort takes precedence over nested
// reasoning.effort; nested reasoning.max_tokens maps to budget.
func extractChatReasoning(source map[string]any) reasoningSelector {
	sel := reasoningSelector{Present: false}
	// Top-level reasoning_effort takes precedence.
	if effort, ok := source["reasoning_effort"].(string); ok && effort != "" {
		sel.Present = true
		sel.Effort = effort
	}
	// Nested reasoning object: effort, max_tokens, and enable/disable.
	if reasoning, ok := source["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok && effort != "" {
			sel.Present = true
			if sel.Effort == "" {
				sel.Effort = effort
			}
		}
		if maxTokens, ok := coerceInt64(reasoning["max_tokens"]); ok {
			sel.Present = true
			sel.BudgetTokens = &maxTokens
		}
		// Enable/disable controls.
		if enabled, ok := reasoning["enabled"].(bool); ok {
			sel.Present = true
			sel.Enabled = &enabled
		}
	}
	return sel
}

// extractResponsesReasoning extracts reasoning controls from Responses.
// Only reasoning.effort is extracted; reasoning.summary (display) is ignored.
func extractResponsesReasoning(source map[string]any) reasoningSelector {
	if reasoning, ok := source["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok && effort != "" {
			return reasoningSelector{Present: true, Effort: effort}
		}
	}
	return reasoningSelector{}
}

// extractMessagesReasoning extracts reasoning controls from Messages.
// output_config.effort, thinking.type, and thinking.budget_tokens are
// extracted; thinking.display is ignored.
func extractMessagesReasoning(source map[string]any) reasoningSelector {
	sel := reasoningSelector{Present: false}
	if outputConfig, ok := source["output_config"].(map[string]any); ok {
		if effort, ok := outputConfig["effort"].(string); ok && effort != "" {
			sel.Present = true
			sel.Effort = effort
		}
	}
	if thinking, ok := source["thinking"].(map[string]any); ok {
		if t, ok := thinking["type"].(string); ok && t != "" {
			sel.Present = true
			sel.Mode = t
		}
		if budget, ok := coerceInt64(thinking["budget_tokens"]); ok {
			sel.Present = true
			sel.BudgetTokens = &budget
		}
	}
	return sel
}

// coerceInt64 converts a JSON number to int64. Only float64 (how
// encoding/json decodes numbers) and int64 are accepted.
func coerceInt64(v any) (int64, bool) {
	return providers.CoerceInt64(v)
}

// applyReasoningSelector maps a canonical selector onto a target request body
// according to the target protocol and the target model's reasoning
// capabilities. Unsupported selector parts are silently omitted.
//
// Mapping rules:
//   - Exact effort values map directly when the target advertises them.
//   - "none" maps to Messages disabled when supported.
//   - "minimal" maps only when explicitly advertised.
//   - Numeric budgets map when a numeric target control exists.
//   - If the target is known not to support reasoning, the selector is omitted.
//   - If support is unknown, the selector is passed through.
func applyReasoningSelector(body []byte, selector reasoningSelector, target providers.Protocol, caps *providers.ReasoningCapabilities) []byte {
	if !selector.Present {
		return body
	}
	var source map[string]any
	if err := json.Unmarshal(body, &source); err != nil {
		return body
	}
	opts := providers.ExtractReasoningOptions(caps)
	unknownSupport := caps == nil
	changed := false
	if !unknownSupport {
		changed = stripReasoningSelectorMap(source, target)
	}

	// If target explicitly doesn't support reasoning, omit the selector.
	if !unknownSupport && !opts.SupportsEffort && !opts.SupportsBudget && !opts.SupportsToggle && !opts.SupportsAdaptive && !opts.SupportsEnabled {
		if !changed {
			return body
		}
		result, _ := json.Marshal(source)
		return result
	}

	mode := selector.Mode
	if selector.Enabled != nil {
		if *selector.Enabled {
			mode = "enabled"
		} else {
			mode = "disabled"
		}
	}
	switch target {
	case providers.ProtocolChat:
		changed = applyChatReasoning(source, selector, mode, opts, caps, unknownSupport) || changed
	case providers.ProtocolResponses:
		changed = applyResponsesReasoning(source, selector, mode, opts, unknownSupport) || changed
	case providers.ProtocolMessages:
		changed = applyMessagesReasoning(source, selector, mode, opts, unknownSupport) || changed
	}
	if !changed {
		return body
	}
	result, _ := json.Marshal(source)
	return result
}

// stripReasoningSelector removes recognized reasoning selector fields from a
// request body so that a target known not to support reasoning receives a
// clean request. Non-selector fields (e.g. OpenRouter's reasoning.exclude,
// which controls whether reasoning is returned) are preserved.
func stripReasoningSelector(body []byte, target providers.Protocol) []byte {
	var source map[string]any
	if err := json.Unmarshal(body, &source); err != nil {
		return body
	}
	if !stripReasoningSelectorMap(source, target) {
		return body
	}
	result, _ := json.Marshal(source)
	return result
}

// stripReasoningSelectorMap removes recognized reasoning selector fields from
// a parsed request body in place, returning whether anything changed.
// Non-selector fields (e.g. OpenRouter's reasoning.exclude) are preserved.
func stripReasoningSelectorMap(source map[string]any, target providers.Protocol) bool {
	changed := false
	switch target {
	case providers.ProtocolChat:
		if _, ok := source["reasoning_effort"]; ok {
			delete(source, "reasoning_effort")
			changed = true
		}
		// Preserve non-selector fields like reasoning.exclude.
		if reasoning, ok := source["reasoning"].(map[string]any); ok {
			if _, ok := reasoning["effort"]; ok {
				delete(reasoning, "effort")
				changed = true
			}
			if _, ok := reasoning["max_tokens"]; ok {
				delete(reasoning, "max_tokens")
				changed = true
			}
			if _, ok := reasoning["enabled"]; ok {
				delete(reasoning, "enabled")
				changed = true
			}
			if _, ok := reasoning["mode"]; ok {
				delete(reasoning, "mode")
				changed = true
			}
			if len(reasoning) == 0 {
				delete(source, "reasoning")
				changed = true
			}
		}
	case providers.ProtocolResponses:
		if reasoning, ok := source["reasoning"].(map[string]any); ok {
			if _, ok := reasoning["effort"]; ok {
				delete(reasoning, "effort")
				changed = true
			}
			if _, ok := reasoning["enabled"]; ok {
				delete(reasoning, "enabled")
				changed = true
			}
			if _, ok := reasoning["mode"]; ok {
				delete(reasoning, "mode")
				changed = true
			}
			if len(reasoning) == 0 {
				delete(source, "reasoning")
				changed = true
			}
		}
	case providers.ProtocolMessages:
		if outputConfig, ok := source["output_config"].(map[string]any); ok {
			if _, ok := outputConfig["effort"]; ok {
				delete(outputConfig, "effort")
				changed = true
			}
			if len(outputConfig) == 0 {
				delete(source, "output_config")
				changed = true
			}
		}
		if thinking, ok := source["thinking"].(map[string]any); ok {
			if _, ok := thinking["type"]; ok {
				delete(thinking, "type")
				changed = true
			}
			if _, ok := thinking["budget_tokens"]; ok {
				delete(thinking, "budget_tokens")
				changed = true
			}
			if len(thinking) == 0 {
				delete(source, "thinking")
				changed = true
			}
		}
	}
	return changed
}

// applyChatReasoning maps a selector onto a Chat Completions request.
// Mode (enabled/disabled) is applied when representable. An explicit disable
// wins over all other selector parts; when it cannot be represented, the
// complete contradictory selector is omitted and the provider's default is
// used.
func applyChatReasoning(source map[string]any, selector reasoningSelector, mode string, opts providers.ReasoningOptions, caps *providers.ReasoningCapabilities, unknownSupport bool) bool {
	disabled := mode == "disabled" || selector.Effort == "none"
	if disabled {
		// Chat's standard disable selector is reasoning_effort:none. When
		// capabilities are unknown, use the portable effort:none representation
		// rather than the non-standard nested enabled field.
		if effortIsSupported("none", opts) || unknownSupport {
			setChatEffort(source, "none")
			return true
		}
		if opts.SupportsToggle {
			setChatEnabled(source, false)
			return true
		}
		return false
	}
	changed := false
	if mode == "enabled" {
		if opts.SupportsToggle {
			setChatEnabled(source, true)
			changed = true
		} else if caps != nil && caps.DefaultEffort != "" && !(caps.Mandatory != nil && *caps.Mandatory && caps.DefaultEffort == "none") && effortIsSupported(caps.DefaultEffort, opts) {
			setChatEffort(source, caps.DefaultEffort)
			changed = true
		}
	} else if mode == "adaptive" && unknownSupport {
		setChatMode(source, "adaptive")
		changed = true
	}
	if selector.Effort != "" && selector.Effort != "none" && (effortIsSupported(selector.Effort, opts) || unknownSupport) {
		setChatEffort(source, selector.Effort)
		changed = true
	}
	if selector.BudgetTokens != nil && (budgetIsSupported(*selector.BudgetTokens, opts) || unknownSupport) {
		setChatBudget(source, *selector.BudgetTokens)
		changed = true
	}
	return changed
}

// applyResponsesReasoning maps a selector onto a Responses request.
func applyResponsesReasoning(source map[string]any, selector reasoningSelector, mode string, opts providers.ReasoningOptions, unknownSupport bool) bool {
	disabled := mode == "disabled" || selector.Effort == "none"
	if disabled {
		// Responses has no standard reasoning.enabled selector. When
		// capabilities are unknown, use the portable effort:none representation.
		if effortIsSupported("none", opts) || unknownSupport {
			setResponsesEffort(source, "none")
			return true
		}
		if opts.SupportsToggle {
			setResponsesEnabled(source, false)
			return true
		}
		return false
	}
	changed := false
	if mode == "enabled" && opts.SupportsToggle {
		setResponsesEnabled(source, true)
		changed = true
	} else if mode == "adaptive" && unknownSupport {
		setResponsesMode(source, "adaptive")
		changed = true
	}
	if selector.Effort != "" && selector.Effort != "none" && (effortIsSupported(selector.Effort, opts) || unknownSupport) {
		setResponsesEffort(source, selector.Effort)
		changed = true
	}
	// Responses has no verified numeric reasoning-budget field. Do not invent
	// reasoning.max_tokens, even when the source protocol had a token budget.
	return changed
}

// applyMessagesReasoning maps a selector onto a Messages request.
// Mode (enabled/disabled/adaptive) is applied when representable. An explicit
// disable wins over all other selector parts; if it is not representable, the
// complete contradictory selector is omitted.
func applyMessagesReasoning(source map[string]any, selector reasoningSelector, mode string, opts providers.ReasoningOptions, unknownSupport bool) bool {
	disabled := mode == "disabled" || selector.Effort == "none"
	if disabled && (opts.SupportsDisable || unknownSupport) {
		setMessagesThinkingType(source, "disabled")
		return true
	}
	if disabled {
		return false
	}
	// A positive effort, budget, or explicit enabled mode implies enabled thinking.
	positiveEffort := selector.Effort != "" && selector.Effort != "none"
	if mode == "" && (positiveEffort || selector.BudgetTokens != nil) {
		mode = "enabled"
	}
	changed := false
	switch mode {
	case "adaptive":
		if opts.SupportsAdaptive || unknownSupport {
			setMessagesThinkingType(source, "adaptive")
			changed = true
		}
	case "enabled":
		if opts.SupportsAdaptive {
			setMessagesThinkingType(source, "adaptive")
			changed = true
		} else if opts.SupportsEnabled || opts.SupportsToggle || opts.SupportsBudget || unknownSupport {
			budget := messagesDefaultBudget(opts)
			if selector.BudgetTokens != nil {
				budget = *selector.BudgetTokens
			}
			// Don't emit a budget that would exceed the caller's explicit output cap.
			if !messagesBudgetFitsOutput(source, budget) {
				return changed
			}
			setMessagesThinkingType(source, "enabled")
			changed = true
			if selector.BudgetTokens != nil {
				setMessagesBudget(source, *selector.BudgetTokens)
			} else {
				setMessagesBudget(source, budget)
			}
		}
	}
	if selector.Effort != "" && selector.Effort != "none" {
		if effortIsSupported(selector.Effort, opts) || unknownSupport || mode == "enabled" {
			setMessagesEffort(source, selector.Effort)
			changed = true
		}
	}
	return changed
}

// matchesEffort returns true when the target explicitly advertises the given
// effort value. "minimal" only matches when explicitly advertised.
func matchesEffort(effort string, supported []string) bool {
	for _, s := range supported {
		if s == effort {
			return true
		}
	}
	return false
}

// effortIsSupported distinguishes an explicitly unrestricted effort option
// (empty values) from a target that has no effort mechanism at all.
func effortIsSupported(effort string, opts providers.ReasoningOptions) bool {
	return opts.SupportsEffort && (len(opts.SupportedEfforts) == 0 || matchesEffort(effort, opts.SupportedEfforts))
}

func budgetIsSupported(budget int64, opts providers.ReasoningOptions) bool {
	if !opts.SupportsBudget {
		return false
	}
	if opts.BudgetMin != nil && budget < *opts.BudgetMin {
		return false
	}
	if opts.BudgetMax != nil && budget > *opts.BudgetMax {
		return false
	}
	return true
}

// setChatEffort sets reasoning_effort on a Chat Completions body.
func setChatEffort(source map[string]any, effort string) {
	source["reasoning_effort"] = effort
}

func setChatEnabled(source map[string]any, enabled bool) {
	reasoning, _ := source["reasoning"].(map[string]any)
	if reasoning == nil {
		reasoning = map[string]any{}
	}
	reasoning["enabled"] = enabled
	source["reasoning"] = reasoning
}

func setChatMode(source map[string]any, mode string) {
	reasoning, _ := source["reasoning"].(map[string]any)
	if reasoning == nil {
		reasoning = map[string]any{}
	}
	reasoning["mode"] = mode
	source["reasoning"] = reasoning
}

// setChatBudget sets reasoning.max_tokens on a Chat Completions body.
const maxJavaScriptSafeInteger = int64(1<<53 - 1)

func setChatBudget(source map[string]any, budget int64) {
	reasoning, _ := source["reasoning"].(map[string]any)
	if reasoning == nil {
		reasoning = map[string]any{}
	}
	if budget > maxJavaScriptSafeInteger {
		reasoning["max_tokens"] = fmt.Sprint(budget)
	} else {
		reasoning["max_tokens"] = budget
	}
	source["reasoning"] = reasoning
}

// setResponsesEffort sets reasoning.effort on a Responses body.
func setResponsesEffort(source map[string]any, effort string) {
	reasoning, _ := source["reasoning"].(map[string]any)
	if reasoning == nil {
		reasoning = map[string]any{}
	}
	reasoning["effort"] = effort
	source["reasoning"] = reasoning
}

func setResponsesEnabled(source map[string]any, enabled bool) {
	reasoning, _ := source["reasoning"].(map[string]any)
	if reasoning == nil {
		reasoning = map[string]any{}
	}
	reasoning["enabled"] = enabled
	source["reasoning"] = reasoning
}

func setResponsesMode(source map[string]any, mode string) {
	reasoning, _ := source["reasoning"].(map[string]any)
	if reasoning == nil {
		reasoning = map[string]any{}
	}
	reasoning["mode"] = mode
	source["reasoning"] = reasoning
}

// setMessagesEffort sets output_config.effort on a Messages body.
func setMessagesEffort(source map[string]any, effort string) {
	outputConfig, _ := source["output_config"].(map[string]any)
	if outputConfig == nil {
		outputConfig = map[string]any{}
	}
	outputConfig["effort"] = effort
	source["output_config"] = outputConfig
}

// setMessagesDisabled sets thinking.type = disabled on a Messages body.
func setMessagesDisabled(source map[string]any) {
	setMessagesThinkingType(source, "disabled")
}

func setMessagesThinkingType(source map[string]any, thinkingType string) {
	thinking, _ := source["thinking"].(map[string]any)
	if thinking == nil {
		thinking = map[string]any{}
	}
	thinking["type"] = thinkingType
	source["thinking"] = thinking
}

// setMessagesBudget sets thinking.budget_tokens on a Messages body.
func setMessagesBudget(source map[string]any, budget int64) {
	thinking, _ := source["thinking"].(map[string]any)
	if thinking == nil {
		thinking = map[string]any{}
	}
	thinking["budget_tokens"] = budget
	source["thinking"] = thinking
}

// messagesDefaultBudget returns a safe budget_tokens value for a target that
// only supports thinking.type: "enabled" (where budget_tokens is required).
// It prefers the target's advertised minimum; if unknown, it falls back to
// Anthropic's documented floor of 1024.
func messagesDefaultBudget(opts providers.ReasoningOptions) int64 {
	if opts.BudgetMin != nil && *opts.BudgetMin > 1024 {
		return *opts.BudgetMin
	}
	return 1024
}

// messagesBudgetFitsOutput prevents a required thinking budget from exceeding
// the caller's explicit output cap. The cap is never raised to accommodate it.
func messagesBudgetFitsOutput(source map[string]any, budget int64) bool {
	if cap, ok := coerceInt64(source["max_tokens"]); ok {
		return cap > 0 && budget < cap
	}
	if cap, ok := coerceInt64(source["max_output_tokens"]); ok {
		return cap > 0 && budget < cap
	}
	return true
}

func requestToChat(source map[string]any, from providers.Protocol) (map[string]any, error) {
	if from == providers.ProtocolChat {
		return source, nil
	}
	chat := map[string]any{}
	for _, key := range []string{"temperature", "top_p", "stream", "stop", "metadata"} {
		if value, ok := source[key]; ok {
			chat[key] = value
		}
	}
	if max, ok := source["max_tokens"]; ok {
		chat["max_tokens"] = max
	}
	if max, ok := source["max_output_tokens"]; ok {
		chat["max_completion_tokens"] = max
	}
	var messages []any
	if from == providers.ProtocolMessages {
		if system, ok := source["system"]; ok {
			messages = append(messages, map[string]any{"role": "system", "content": system})
		}
		for _, raw := range asSlice(source["messages"]) {
			message, _ := raw.(map[string]any)
			role, _ := message["role"].(string)
			content := message["content"]
			blocks := asSlice(content)
			if blocks == nil {
				messages = append(messages, map[string]any{"role": role, "content": content})
				continue
			}
			textBlocks := []any{}
			toolCalls := []any{}
			reasoningDetails := []any{}
			var reasoningText string
			for index, item := range blocks {
				block, _ := item.(map[string]any)
				switch block["type"] {
				case "text":
					textBlocks = append(textBlocks, map[string]any{"type": "text", "text": block["text"]})
				case "image":
					textBlocks = append(textBlocks, anthropicImageToOpenAI(block))
				case "tool_use":
					toolCalls = append(toolCalls, map[string]any{"id": block["id"], "type": "function", "function": map[string]any{"name": block["name"], "arguments": jsonString(block["input"])}})
				case "tool_result":
					messages = append(messages, map[string]any{"role": "tool", "tool_call_id": block["tool_use_id"], "content": block["content"]})
				case "thinking", "redacted_thinking":
					if block["type"] == "thinking" {
						text, _ := block["thinking"].(string)
						reasoningText += text
						if signature, _ := block["signature"].(string); signature == "" {
							continue
						}
					}
					detail, err := anthropicReasoningToChatDetails(block, index)
					if err != nil {
						return nil, err
					}
					// Attach details to the assistant message below. Thinking is
					// state, not ordinary visible text.
					if role == "assistant" {
						reasoningDetails = append(reasoningDetails, detail)
					}
				}
			}
			if len(textBlocks) > 0 || len(toolCalls) > 0 || len(reasoningDetails) > 0 || reasoningText != "" {
				m := map[string]any{"role": role, "content": textBlocks}
				if reasoningText != "" {
					m["reasoning_content"] = reasoningText
				}
				if len(toolCalls) > 0 {
					m["tool_calls"] = toolCalls
				}
				if len(reasoningDetails) > 0 {
					m["reasoning_details"] = reasoningDetails
				}
				messages = append(messages, m)
			}
		}
		if tools, ok := source["tools"]; ok {
			converted := []any{}
			for _, raw := range asSlice(tools) {
				tool, _ := raw.(map[string]any)
				converted = append(converted, map[string]any{"type": "function", "function": map[string]any{"name": tool["name"], "description": tool["description"], "parameters": tool["input_schema"]}})
			}
			chat["tools"] = converted
		}
		if choice, ok := source["tool_choice"].(map[string]any); ok {
			switch choice["type"] {
			case "auto":
				chat["tool_choice"] = "auto"
			case "any":
				chat["tool_choice"] = "required"
			case "tool":
				chat["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": choice["name"]}}
			}
		}
	} else {
		if instructions, ok := source["instructions"].(string); ok && instructions != "" {
			messages = append(messages, map[string]any{"role": "system", "content": instructions})
		}
		input := source["input"]
		if text, ok := input.(string); ok {
			messages = append(messages, map[string]any{"role": "user", "content": text})
		} else {
			for _, raw := range asSlice(input) {
				item, _ := raw.(map[string]any)
				kind, _ := item["type"].(string)
				switch kind {
				case "reasoning":
					details := responsesReasoningToChatDetails(item)
					if len(details) > 0 {
						messages = append(messages, map[string]any{"role": "assistant", "content": "", "reasoning_details": details})
					}
				case "message", "":
					messages = append(messages, map[string]any{"role": item["role"], "content": responsesContentToChat(item["content"])})
				case "function_call":
					messages = append(messages, map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": item["call_id"], "type": "function", "function": map[string]any{"name": item["name"], "arguments": item["arguments"]}}}})
				case "function_call_output":
					messages = append(messages, map[string]any{"role": "tool", "tool_call_id": item["call_id"], "content": item["output"]})
				}
			}
		}
		if tools, ok := source["tools"]; ok {
			converted := []any{}
			for _, raw := range asSlice(tools) {
				tool, _ := raw.(map[string]any)
				converted = append(converted, map[string]any{"type": "function", "function": map[string]any{"name": tool["name"], "description": tool["description"], "parameters": tool["parameters"], "strict": tool["strict"]}})
			}
			chat["tools"] = converted
		}
		if choice, ok := source["tool_choice"]; ok {
			chat["tool_choice"] = choice
		}
	}
	chat["messages"] = messages
	return chat, nil
}

func chatToMessagesRequest(chat map[string]any) (map[string]any, error) {
	maxTokens := any(4096)
	if max, ok := chat["max_output_tokens"]; ok {
		maxTokens = max
	}
	out := map[string]any{"model": chat["model"], "max_tokens": maxTokens}
	for _, key := range []string{"temperature", "top_p", "stream", "stop_sequences", "metadata"} {
		if v, ok := chat[key]; ok {
			out[key] = v
		}
	}
	if max, ok := chat["max_tokens"]; ok {
		out["max_tokens"] = max
	}
	if max, ok := chat["max_completion_tokens"]; ok {
		out["max_tokens"] = max
	}
	messages := []any{}
	systems := []any{}
	for _, raw := range asSlice(chat["messages"]) {
		message, _ := raw.(map[string]any)
		role, _ := message["role"].(string)
		if role == "system" || role == "developer" {
			systems = append(systems, message["content"])
			continue
		}
		if role == "tool" {
			messages = append(messages, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": message["tool_call_id"], "content": message["content"]}}})
			continue
		}
		content := chatContentToAnthropic(message["content"])
		if details, ok := message["reasoning_details"]; ok {
			blocks, err := chatReasoningDetailsToAnthropic(details)
			if err != nil {
				return nil, err
			}
			content = append(blocks, content...)
		} else if reasoning, ok := message["reasoning_content"].(string); ok && reasoning != "" {
			// Plain reasoning_content has no Claude signature and cannot be
			// used as assistant thinking on a Messages target.
			if len(asSlice(message["tool_calls"])) > 0 {
				return nil, reasoningStateError{"unsigned tool-call reasoning"}
			}
		}
		if calls := asSlice(message["tool_calls"]); calls != nil {
			for _, rawCall := range calls {
				call, _ := rawCall.(map[string]any)
				fn, _ := call["function"].(map[string]any)
				content = append(content, map[string]any{"type": "tool_use", "id": call["id"], "name": fn["name"], "input": jsonValue(fn["arguments"])})
			}
		}
		messages = append(messages, map[string]any{"role": role, "content": content})
	}
	if len(systems) > 0 {
		out["system"] = systems
	}
	out["messages"] = messages
	if tools := asSlice(chat["tools"]); tools != nil {
		converted := []any{}
		for _, raw := range tools {
			tool, _ := raw.(map[string]any)
			fn, _ := tool["function"].(map[string]any)
			converted = append(converted, map[string]any{"name": fn["name"], "description": fn["description"], "input_schema": fn["parameters"]})
		}
		out["tools"] = converted
	}
	if choice, ok := chat["tool_choice"]; ok {
		switch v := choice.(type) {
		case string:
			if v == "required" {
				out["tool_choice"] = map[string]any{"type": "any"}
			} else {
				out["tool_choice"] = map[string]any{"type": v}
			}
		case map[string]any:
			fn, _ := v["function"].(map[string]any)
			out["tool_choice"] = map[string]any{"type": "tool", "name": fn["name"]}
		}
	}
	return out, nil
}

func chatToResponsesRequest(chat map[string]any) (map[string]any, error) {
	out := map[string]any{"model": chat["model"]}

	var input []any
	var systemParts []string

	if messages := asSlice(chat["messages"]); messages != nil {
		for _, raw := range messages {
			msg, ok := raw.(map[string]any)
			if !ok {
				return nil, unsupportedFeature{"message items"}
			}
			role, _ := msg["role"].(string)

			switch role {
			case "system", "developer":
				if text := responsesInstructionText(msg["content"]); text != "" {
					systemParts = append(systemParts, text)
				}

			case "user":
				item, err := userMessageToResponsesItem(msg)
				if err != nil {
					return nil, err
				}
				input = append(input, item)

			case "assistant":
				if details, ok := msg["reasoning_details"]; ok {
					reasoning, err := chatReasoningDetailsToResponses(details)
					if err != nil {
						return nil, err
					}
					input = append(input, reasoning...)
				}
				if calls := asSlice(msg["tool_calls"]); calls != nil && len(calls) > 0 {
					// Assistant tool calls remain separate typed Responses items.
					if hasChatMessageContent(msg["content"]) {
						item, err := assistantMessageToResponsesItem(msg)
						if err != nil {
							return nil, err
						}
						input = append(input, item)
					}
					for _, callRaw := range calls {
						call, ok := callRaw.(map[string]any)
						fn, fnOK := call["function"].(map[string]any)
						if !ok || !fnOK || call["id"] == nil || fn["name"] == nil || fn["arguments"] == nil {
							return nil, unsupportedFeature{"tool calls"}
						}
						id, _ := call["id"].(string)
						input = append(input, map[string]any{
							"type":      "function_call",
							"call_id":   id,
							"name":      fn["name"],
							"arguments": fn["arguments"],
						})
					}
				} else if hasChatMessageContent(msg["content"]) || msg["reasoning_details"] == nil {
					item, err := assistantMessageToResponsesItem(msg)
					if err != nil {
						return nil, err
					}
					input = append(input, item)
				}

			case "tool":
				id, idOK := msg["tool_call_id"].(string)
				content, contentOK := msg["content"].(string)
				if !idOK || id == "" || !contentOK {
					return nil, unsupportedFeature{"tool messages"}
				}
				input = append(input, map[string]any{
					"type":    "function_call_output",
					"call_id": id,
					"output":  content,
				})

			default:
				return nil, unsupportedFeature{"message role " + role}
			}
		}
	}

	if len(systemParts) > 0 {
		joined := strings.Join(systemParts, "\n\n")
		if existing, _ := out["instructions"].(string); existing != "" {
			out["instructions"] = joined + "\n\n" + existing
		} else {
			out["instructions"] = joined
		}
	}

	out["input"] = input

	// Handle tool_choice before the main key loop. The Responses relay
	// accepts only the string "auto" for tool_choice; every other Chat
	// value must be either represented safely or rejected rather than
	// silently weakened. "none" means "do not call tools" — the only
	// safe relay representation is to omit tools entirely. "required"
	// and named function choices have no equivalent and are rejected.
	var suppressTools bool
	if v, ok := chat["tool_choice"]; ok {
		choice, err := convertToolChoice(v)
		if err != nil {
			return nil, err
		}
		if choice == actionSuppressTools {
			suppressTools = true
		} else if choice != "" {
			out["tool_choice"] = choice
		}
	}
	for _, key := range []string{"temperature", "top_p", "stream", "metadata"} {
		if v, ok := chat[key]; ok {
			out[key] = v
		}
	}
	if max, ok := chat["max_completion_tokens"]; ok {
		out["max_output_tokens"] = max
	} else if max, ok := chat["max_tokens"]; ok {
		out["max_output_tokens"] = max
	}
	if !suppressTools {
		if tools := asSlice(chat["tools"]); tools != nil {
			converted := []any{}
			for _, raw := range tools {
				tool, ok := raw.(map[string]any)
				if !ok || tool["type"] != "function" {
					return nil, unsupportedFeature{"non-function tools"}
				}
				fn, fnOK := tool["function"].(map[string]any)
				if !fnOK {
					return nil, unsupportedFeature{"non-function tools"}
				}
				converted = append(converted, map[string]any{"type": "function", "name": fn["name"], "description": fn["description"], "parameters": fn["parameters"], "strict": fn["strict"]})
			}
			out["tools"] = converted
		}
	}
	return out, nil
}

// responsesInstructionText flattens a Chat system/developer message's content
// into a single string for the Responses "instructions" field. It accepts
// either a bare string or a content-part array (with "text" or nested
// "content" entries), matching the canonical Responses shape.
func responsesInstructionText(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			if text := responsesInstructionText(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		if text, ok := value["text"]; ok {
			return responsesInstructionText(text)
		}
		if content, ok := value["content"]; ok {
			return responsesInstructionText(content)
		}
	}
	return ""
}

func translateResponse(w http.ResponseWriter, r io.Reader, incoming, target providers.Protocol, route resolvedRoute, usage *usageCapture) error {
	reader := bufio.NewReader(r)
	prefix, err := reader.Peek(1)
	if err != nil {
		return err
	}
	if prefix[0] == '{' || prefix[0] == '[' {
		body, err := io.ReadAll(io.LimitReader(reader, 64<<20))
		if err != nil {
			return err
		}
		extractUsage(body, usage)
		translated, err := translateNonstreamResponse(body, incoming, target, route.RequestedModel)
		if err != nil {
			return err
		}
		_, err = w.Write(translated)
		return err
	}
	return translateSSE(w, reader, incoming, target, route.RequestedModel, usage)
}

func translateNonstreamResponse(body []byte, incoming, target providers.Protocol, model string) ([]byte, error) {
	var source map[string]any
	if err := json.Unmarshal(body, &source); err != nil {
		return nil, err
	}
	if err := validateResponseReasoningState(source, target, incoming); err != nil {
		return nil, err
	}
	chat := responseToChat(source, target, model)
	switch incoming {
	case providers.ProtocolChat:
		return json.Marshal(chat)
	case providers.ProtocolMessages:
		return json.Marshal(chatResponseToMessages(chat, model))
	case providers.ProtocolResponses:
		return json.Marshal(chatResponseToResponses(chat, model))
	}
	return nil, errors.New("unknown client protocol")
}

func responseToChat(source map[string]any, target providers.Protocol, model string) map[string]any {
	if target == providers.ProtocolChat {
		if choices := asSlice(source["choices"]); len(choices) > 0 {
			if choice, ok := choices[0].(map[string]any); ok {
				if message, ok := choice["message"].(map[string]any); ok {
					if text, _ := message["reasoning_content"].(string); text == "" {
						if reasoning, ok := message["reasoning"].(string); ok && reasoning != "" {
							message["reasoning_content"] = reasoning
						}
					}
				}
			}
		}
		source["model"] = model
		return source
	}
	message := map[string]any{"role": "assistant", "content": ""}
	finish := "stop"
	usage := source["usage"]
	if target == providers.ProtocolMessages {
		content := []any{}
		var reasoningText string
		for index, raw := range asSlice(source["content"]) {
			block, _ := raw.(map[string]any)
			switch block["type"] {
			case "text":
				if text, _ := block["text"].(string); text != "" {
					content = append(content, map[string]any{"type": "text", "text": text})
				}
			case "thinking", "redacted_thinking":
				if text, _ := block["thinking"].(string); text != "" {
					reasoningText += text
				}
				if detail, err := anthropicReasoningToChatDetails(block, index); err == nil {
					message["reasoning_details"] = append(asSlice(message["reasoning_details"]), detail)
				}
			case "tool_use":
				calls, _ := message["tool_calls"].([]any)
				calls = append(calls, map[string]any{"id": block["id"], "type": "function", "function": map[string]any{"name": block["name"], "arguments": jsonString(block["input"])}})
				message["tool_calls"] = calls
			}
		}
		if reasoningText != "" {
			message["reasoning_content"] = reasoningText
		}
		message["content"] = content
		if source["stop_reason"] == "tool_use" {
			finish = "tool_calls"
		}
		if u, ok := usage.(map[string]any); ok {
			usage = map[string]any{"prompt_tokens": u["input_tokens"], "completion_tokens": u["output_tokens"], "total_tokens": number(u["input_tokens"]) + number(u["output_tokens"])}
		}
	} else {
		content := []any{}
		calls := []any{}
		var reasoningText string
		for _, raw := range asSlice(source["output"]) {
			item, _ := raw.(map[string]any)
			switch item["type"] {
			case "message":
				for _, partRaw := range asSlice(item["content"]) {
					part, _ := partRaw.(map[string]any)
					switch part["type"] {
					case "output_text":
						content = append(content, map[string]any{"type": "text", "text": part["text"]})
					case "reasoning":
						reasoningText += fmt.Sprint(part["text"])
					}
				}
			case "reasoning":
				message["reasoning_details"] = append(asSlice(message["reasoning_details"]), responsesReasoningToChatDetails(item)...)
				for _, summaryRaw := range asSlice(item["summary"]) {
					if summary, ok := summaryRaw.(map[string]any); ok {
						reasoningText += fmt.Sprint(summary["text"])
					}
				}
			case "function_call":
				calls = append(calls, map[string]any{"id": item["call_id"], "type": "function", "function": map[string]any{"name": item["name"], "arguments": item["arguments"]}})
			}
		}
		if reasoningText != "" {
			message["reasoning_content"] = reasoningText
		}
		message["content"] = content
		if len(calls) > 0 {
			message["tool_calls"] = calls
			finish = "tool_calls"
		}
		if u, ok := usage.(map[string]any); ok {
			usage = map[string]any{"prompt_tokens": u["input_tokens"], "completion_tokens": u["output_tokens"], "total_tokens": u["total_tokens"]}
		}
	}
	return map[string]any{"id": source["id"], "object": "chat.completion", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": usage}
}

func chatResponseToMessages(chat map[string]any, model string) map[string]any {
	choices := asSlice(chat["choices"])
	choice := map[string]any{}
	if len(choices) > 0 {
		choice, _ = choices[0].(map[string]any)
	}
	message, _ := choice["message"].(map[string]any)
	content := chatContentToAnthropic(message["content"])
	for _, raw := range asSlice(message["tool_calls"]) {
		call, _ := raw.(map[string]any)
		fn, _ := call["function"].(map[string]any)
		content = append(content, map[string]any{"type": "tool_use", "id": call["id"], "name": fn["name"], "input": jsonValue(fn["arguments"])})
	}
	if blocks, _ := chatReasoningDetailsToAnthropic(message["reasoning_details"]); len(blocks) > 0 {
		content = append(blocks, content...)
	} else if reasoning, ok := message["reasoning_content"].(string); ok && reasoning != "" {
		content = append([]any{map[string]any{"type": "thinking", "thinking": reasoning}}, content...)
	}
	stop := "end_turn"
	if choice["finish_reason"] == "tool_calls" {
		stop = "tool_use"
	}
	usage := map[string]any{}
	if u, ok := chat["usage"].(map[string]any); ok {
		usage = map[string]any{"input_tokens": u["prompt_tokens"], "output_tokens": u["completion_tokens"]}
	}
	return map[string]any{"id": chat["id"], "type": "message", "role": "assistant", "model": model, "content": content, "stop_reason": stop, "stop_sequence": nil, "usage": usage}
}

func chatResponseToResponses(chat map[string]any, model string) map[string]any {
	choices := asSlice(chat["choices"])
	choice := map[string]any{}
	if len(choices) > 0 {
		choice, _ = choices[0].(map[string]any)
	}
	message, _ := choice["message"].(map[string]any)
	output := []any{}
	parts := []any{}
	for _, part := range chatContentToAnthropic(message["content"]) {
		block, _ := part.(map[string]any)
		if block["type"] == "text" {
			parts = append(parts, map[string]any{"type": "output_text", "text": block["text"], "annotations": []any{}})
		}
	}
	output = append(output, map[string]any{"id": "msg_" + fmt.Sprint(chat["id"]), "type": "message", "role": "assistant", "status": "completed", "content": parts})
	if items, _ := chatReasoningDetailsToResponses(message["reasoning_details"]); len(items) > 0 {
		output = append(output, items...)
	} else if reasoning, ok := message["reasoning_content"].(string); ok && reasoning != "" {
		output = append(output, map[string]any{"id": "rs_" + fmt.Sprint(chat["id"]), "type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": reasoning}}})
	}
	for _, raw := range asSlice(message["tool_calls"]) {
		call, _ := raw.(map[string]any)
		fn, _ := call["function"].(map[string]any)
		output = append(output, map[string]any{"type": "function_call", "call_id": call["id"], "name": fn["name"], "arguments": fn["arguments"], "status": "completed"})
	}
	usage := map[string]any{}
	if u, ok := chat["usage"].(map[string]any); ok {
		usage = map[string]any{"input_tokens": u["prompt_tokens"], "output_tokens": u["completion_tokens"], "total_tokens": u["total_tokens"]}
	}
	return map[string]any{"id": chat["id"], "object": "response", "created_at": time.Now().Unix(), "status": "completed", "model": model, "output": output, "usage": usage}
}

func translateSSE(w http.ResponseWriter, reader *bufio.Reader, incoming, target providers.Protocol, model string, usage *usageCapture) error {
	flusher, _ := w.(http.Flusher)
	state := &streamState{id: "tiller_" + fmt.Sprint(time.Now().UnixNano()), model: model, reasoningIndex: -1, messageIndex: -1, toolIndex: -1}
	for {
		event, err := readSSEEvent(reader)
		data := event.Data
		if string(data) == "[DONE]" {
			state.completed = true
			writeStreamDone(w, incoming, state)
			if flusher != nil {
				flusher.Flush()
			}
			return nil
		}
		if len(data) > 0 && string(data) != "[DONE]" {
			var payload map[string]any
			if json.Unmarshal(data, &payload) == nil {
				captureStreamUsage(payload, target, usage)
				deltas, done := canonicalDeltas(event.Name, payload, target, state)
				for _, delta := range deltas {
					if delta.Kind == "error" {
						return errors.New("upstream stream reported failure")
					}
					if err := writeTranslatedEvent(w, incoming, state, delta); err != nil {
						return err
					}
					if flusher != nil {
						flusher.Flush()
					}
				}
				if done {
					state.completed = true
					writeStreamDone(w, incoming, state)
					if flusher != nil {
						flusher.Flush()
					}
					return nil
				}
			}
		}
		if err != nil {
			if err == io.EOF && state.completed {
				writeStreamDone(w, incoming, state)
				return nil
			}
			if err == io.EOF {
				return errors.New("upstream stream ended before completion")
			}
			return err
		}
	}
}

func writeStreamFailure(w io.Writer, protocol providers.Protocol, code, id, model string) {
	message := "The upstream stream could not be completed."
	switch protocol {
	case providers.ProtocolMessages:
		writeSSE(w, "error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": message}})
	case providers.ProtocolResponses:
		writeSSE(w, "response.failed", map[string]any{"type": "response.failed", "response": map[string]any{"id": id, "object": "response", "status": "failed", "model": model, "output": []any{}, "error": map[string]any{"code": code, "message": message}}})
	default:
		writeSSE(w, "", map[string]any{"error": map[string]any{"type": "server_error", "code": code, "message": message}})
	}
}

const (
	maxAccumulatedTextBytes         = 8 * 1024 * 1024
	maxAccumulatedToolArgumentBytes = 8 * 1024 * 1024
	maxStreamToolCalls              = 128
)

type streamState struct {
	id, model                                                  string
	started, contentStarted, reasoningStarted, done, completed bool
	outputIndex                                                int
	accumulated                                                strings.Builder
	accumulatedBytes                                           int
	reasoningAccumulated                                       strings.Builder
	nextIndex                                                  int
	reasoningIndex, messageIndex, toolIndex                    int
	outputOrder                                                []streamOutput
	extraOutputs                                               map[int]any
	activeKind                                                 string
	activeIndex                                                int
	toolCalls                                                  []*toolCallState
	toolAliases                                                map[string]*toolCallState
	toolArgumentBytes                                          int
	messagesToolsWritten                                       bool
	inputTokens                                                int64
	outputTokens                                               int64
	hasInputTokens                                             bool
	hasOutputTokens                                            bool
}
type toolCallState struct {
	callID    string
	name      string
	arguments strings.Builder
	itemID    string
	index     int
	ordinal   int
}

type streamOutput struct {
	kind string
	tool *toolCallState
}

type canonicalDelta struct {
	Kind, Text, CallID, Name, Arguments, Finish string
	UpstreamIndex                               int
	HasUpstreamIndex                            bool
	ItemID                                      string
	Usage                                       any
	Detail                                      map[string]any
}

type sseEvent struct {
	Name  string
	Data  []byte
	ID    string
	Retry *int
}

func strField(v any) string {
	value, _ := v.(string)
	return value
}

func coerceOrDefault(v any, fallback int64) int64 {
	if value, ok := coerceInt64(v); ok {
		return value
	}
	return fallback
}

func canonicalDeltas(event string, payload map[string]any, target providers.Protocol, state *streamState) ([]canonicalDelta, bool) {
	out := []canonicalDelta{}
	if target == providers.ProtocolChat {
		if id, ok := payload["id"].(string); ok {
			state.id = id
		}
		for _, raw := range asSlice(payload["choices"]) {
			choice, _ := raw.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			if text, ok := delta["content"].(string); ok && text != "" {
				out = append(out, canonicalDelta{Kind: "text", Text: text})
			}
			if text, ok := delta["reasoning_content"].(string); ok && text != "" {
				out = append(out, canonicalDelta{Kind: "reasoning", Text: text})
			} else if text, ok := delta["reasoning"].(string); ok && text != "" {
				out = append(out, canonicalDelta{Kind: "reasoning", Text: text})
			}
			for index, callRaw := range asSlice(delta["tool_calls"]) {
				call, _ := callRaw.(map[string]any)
				fn, _ := call["function"].(map[string]any)
				upstreamIndex := index
				if value, ok := coerceInt64(call["index"]); ok {
					upstreamIndex = int(value)
				}
				out = append(out, canonicalDelta{Kind: "tool", UpstreamIndex: upstreamIndex, HasUpstreamIndex: true, CallID: strField(call["id"]), Name: strField(fn["name"]), Arguments: strField(fn["arguments"])})
			}
			if finish, ok := choice["finish_reason"].(string); ok && finish != "" {
				out = append(out, canonicalDelta{Kind: "finish", Finish: finish})
			}
		}
		if usage := payload["usage"]; usage != nil {
			out = append(out, canonicalDelta{Kind: "usage", Usage: usage})
		}
		return out, false
	}
	if target == providers.ProtocolMessages {
		switch event {
		case "message_start":
			if message, ok := payload["message"].(map[string]any); ok {
				if id, ok := message["id"].(string); ok {
					state.id = id
				}
				if u, ok := message["usage"].(map[string]any); ok {
					out = append(out, canonicalDelta{Kind: "usage", Usage: map[string]any{"input_tokens": u["input_tokens"]}})
				}
			}
		case "content_block_start":
			if block, ok := payload["content_block"].(map[string]any); ok && block["type"] == "tool_use" {
				out = append(out, canonicalDelta{Kind: "tool", UpstreamIndex: int(coerceOrDefault(payload["index"], 0)), HasUpstreamIndex: true, CallID: strField(block["id"]), Name: strField(block["name"])})
			}
		case "content_block_delta":
			if delta, ok := payload["delta"].(map[string]any); ok {
				if text, ok := delta["text"].(string); ok {
					out = append(out, canonicalDelta{Kind: "text", Text: text})
				}
				if thinking, ok := delta["thinking"].(string); ok && thinking != "" {
					out = append(out, canonicalDelta{Kind: "reasoning", Text: thinking})
				}
				if partial, ok := delta["partial_json"].(string); ok {
					out = append(out, canonicalDelta{Kind: "tool", UpstreamIndex: int(coerceOrDefault(payload["index"], 0)), HasUpstreamIndex: true, Arguments: partial})
				}
			}
		case "message_delta":
			if u, ok := payload["usage"].(map[string]any); ok {
				out = append(out, canonicalDelta{Kind: "usage", Usage: map[string]any{"output_tokens": u["output_tokens"]}})
			}
			if delta, ok := payload["delta"].(map[string]any); ok {
				out = append(out, canonicalDelta{Kind: "finish", Finish: fmt.Sprint(delta["stop_reason"])})
			}
		case "message_stop":
			state.completed = true
			return out, true
		case "message_error", "error":
			return []canonicalDelta{{Kind: "error", Text: "upstream stream error"}}, false
		}
		return out, false
	}
	// Responses events.
	switch event {
	case "response.created":
		if response, ok := payload["response"].(map[string]any); ok {
			if id, ok := response["id"].(string); ok {
				state.id = id
			}
		}
	case "response.output_text.delta":
		out = append(out, canonicalDelta{Kind: "text", Text: fmt.Sprint(payload["delta"])})
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		out = append(out, canonicalDelta{Kind: "reasoning", Text: fmt.Sprint(payload["delta"])})
	case "response.function_call_arguments.delta":
		index, hasIndex := coerceInt64(payload["output_index"])
		out = append(out, canonicalDelta{Kind: "tool", UpstreamIndex: int(index), HasUpstreamIndex: hasIndex, CallID: strField(payload["call_id"]), ItemID: strField(payload["item_id"]), Arguments: strField(payload["delta"])})
	case "response.output_item.added":
		if item, ok := payload["item"].(map[string]any); ok && item["type"] == "function_call" {
			index, hasIndex := coerceInt64(payload["output_index"])
			out = append(out, canonicalDelta{Kind: "tool", UpstreamIndex: int(index), HasUpstreamIndex: hasIndex, CallID: strField(item["call_id"]), ItemID: strField(item["id"]), Name: strField(item["name"])})
		}
	case "response.failed", "response.incomplete", "error":
		return []canonicalDelta{{Kind: "error", Text: "upstream stream error"}}, false
	case "response.completed":
		state.completed = true
		if response, ok := payload["response"].(map[string]any); ok {
			if u, ok := response["usage"].(map[string]any); ok {
				out = append(out, canonicalDelta{Kind: "usage", Usage: u})
			}
		}
		// Responses has no separate finish event. Emit one so translated
		// Chat and Messages streams can close their assistant message cleanly.
		out = append(out, canonicalDelta{Kind: "finish", Finish: "stop"})
		return out, true
	}
	return out, false
}

func writeTranslatedEvent(w io.Writer, incoming providers.Protocol, state *streamState, delta canonicalDelta) error {
	if incoming == providers.ProtocolChat {
		payload := map[string]any{"id": state.id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": state.model}
		choice := map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": nil}
		d := choice["delta"].(map[string]any)
		if !state.started {
			d["role"] = "assistant"
			state.started = true
		}
		switch delta.Kind {
		case "text":
			d["content"] = delta.Text
		case "reasoning":
			d["reasoning_content"] = delta.Text
		case "tool":
			call, err := state.applyToolDelta(delta)
			if err != nil {
				return err
			}
			d["tool_calls"] = []any{map[string]any{"index": call.ordinal, "id": emptyNil(delta.CallID), "type": "function", "function": map[string]any{"name": emptyNil(delta.Name), "arguments": delta.Arguments}}}
		case "finish":
			choice["finish_reason"] = normalizeFinish(delta.Finish)
		case "error":
			return errors.New("upstream stream reported failure")
		}
		payload["choices"] = []any{choice}
		if delta.Kind == "usage" {
			payload["choices"] = []any{}
			payload["usage"] = chatUsage(delta.Usage)
		}
		writeSSE(w, "", payload)
		return nil
	}
	if incoming == providers.ProtocolMessages {
		if !state.started {
			writeSSE(w, "message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": state.id, "type": "message", "role": "assistant", "model": state.model, "content": []any{}, "stop_reason": nil, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}})
			state.started = true
		}
		switch delta.Kind {
		case "text":
			if state.activeKind != "text" {
				closeMessagesBlock(w, state)
				state.activeIndex = state.nextIndex
				state.nextIndex++
				writeSSE(w, "content_block_start", map[string]any{"type": "content_block_start", "index": state.activeIndex, "content_block": map[string]any{"type": "text", "text": ""}})
				state.activeKind = "text"
				state.contentStarted = true
			}
			writeSSE(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": state.activeIndex, "delta": map[string]any{"type": "text_delta", "text": delta.Text}})
		case "reasoning":
			if state.activeKind != "reasoning" {
				closeMessagesBlock(w, state)
				state.activeIndex = state.nextIndex
				state.nextIndex++
				writeSSE(w, "content_block_start", map[string]any{"type": "content_block_start", "index": state.activeIndex, "content_block": map[string]any{"type": "thinking", "thinking": ""}})
				state.activeKind = "reasoning"
				state.reasoningStarted = true
			}
			writeSSE(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": state.activeIndex, "delta": map[string]any{"type": "thinking_delta", "thinking": delta.Text}})
		case "tool":
			if _, err := state.applyToolDelta(delta); err != nil {
				return err
			}
		case "usage":
			if u, ok := delta.Usage.(map[string]any); ok {
				if value, ok := coerceInt64(u["input_tokens"]); ok && !state.hasInputTokens {
					state.inputTokens = value
					state.hasInputTokens = true
				}
				if value, ok := coerceInt64(u["output_tokens"]); ok && !state.hasOutputTokens {
					state.outputTokens = value
					state.hasOutputTokens = true
				}
			}
		case "finish":
			closeMessagesBlock(w, state)
			if !state.messagesToolsWritten {
				for _, call := range state.toolCalls {
					index := state.nextIndex
					state.nextIndex++
					writeSSE(w, "content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "tool_use", "id": call.callID, "name": call.name, "input": map[string]any{}}})
					if call.arguments.Len() > 0 {
						writeSSE(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": call.arguments.String()}})
					}
					writeSSE(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
				}
				state.messagesToolsWritten = true
			}
			usage := map[string]any{"input_tokens": 0, "output_tokens": 0}
			if state.hasInputTokens {
				usage["input_tokens"] = state.inputTokens
			}
			if state.hasOutputTokens {
				usage["output_tokens"] = state.outputTokens
			}
			writeSSE(w, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": normalizeAnthropicFinish(delta.Finish), "stop_sequence": nil}, "usage": usage})
		case "error":
			return errors.New("upstream stream reported failure")
		}
		return nil
	}
	if !state.started {
		response := map[string]any{"id": state.id, "object": "response", "created_at": time.Now().Unix(), "status": "in_progress", "model": state.model, "output": []any{}}
		writeSSE(w, "response.created", map[string]any{"type": "response.created", "response": response})
		state.started = true
	}
	switch delta.Kind {
	case "error":
		return errors.New("upstream stream reported failure")
	case "text":
		if state.contentStarted == false {
			state.outputIndex = state.nextIndex
			state.messageIndex = state.outputIndex
			state.nextIndex++
			state.outputOrder = append(state.outputOrder, streamOutput{kind: "message"})
			writeSSE(w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": state.outputIndex, "item": map[string]any{"id": "msg_" + state.id, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}})
			writeSSE(w, "response.content_part.added", map[string]any{"type": "response.content_part.added", "output_index": state.outputIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
			state.contentStarted = true
		}
		if len(delta.Text) > maxAccumulatedTextBytes-state.accumulatedBytes {
			return errors.New("translated stream text exceeds limit")
		}
		state.accumulated.WriteString(delta.Text)
		state.accumulatedBytes += len(delta.Text)
		writeSSE(w, "response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": state.messageIndex, "item_id": "msg_" + state.id, "content_index": 0, "delta": delta.Text})
	case "reasoning":
		if !state.reasoningStarted {
			state.outputIndex = state.nextIndex
			state.reasoningIndex = state.outputIndex
			state.nextIndex++
			state.outputOrder = append(state.outputOrder, streamOutput{kind: "reasoning"})
			writeSSE(w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": state.outputIndex, "item": map[string]any{"id": "rs_" + state.id, "type": "reasoning", "status": "in_progress", "summary": []any{}}})
			writeSSE(w, "response.reasoning_summary_part.added", map[string]any{"type": "response.reasoning_summary_part.added", "output_index": state.outputIndex, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}})
			state.reasoningStarted = true
		}
		if len(delta.Text) > maxAccumulatedTextBytes-state.reasoningAccumulated.Len() {
			return errors.New("translated stream reasoning exceeds limit")
		}
		state.reasoningAccumulated.WriteString(delta.Text)
		writeSSE(w, "response.reasoning_summary_text.delta", map[string]any{"type": "response.reasoning_summary_text.delta", "output_index": state.reasoningIndex, "item_id": "rs_" + state.id, "summary_index": 0, "delta": delta.Text})
	case "tool":
		call, err := state.applyToolDelta(delta)
		if err != nil {
			return err
		}
		if call.index < 0 {
			state.outputIndex = state.nextIndex
			call.index = state.nextIndex
			state.nextIndex++
			state.outputOrder = append(state.outputOrder, streamOutput{kind: "tool", tool: call})
			writeSSE(w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": call.index, "item": map[string]any{"type": "function_call", "call_id": delta.CallID, "name": delta.Name, "arguments": ""}})
		}
		if delta.Arguments != "" {
			writeSSE(w, "response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "output_index": call.index, "item_id": call.itemID, "call_id": call.callID, "delta": delta.Arguments})
		}
	}
	return nil
}

func (state *streamState) applyToolDelta(delta canonicalDelta) (*toolCallState, error) {
	aliases := []string{}
	if delta.ItemID != "" {
		aliases = append(aliases, "item:"+delta.ItemID)
	}
	if delta.CallID != "" {
		aliases = append(aliases, "call:"+delta.CallID)
	}
	if delta.HasUpstreamIndex {
		aliases = append(aliases, fmt.Sprintf("index:%d", delta.UpstreamIndex))
	}
	var call *toolCallState
	for _, alias := range aliases {
		if existing := state.toolAliases[alias]; existing != nil {
			call = existing
			break
		}
	}
	if call == nil {
		if len(state.toolCalls) >= maxStreamToolCalls {
			return nil, errors.New("translated stream has too many tool calls")
		}
		call = &toolCallState{index: -1, ordinal: len(state.toolCalls)}
		state.toolCalls = append(state.toolCalls, call)
	}
	if state.toolAliases == nil {
		state.toolAliases = make(map[string]*toolCallState)
	}
	for _, alias := range aliases {
		state.toolAliases[alias] = call
	}
	if delta.CallID != "" {
		call.callID = delta.CallID
	}
	if delta.ItemID != "" {
		call.itemID = delta.ItemID
	}
	if call.itemID == "" {
		call.itemID = call.callID
	}
	if delta.Name != "" {
		call.name = delta.Name
	}
	if len(delta.Arguments) > maxAccumulatedToolArgumentBytes-state.toolArgumentBytes {
		return nil, errors.New("translated stream tool arguments exceed limit")
	}
	call.arguments.WriteString(delta.Arguments)
	state.toolArgumentBytes += len(delta.Arguments)
	return call, nil
}

func closeMessagesBlock(w io.Writer, state *streamState) {
	if state.activeKind != "" {
		writeSSE(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": state.activeIndex})
		state.activeKind = ""
	}
}

func chatUsage(value any) any {
	u, ok := value.(map[string]any)
	if !ok {
		return value
	}
	if _, hasPrompt := u["prompt_tokens"]; hasPrompt {
		return u
	}
	converted := map[string]any{}
	for key, value := range u {
		converted[key] = value
	}
	if input, ok := converted["input_tokens"]; ok {
		converted["prompt_tokens"] = input
		delete(converted, "input_tokens")
	}
	if output, ok := converted["output_tokens"]; ok {
		converted["completion_tokens"] = output
		delete(converted, "output_tokens")
	}
	return converted
}

func writeStreamDone(w io.Writer, incoming providers.Protocol, state *streamState) {
	if state.done {
		return
	}
	state.done = true
	if incoming == providers.ProtocolChat {
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		return
	}
	if incoming == providers.ProtocolMessages {
		if !state.started || (!state.messagesToolsWritten && len(state.toolCalls) > 0) {
			_ = writeTranslatedEvent(w, incoming, state, canonicalDelta{Kind: "finish", Finish: "stop"})
		} else {
			closeMessagesBlock(w, state)
		}
		writeSSE(w, "message_stop", map[string]any{"type": "message_stop"})
		return
	}
	if state.reasoningStarted {
		text := state.reasoningAccumulated.String()
		writeSSE(w, "response.reasoning_summary_text.done", map[string]any{"type": "response.reasoning_summary_text.done", "output_index": state.reasoningIndex, "summary_index": 0, "text": text})
		writeSSE(w, "response.content_part.done", map[string]any{"type": "response.content_part.done", "output_index": state.reasoningIndex, "content_index": 0, "part": map[string]any{"type": "summary_text", "text": text}})
		writeSSE(w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": state.reasoningIndex, "item": map[string]any{"id": "rs_" + state.id, "type": "reasoning", "status": "completed", "summary": []any{map[string]any{"type": "summary_text", "text": text}}}})
	}
	if len(state.toolCalls) > 0 {
		for _, call := range state.toolCalls {
			args := call.arguments.String()
			writeSSE(w, "response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "output_index": call.index, "item_id": call.itemID, "call_id": call.callID, "arguments": args})
			writeSSE(w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": call.index, "item": map[string]any{"type": "function_call", "call_id": call.callID, "name": call.name, "arguments": args, "status": "completed"}})
		}
	}
	text := state.accumulated.String()
	output := []any{}
	if state.messageIndex >= 0 {
		writeSSE(w, "response.output_text.done", map[string]any{"type": "response.output_text.done", "output_index": state.messageIndex, "content_index": 0, "text": text})
		writeSSE(w, "response.content_part.done", map[string]any{"type": "response.content_part.done", "output_index": state.messageIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}}})
		writeSSE(w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": state.messageIndex, "item": map[string]any{"id": "msg_" + state.id, "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}})
		output = append(output, map[string]any{"id": "msg_" + state.id, "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}})
	}
	if state.reasoningStarted || len(state.toolCalls) > 0 {
		reasoningItem := map[string]any{"id": "rs_" + state.id, "type": "reasoning", "status": "completed", "summary": []any{map[string]any{"type": "summary_text", "text": state.reasoningAccumulated.String()}}}
		if len(state.outputOrder) > 1 {
			output = nil
			for _, item := range state.outputOrder {
				if item.kind == "reasoning" {
					output = append(output, reasoningItem)
				} else if item.kind == "message" {
					output = append(output, map[string]any{"id": "msg_" + state.id, "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}})
				} else if item.kind == "tool" && item.tool != nil {
					output = append(output, map[string]any{"type": "function_call", "call_id": item.tool.callID, "name": item.tool.name, "arguments": item.tool.arguments.String(), "status": "completed"})
				}
			}
		} else if state.reasoningStarted {
			output = append([]any{reasoningItem}, output...)
		} else {
			for _, call := range state.toolCalls {
				output = append(output, map[string]any{"type": "function_call", "call_id": call.callID, "name": call.name, "arguments": call.arguments.String(), "status": "completed"})
			}
		}
	}
	response := map[string]any{"id": state.id, "object": "response", "created_at": time.Now().Unix(), "status": "completed", "model": state.model, "output": output}
	writeSSE(w, "response.completed", map[string]any{"type": "response.completed", "response": response})
}

const maxSSELineBytes = 1 << 20

func readSSEEvent(r *bufio.Reader) (sseEvent, error) {
	var event sseEvent
	for {
		var line []byte
		for {
			fragment, err := r.ReadSlice('\n')
			line = append(line, fragment...)
			if len(line) > maxSSELineBytes {
				return event, errors.New("SSE line exceeds limit")
			}
			if err != bufio.ErrBufferFull {
				if err != nil && len(line) == 0 {
					return event, err
				}
				break
			}
		}
		trim := strings.TrimRight(string(line), "\r\n")
		field, value, hasField := strings.Cut(trim, ":")
		if hasField {
			value = strings.TrimPrefix(value, " ")
		}
		switch field {
		case "event":
			event.Name = value
		case "data":
			if len(event.Data) > 0 {
				event.Data = append(event.Data, '\n')
			}
			if len(event.Data)+len(value) > maxAccumulatedTextBytes {
				return event, errors.New("SSE event exceeds limit")
			}
			event.Data = append(event.Data, value...)
		case "id":
			event.ID = value
		case "retry":
			var retry int
			if _, scanErr := fmt.Sscanf(value, "%d", &retry); scanErr == nil && retry >= 0 {
				event.Retry = &retry
			}
		}
		if trim == "" && (len(event.Data) > 0 || event.Name != "" || event.ID != "" || event.Retry != nil) {
			return event, nil
		}
	}
}
func writeSSE(w io.Writer, event string, payload any) {
	body, _ := json.Marshal(payload)
	if event != "" {
		_, _ = fmt.Fprintf(w, "event: %s\n", event)
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", body)
}

func asSlice(value any) []any {
	if value == nil {
		return nil
	}
	if values, ok := value.([]any); ok {
		return values
	}
	return nil
}

func userMessageToResponsesItem(msg map[string]any) (map[string]any, error) {
	return messageToResponsesItem(msg, "user")
}

func messageToResponsesItem(msg map[string]any, role string) (map[string]any, error) {
	parts, err := chatContentToResponsesParts(msg["content"])
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"type":    "message",
		"role":    role,
		"content": parts,
	}, nil
}

func hasChatMessageContent(value any) bool {
	switch content := value.(type) {
	case string:
		return content != ""
	case []any:
		return len(content) > 0
	default:
		return value != nil
	}
}

func assistantMessageToResponsesItem(msg map[string]any) (map[string]any, error) {
	parts, err := chatContentToResponsesParts(msg["content"])
	if err != nil {
		return nil, err
	}
	rewriteResponsesTextRoles(parts, "output_text")
	return map[string]any{
		"type":    "message",
		"role":    "assistant",
		"content": parts,
	}, nil
}

func chatContentToResponsesParts(value any) ([]any, error) {
	if str, ok := value.(string); ok {
		return []any{map[string]any{"type": "input_text", "text": str}}, nil
	}
	parts := []any{}
	list := asSlice(value)
	if list == nil {
		return nil, unsupportedFeature{"message content"}
	}
	for _, raw := range list {
		block, ok := raw.(map[string]any)
		if !ok {
			return nil, unsupportedFeature{"message content blocks"}
		}
		var part map[string]any
		switch block["type"] {
		case "text":
			text, ok := block["text"].(string)
			if !ok {
				return nil, unsupportedFeature{"text content blocks"}
			}
			part = map[string]any{"type": "input_text", "text": text}
		case "image_url":
			url, ok := block["image_url"].(map[string]any)
			value, valueOK := url["url"].(string)
			if !ok || !valueOK || value == "" {
				return nil, unsupportedFeature{"image content blocks"}
			}
			part = map[string]any{"type": "input_image", "image_url": value}
		default:
			return nil, unsupportedFeature{"content block " + fmt.Sprint(block["type"])}
		}
		parts = append(parts, part)
	}
	return parts, nil
}

// rewriteResponsesTextRoles rewrites input_text content parts to the
// specified type. The Responses spec requires input_text on user messages
// and output_text on assistant messages; chatContentToResponsesParts emits
// input_text for all roles, so callers building assistant items must rewrite.
func rewriteResponsesTextRoles(parts []any, toType string) {
	for _, raw := range parts {
		if block, ok := raw.(map[string]any); ok && block["type"] == "input_text" {
			block["type"] = toType
		}
	}
}

type toolChoiceAction int

const actionSuppressTools toolChoiceAction = iota + 1

func convertToolChoice(value any) (any, error) {
	// The Responses wire shape used by chat-completions translation only
	// supports tool_choice as the string "auto". "none" means "do not
	// call tools" — the only safe relay representation is to omit tools
	// entirely. "required" and named function choices have no equivalent
	// and are rejected as unsupported rather than silently weakened.
	v, ok := value.(string)
	if !ok {
		return nil, unsupportedFeature{"tool_choice"}
	}
	switch v {
	case "auto":
		return v, nil
	case "none":
		return actionSuppressTools, nil
	default:
		return nil, unsupportedFeature{"tool_choice " + v}
	}
}

func chatContentToAnthropic(value any) []any {
	if text, ok := value.(string); ok {
		return []any{map[string]any{"type": "text", "text": text}}
	}
	out := []any{}
	for _, raw := range asSlice(value) {
		block, _ := raw.(map[string]any)
		switch block["type"] {
		case "text", "input_text":
			out = append(out, map[string]any{"type": "text", "text": first(block["text"], block["input_text"])})
		case "image_url", "input_image":
			out = append(out, openAIImageToAnthropic(block))
		}
	}
	return out
}
func responsesContentToChat(value any) any {
	out := []any{}
	for _, raw := range asSlice(value) {
		block, _ := raw.(map[string]any)
		switch block["type"] {
		case "input_text", "output_text":
			out = append(out, map[string]any{"type": "text", "text": block["text"]})
		case "input_image":
			out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": block["image_url"]}})
		}
	}
	return out
}
func openAIImageToAnthropic(block map[string]any) map[string]any {
	raw := block["image_url"]
	if nested, ok := raw.(map[string]any); ok {
		raw = nested["url"]
	}
	urlText, _ := raw.(string)
	if strings.HasPrefix(urlText, "data:") {
		parts := strings.SplitN(urlText, ",", 2)
		meta := strings.TrimPrefix(strings.Split(parts[0], ";")[0], "data:")
		data := ""
		if len(parts) == 2 {
			data = parts[1]
		}
		return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": meta, "data": data}}
	}
	return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": urlText}}
}
func anthropicImageToOpenAI(block map[string]any) map[string]any {
	source, _ := block["source"].(map[string]any)
	urlText := fmt.Sprint(source["url"])
	if source["type"] == "base64" {
		urlText = "data:" + fmt.Sprint(source["media_type"]) + ";base64," + fmt.Sprint(source["data"])
	}
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": urlText}}
}
func jsonString(value any) string { body, _ := json.Marshal(value); return string(body) }
func jsonValue(value any) any {
	if text, ok := value.(string); ok {
		var decoded any
		if json.Unmarshal([]byte(text), &decoded) == nil {
			return decoded
		}
	}
	return value
}
func number(value any) float64 {
	if n, ok := value.(float64); ok {
		return n
	}
	return 0
}
func first(values ...any) any {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return ""
}
func emptyNil(value string) any {
	if value == "" || value == "<nil>" {
		return nil
	}
	return value
}
func normalizeFinish(value string) string {
	switch value {
	case "end_turn", "stop_sequence", "stop":
		return "stop"
	case "max_tokens", "length":
		return "length"
	case "tool_use", "tool_calls":
		return "tool_calls"
	}
	return value
}
func normalizeAnthropicFinish(value string) string {
	switch value {
	case "stop", "end_turn":
		return "end_turn"
	case "length", "max_tokens":
		return "max_tokens"
	case "tool_calls", "tool_use":
		return "tool_use"
	}
	return value
}
