package server

import (
	"encoding/json"
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
