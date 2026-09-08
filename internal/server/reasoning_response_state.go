package server

import "github.com/tiller-router/tiller-router/internal/providers"

// validateResponseReasoningState checks whether opaque provider reasoning in
// an upstream response can survive conversion to the requested protocol.
// Plain text thinking and Responses summaries are portable; signed or
// encrypted state is rejected when the target cannot round-trip it.
func validateResponseReasoningState(source map[string]any, from, to providers.Protocol) error {
	if from == to {
		return nil
	}
	switch {
	case from == providers.ProtocolMessages && to == providers.ProtocolResponses:
		for _, raw := range asSlice(source["content"]) {
			block, _ := raw.(map[string]any)
			if block["type"] == "redacted_thinking" {
				return reasoningStateError{"Messages redacted thinking"}
			}
			if block["type"] == "thinking" {
				if signature, ok := block["signature"].(string); ok && signature != "" {
					return reasoningStateError{"Messages signed thinking"}
				}
			}
		}
	case from == providers.ProtocolResponses && to == providers.ProtocolMessages:
		for _, raw := range asSlice(source["output"]) {
			item, _ := raw.(map[string]any)
			if item["type"] == "reasoning" {
				if encrypted, ok := item["encrypted_content"].(string); ok && encrypted != "" {
					return reasoningStateError{"Responses encrypted reasoning"}
				}
			}
		}
	case from == providers.ProtocolChat:
		for _, raw := range asSlice(source["choices"]) {
			choice, _ := raw.(map[string]any)
			message, _ := choice["message"].(map[string]any)
			details, ok := message["reasoning_details"]
			if !ok {
				continue
			}
			var err error
			switch to {
			case providers.ProtocolMessages:
				_, err = chatReasoningDetailsToAnthropic(details)
			case providers.ProtocolResponses:
				_, err = chatReasoningDetailsToResponses(details)
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}
