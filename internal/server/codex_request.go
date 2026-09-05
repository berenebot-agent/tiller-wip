package server

import (
	"encoding/json"
	"strings"
)

// normalizeCodexRequest applies the small set of Responses adjustments that
// the ChatGPT Codex backend requires but the public Responses surface leaves
// optional. It intentionally does not log or return request contents.
func normalizeCodexRequest(body []byte) ([]byte, error) {
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	switch input := request["input"].(type) {
	case string:
		request["input"] = []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": input}}}}
	case nil:
		request["input"] = []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "..."}}}}
	}
	if input, ok := request["input"].([]any); ok {
		kept := make([]any, 0, len(input))
		var systemText []string
		for _, raw := range input {
			item, ok := raw.(map[string]any)
			role, _ := item["role"].(string)
			if !ok {
				kept = append(kept, raw)
				continue
			}
			if role == "assistant" {
				normalizeCodexAssistantContent(item["content"])
			}
			if role != "system" && role != "developer" {
				kept = append(kept, raw)
				continue
			}
			if text := codexInstructionText(item["content"]); text != "" {
				systemText = append(systemText, text)
			}
		}
		request["input"] = kept
		if len(systemText) > 0 {
			existing, _ := request["instructions"].(string)
			request["instructions"] = strings.TrimSpace(strings.Join(append(systemText, existing), "\n\n"))
		}
	}
	if input, ok := request["input"].([]any); ok && len(input) == 0 {
		request["input"] = []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "..."}}}}
	}
	request["stream"] = true
	request["store"] = false
	if instructions, ok := request["instructions"].(string); !ok || instructions == "" {
		request["instructions"] = "You are Codex, a coding assistant."
	}
	for _, key := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens", "temperature", "top_p", "metadata", "stream_options", "previous_response_id", "user"} {
		delete(request, key)
	}
	return json.Marshal(request)
}

func normalizeCodexAssistantContent(value any) {
	for _, item := range asSlice(value) {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if block["type"] == "input_text" {
			block["type"] = "output_text"
		}
	}
}

func codexInstructionText(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			if text := codexInstructionText(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		if text, ok := value["text"]; ok {
			return codexInstructionText(text)
		}
		if content, ok := value["content"]; ok {
			return codexInstructionText(content)
		}
	}
	return ""
}
