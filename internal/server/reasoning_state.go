package server

import (
	"fmt"

	"github.com/tiller-router/tiller-router/internal/providers"
)

// reasoningStateError is deliberately content-free: reasoning signatures and
// encrypted state must never be included in errors or logs.
type reasoningStateError struct{ reason string }

func (e reasoningStateError) Error() string {
	return "reasoning state cannot be represented for this protocol"
}
func (e reasoningStateError) Unwrap() error { return unsupportedFeature{"reasoning state"} }

func anthropicReasoningToChatDetails(block map[string]any, index int) (map[string]any, error) {
	result := map[string]any{"format": "anthropic-claude-v1", "index": index}
	switch block["type"] {
	case "thinking":
		text, ok := block["thinking"].(string)
		signature, sigOK := block["signature"].(string)
		if !ok {
			return nil, reasoningStateError{"unsigned thinking"}
		}
		result["type"], result["text"], result["signature"] = "reasoning.text", text, signature
		if !sigOK || signature == "" {
			delete(result, "signature")
		}
	case "redacted_thinking":
		data, ok := block["data"].(string)
		if !ok || data == "" {
			return nil, reasoningStateError{"invalid redacted thinking"}
		}
		result["type"], result["data"] = "reasoning.encrypted", data
	default:
		return nil, reasoningStateError{"unknown thinking block"}
	}
	return result, nil
}

// chatReasoningDetailsToAnthropic converts only the Claude extension. Other
// OpenRouter formats (notably openai-responses-v1) cannot be presented as
// Anthropic signatures and are rejected.
func chatReasoningDetailsToAnthropic(value any) ([]any, error) {
	details, ok := value.([]any)
	if !ok {
		return nil, reasoningStateError{"invalid reasoning details"}
	}
	out := make([]any, 0, len(details))
	for i, raw := range details {
		detail, ok := raw.(map[string]any)
		if !ok || detail["format"] != "anthropic-claude-v1" {
			return nil, reasoningStateError{"foreign reasoning format"}
		}
		typ, _ := detail["type"].(string)
		block := map[string]any{}
		switch typ {
		case "reasoning.text":
			text, textOK := detail["text"].(string)
			sig, sigOK := detail["signature"].(string)
			if !textOK {
				return nil, reasoningStateError{"invalid reasoning text"}
			}
			block = map[string]any{"type": "thinking", "thinking": text, "signature": sig}
			if !sigOK || sig == "" {
				delete(block, "signature")
			}
		case "reasoning.encrypted":
			data, dataOK := detail["data"].(string)
			if !dataOK || data == "" {
				return nil, reasoningStateError{"invalid encrypted reasoning"}
			}
			block = map[string]any{"type": "redacted_thinking", "data": data}
		default:
			return nil, reasoningStateError{fmt.Sprintf("unsupported reasoning detail %d", i)}
		}
		out = append(out, block)
	}
	return out, nil
}

// responsesReasoningToChatDetails carries Responses' opaque reasoning through
// Chat without pretending it is an Anthropic signature. Summary text is also
// retained as a readable reasoning detail.
func responsesReasoningToChatDetails(item map[string]any) []any {
	out := []any{}
	format := "openai-responses-v1"
	if encrypted, ok := item["encrypted_content"].(string); ok && encrypted != "" {
		d := map[string]any{"type": "reasoning.encrypted", "data": encrypted, "format": format}
		if id, ok := item["id"].(string); ok {
			d["id"] = id
		}
		out = append(out, d)
	}
	for index, raw := range asSlice(item["summary"]) {
		summary, _ := raw.(map[string]any)
		text, _ := summary["text"].(string)
		if text == "" {
			continue
		}
		d := map[string]any{"type": "reasoning.summary", "summary": text, "format": format, "index": index}
		if id, ok := item["id"].(string); ok {
			d["id"] = id
		}
		out = append(out, d)
	}
	return out
}

func chatReasoningDetailsToResponses(value any) ([]any, error) {
	details, ok := value.([]any)
	if !ok {
		return nil, reasoningStateError{"invalid reasoning details"}
	}
	out := []any{}
	byID := map[string]map[string]any{}
	seenID := map[string]bool{}
	for _, raw := range details {
		d, ok := raw.(map[string]any)
		if !ok || d["format"] != "openai-responses-v1" {
			return nil, reasoningStateError{"foreign reasoning format"}
		}
		item := map[string]any{"type": "reasoning"}
		id, _ := d["id"].(string)
		if id != "" {
			if prior := byID[id]; prior != nil {
				item = prior
			} else {
				item["id"] = id
				byID[id] = item
			}
		}
		switch d["type"] {
		case "reasoning.encrypted":
			data, ok := d["data"].(string)
			if !ok || data == "" {
				return nil, reasoningStateError{"invalid encrypted reasoning"}
			}
			item["encrypted_content"] = data
			if _, exists := item["summary"]; !exists {
				item["summary"] = []any{}
			}
		case "reasoning.summary":
			text, ok := d["summary"].(string)
			if !ok {
				return nil, reasoningStateError{"invalid reasoning summary"}
			}
			summary, _ := item["summary"].([]any)
			item["summary"] = append(summary, map[string]any{"type": "summary_text", "text": text})
		default:
			return nil, reasoningStateError{"unsupported reasoning detail"}
		}
		if id == "" {
			out = append(out, item)
		} else if !seenID[id] {
			out = append(out, item)
			seenID[id] = true
		}
	}
	return out, nil
}

func validateReasoningRepresentability(source map[string]any, from, to providers.Protocol) error {
	if from == to {
		return nil
	}
	if from == providers.ProtocolResponses {
		if to == providers.ProtocolMessages {
			for _, raw := range asSlice(source["input"]) {
				item, _ := raw.(map[string]any)
				if item["type"] == "reasoning" && item["encrypted_content"] != nil {
					return reasoningStateError{"Responses reasoning state"}
				}
			}
		}
	}
	if from == providers.ProtocolChat && to == providers.ProtocolResponses {
		for _, raw := range asSlice(source["messages"]) {
			msg, _ := raw.(map[string]any)
			if details, ok := msg["reasoning_details"].([]any); ok {
				for _, rawDetail := range details {
					detail, _ := rawDetail.(map[string]any)
					if detail["format"] == "anthropic-claude-v1" {
						return reasoningStateError{"Claude reasoning state"}
					}
				}
			}
		}
	}
	return nil
}
