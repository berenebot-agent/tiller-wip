package server

import (
	"errors"
	"testing"

	"github.com/tiller-router/tiller-router/internal/providers"
)

func TestValidateResponseReasoningState_AllProtocolDirections(t *testing.T) {
	claudeDetails := []any{map[string]any{"type": "reasoning.text", "text": "plan", "signature": "sig", "format": "anthropic-claude-v1"}}
	openAIDetails := []any{map[string]any{"type": "reasoning.encrypted", "data": "opaque", "format": "openai-responses-v1"}}
	tests := []struct {
		name    string
		source  map[string]any
		from    providers.Protocol
		to      providers.Protocol
		wantErr bool
	}{
		{"Messages to Chat accepts signed Claude state", map[string]any{"content": []any{map[string]any{"type": "thinking", "thinking": "plan", "signature": "sig"}}}, providers.ProtocolMessages, providers.ProtocolChat, false},
		{"Messages to Responses rejects signed state", map[string]any{"content": []any{map[string]any{"type": "thinking", "thinking": "plan", "signature": "sig"}}}, providers.ProtocolMessages, providers.ProtocolResponses, true},
		{"Responses to Chat accepts encrypted state", map[string]any{"output": []any{map[string]any{"type": "reasoning", "encrypted_content": "opaque"}}}, providers.ProtocolResponses, providers.ProtocolChat, false},
		{"Responses to Messages rejects encrypted state", map[string]any{"output": []any{map[string]any{"type": "reasoning", "encrypted_content": "opaque"}}}, providers.ProtocolResponses, providers.ProtocolMessages, true},
		{"Chat Claude state to Messages accepted", map[string]any{"choices": []any{map[string]any{"message": map[string]any{"reasoning_details": claudeDetails}}}}, providers.ProtocolChat, providers.ProtocolMessages, false},
		{"Chat OpenAI state to Responses accepted", map[string]any{"choices": []any{map[string]any{"message": map[string]any{"reasoning_details": openAIDetails}}}}, providers.ProtocolChat, providers.ProtocolResponses, false},
		{"Chat Claude state to Responses rejected", map[string]any{"choices": []any{map[string]any{"message": map[string]any{"reasoning_details": claudeDetails}}}}, providers.ProtocolChat, providers.ProtocolResponses, true},
		{"Chat OpenAI state to Messages rejected", map[string]any{"choices": []any{map[string]any{"message": map[string]any{"reasoning_details": openAIDetails}}}}, providers.ProtocolChat, providers.ProtocolMessages, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateResponseReasoningState(tc.source, tc.from, tc.to)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil {
				var stateErr reasoningStateError
				if !errors.As(err, &stateErr) {
					t.Fatalf("error = %T, want reasoningStateError", err)
				}
				if stateErr.Error() != "reasoning state cannot be represented for this protocol" {
					t.Fatalf("error exposed state details: %q", stateErr.Error())
				}
			}
		})
	}
}

func TestValidateResponseReasoningState_PlainTextAndSummaryArePortable(t *testing.T) {
	plainMessages := map[string]any{"content": []any{map[string]any{"type": "thinking", "thinking": "plain"}}}
	if err := validateResponseReasoningState(plainMessages, providers.ProtocolMessages, providers.ProtocolResponses); err != nil {
		t.Fatalf("plain Messages thinking rejected: %v", err)
	}
	summary := map[string]any{"output": []any{map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "plan"}}}}}
	if err := validateResponseReasoningState(summary, providers.ProtocolResponses, providers.ProtocolMessages); err != nil {
		t.Fatalf("Responses summary rejected: %v", err)
	}
}
