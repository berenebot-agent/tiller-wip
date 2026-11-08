package server

import "encoding/json"

// normalizeDeepSeekRequest applies the provider-scoped compatibility rules that
// DeepSeek's OpenAI-compatible Chat surface requires. It intentionally never
// logs or returns request contents.
//
// Two independent DeepSeek behaviours are handled:
//
//  1. reasoning_content round-trip. DeepSeek's thinking mode is on by default,
//     and when a request carries `tools` it requires the `reasoning_content` of
//     every prior assistant turn to be passed back (its documented tool-call
//     contract: https://api-docs.deepseek.com/guides/thinking_mode). A history
//     assembled by a *different* model — the normal case when a client switches
//     models mid-conversation — has no reasoning_content at all, so the request
//     is rejected with
//     `The reasoning_content in the thinking mode must be passed back to the
//     API` (HTTP 400). Disabling thinking does not avoid the requirement
//     (verified against the live API). Tiller's job is to make a foreign history
//     routable, so each assistant turn missing reasoning_content is back-filled
//     with an empty string, which satisfies the wire contract without
//     fabricating chain-of-thought or altering the client's requested effort.
//     Only applied when `tools` is present — that is the condition that triggers
//     the requirement.
//
//  2. Role vocabulary. DeepSeek's Chat surface accepts only
//     system/user/assistant/tool and rejects OpenAI's `developer` role with
//     HTTP 422 ("messages[0].role: unknown variant 'developer'") before
//     inference runs. `developer` is OpenAI's higher-precedence replacement for
//     `system`, so the rename is semantic-preserving; an OpenAI-shaped client
//     (the pi harness writes its system prompt there) cannot know which
//     chat-compatible upstream is strict, and a same-protocol Chat forward is
//     otherwise relayed verbatim. Message order and content are preserved — only
//     the role token changes.
//
//  3. Toggle mapping. DeepSeek does not read OpenRouter's `reasoning.enabled`
//     object; its OpenAI-format thinking controls are
//     `thinking: {"type": "enabled"|"disabled"}` plus top-level
//     `reasoning_effort`. The router's toggle mapping (applyChatReasoning) emits
//     `reasoning: {"enabled": false}` for a target that advertises a toggle but
//     no `none` effort, which DeepSeek silently ignores. Mapping it onto the
//     native field keeps the router's own selector faithful to the wire
//     surface. A valid `reasoning_effort` (low/high/max) is left untouched.
func normalizeDeepSeekRequest(body []byte) ([]byte, error) {
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	changed := false

	if reasoning, ok := request["reasoning"].(map[string]any); ok {
		if enabled, ok := reasoning["enabled"].(bool); ok {
			thinking, _ := request["thinking"].(map[string]any)
			if thinking == nil {
				thinking = map[string]any{}
			}
			if enabled {
				thinking["type"] = "enabled"
			} else {
				thinking["type"] = "disabled"
			}
			request["thinking"] = thinking
			changed = true
		}
		// The nested object is an OpenRouter/OpenAI-compat extension DeepSeek
		// does not read; drop it once its intent has been expressed natively.
		delete(request, "reasoning")
		changed = true
	}

	// `reasoning_effort: "none"` is not a DeepSeek effort value; the disable is
	// expressed with thinking.type instead.
	if effort, ok := request["reasoning_effort"].(string); ok && effort == "none" {
		thinking, _ := request["thinking"].(map[string]any)
		if thinking == nil {
			thinking = map[string]any{}
		}
		thinking["type"] = "disabled"
		request["thinking"] = thinking
		delete(request, "reasoning_effort")
		changed = true
	}

	if backfillDeepSeekReasoningContent(request) {
		changed = true
	}

	if normalizeDeepSeekRoles(request) {
		changed = true
	}

	if !changed {
		return body, nil
	}
	return json.Marshal(request)
}

// normalizeDeepSeekRoles rewrites OpenAI's `developer` role onto `system`, which
// is the role DeepSeek accepts for the same message. It returns whether it
// changed the request. Non-object entries and every other role are left alone,
// so a body that never carried `developer` is returned unchanged and the caller
// keeps the original bytes.
func normalizeDeepSeekRoles(request map[string]any) bool {
	messages, ok := request["messages"].([]any)
	if !ok {
		return false
	}
	changed := false
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if message["role"] == "developer" {
			message["role"] = "system"
			changed = true
		}
	}
	return changed
}

// backfillDeepSeekReasoningContent fills a missing `reasoning_content` on every
// assistant message with an empty string when the request carries tools, so a
// history built by another model still satisfies DeepSeek's tool-call
// reasoning_content round-trip requirement. It returns whether it changed the
// request. It is a no-op without tools, and it never overwrites an existing
// (including empty-string) value.
func backfillDeepSeekReasoningContent(request map[string]any) bool {
	tools, ok := request["tools"].([]any)
	if !ok || len(tools) == 0 {
		return false
	}
	messages, ok := request["messages"].([]any)
	if !ok {
		return false
	}
	changed := false
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok || message["role"] != "assistant" {
			continue
		}
		if _, exists := message["reasoning_content"]; exists {
			continue
		}
		// Foreign reasoning_details (including opaque/encrypted blocks) do
		// not satisfy DeepSeek's native reasoning_content contract.
		message["reasoning_content"] = ""
		changed = true
	}
	return changed
}
