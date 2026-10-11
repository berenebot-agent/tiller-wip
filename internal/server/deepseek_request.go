package server

import "encoding/json"

// normalizeDeepSeekRequest rewrites the provider-neutral reasoning selector
// shapes the router emits into DeepSeek's OpenAI-format thinking controls.
//
// DeepSeek's Chat surface does not read OpenRouter's `reasoning.enabled`
// object; its thinking controls are `thinking: {"type": "enabled"|"disabled"}`
// plus the top-level `reasoning_effort` field. The router's toggle mapping
// (applyChatReasoning) emits `reasoning: {"enabled": false}` for a target that
// advertises a toggle but no `none` effort, which DeepSeek silently ignores —
// leaving thinking mode on. Because thinking mode is on by default and, when a
// request carries tools, DeepSeek requires every prior assistant turn's
// reasoning_content to be passed back, an ignored disable turns an ordinary
// tool-carrying request into a 400. Mapping the toggle onto the real wire field
// makes the router's disable actually take effect.
//
// It intentionally leaves a valid `reasoning_effort` (low/high/max) untouched,
// and it never logs or returns request contents.
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

	if !changed {
		return body, nil
	}
	return json.Marshal(request)
}
