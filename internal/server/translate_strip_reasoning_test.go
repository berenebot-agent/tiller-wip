package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/providers"
)

// A chat switched from a Chat model to Claude arrives with assistant turns that
// carry reasoning_content (or foreign reasoning_details) plus tool_calls. That
// state cannot become a signed Anthropic thinking block, so the turn must be
// sent without it instead of failing the whole request.
func TestChatToMessagesSendsToolTurnWithoutUnverifiableReasoning(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "user", "content": "run the tool"},
			map[string]any{"role": "assistant", "content": "", "reasoning_content": "pondering", "tool_calls": []any{
				map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "f", "arguments": `{"a":1}`}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": "result"},
			map[string]any{"role": "assistant", "content": "done", "reasoning_details": []any{
				map[string]any{"type": "reasoning.encrypted", "data": "opaque", "format": "openai-responses-v1"},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, stripped, err := translateRequestObserved(body, providers.ProtocolChat, providers.ProtocolMessages, "claude-x")
	if err != nil {
		t.Fatalf("cross-provider history was refused: %v", err)
	}
	if !stripped {
		t.Fatal("degradation was not reported to the caller")
	}
	if strings.Contains(string(out), "pondering") || strings.Contains(string(out), "opaque") {
		t.Fatalf("unverifiable reasoning leaked into the Messages body: %s", out)
	}
	var parsed struct {
		Messages []struct {
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	if got := parsed.Messages[1].Content; len(got) != 1 || got[0].Type != "tool_use" {
		t.Fatalf("tool turn = %+v, want only tool_use", got)
	}
	if got := parsed.Messages[3].Content; len(got) != 1 || got[0].Type != "text" {
		t.Fatalf("assistant turn = %+v, want only text", got)
	}
}

// A Claude-formatted history must still replay its signed thinking: stripping
// is for state the target cannot verify, not for state it issued.
func TestChatToMessagesKeepsSignedClaudeReasoning(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "done", "reasoning_details": []any{
				map[string]any{"type": "reasoning.text", "text": "plan", "signature": "sig", "format": "anthropic-claude-v1"},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, stripped, err := translateRequestObserved(body, providers.ProtocolChat, providers.ProtocolMessages, "claude-x")
	if err != nil {
		t.Fatalf("signed Claude history was refused: %v", err)
	}
	if stripped {
		t.Fatal("signed Claude reasoning was reported as stripped")
	}
	if !strings.Contains(string(out), `"signature":"sig"`) || !strings.Contains(string(out), `"type":"thinking"`) {
		t.Fatalf("signed thinking was lost: %s", out)
	}
}

// The refusal message must not claim a Responses-only restriction for a
// Messages target.
func TestUnsupportedFeatureErrorNamesTheRealLimit(t *testing.T) {
	t.Parallel()
	if got := (unsupportedFeature{"reasoning state"}).Error(); strings.Contains(got, "Responses") {
		t.Fatalf("reasoning state blamed the Responses relay: %q", got)
	}
	if got := (unsupportedFeature{"conversation"}).Error(); !strings.Contains(got, "Responses") {
		t.Fatalf("Responses-only feature lost its real limit: %q", got)
	}
}
