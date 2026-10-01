package server

import (
	"bufio"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/providers"
)

// These tests cover the streaming counterpart of the non-streaming opaque
// reasoning-state preservation: a signature or encrypted blob that arrives on
// the upstream stream must reach the client so a reasoning/tool continuation can
// replay it. Before this was wired, only the readable thinking text survived.

// TestStreamedChatReasoningDetailsReachAnthropicClient covers the OpenRouter /
// Chat-upstream case for an Anthropic client: Claude reasoning arrives as
// reasoning_details with a signature, and the client must receive a thinking
// block carrying that signature.
func TestStreamedChatReasoningDetailsReachAnthropicClient(t *testing.T) {
	s := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning\":\"plan\",\"reasoning_details\":[{\"type\":\"reasoning.text\",\"text\":\"plan\",\"signature\":\"sig-abc\",\"format\":\"anthropic-claude-v1\"}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	out := translatedSSE(t, s, providers.ProtocolMessages, providers.ProtocolChat)

	if !strings.Contains(out, `"type":"thinking"`) {
		t.Fatalf("no thinking block emitted: %s", out)
	}
	if !strings.Contains(out, `"signature":"sig-abc"`) || !strings.Contains(out, `"type":"signature_delta"`) {
		t.Fatalf("signature did not reach the Anthropic client: %s", out)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("text after reasoning lost: %s", out)
	}
}

// TestStreamedChatReasoningDetailsReachResponsesClient covers the same upstream
// shape for a Responses client, where the state must appear as
// encrypted_content on the reasoning item.
func TestStreamedChatReasoningDetailsReachResponsesClient(t *testing.T) {
	s := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning\":\"plan\",\"reasoning_details\":[{\"type\":\"reasoning.encrypted\",\"data\":\"opaque-blob\",\"format\":\"openai-responses-v1\"}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	out := translatedSSE(t, s, providers.ProtocolResponses, providers.ProtocolChat)

	if !strings.Contains(out, `"encrypted_content":"opaque-blob"`) {
		t.Fatalf("encrypted reasoning state did not reach the Responses client: %s", out)
	}
	if !strings.Contains(out, `response.completed`) {
		t.Fatalf("stream did not complete: %s", out)
	}
}

// TestStreamedChatReasoningDetailsReachChatClient verifies the same-protocol
// shape is passed through rather than converted.
func TestStreamedChatReasoningDetailsReachChatClient(t *testing.T) {
	s := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning\":\"plan\",\"reasoning_details\":[{\"type\":\"reasoning.encrypted\",\"data\":\"blob\",\"format\":\"openai-responses-v1\"}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	out := translatedSSE(t, s, providers.ProtocolChat, providers.ProtocolChat)

	if !strings.Contains(out, `"reasoning_details"`) || !strings.Contains(out, "blob") {
		t.Fatalf("reasoning_details not passed to the Chat client: %s", out)
	}
}

// TestStreamedMessagesSignatureReachesChatClient covers an Anthropic upstream
// feeding a Chat client: the thinking signature arrives as its own
// signature_delta and must become reasoning_details the request path can replay.
func TestStreamedMessagesSignatureReachesChatClient(t *testing.T) {
	s := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\"}}\n\n" +
		"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"plan\"}}\n\n" +
		"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig-xyz\"}}\n\n" +
		"event: content_block_stop\ndata: {\"index\":0}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	out := translatedSSE(t, s, providers.ProtocolChat, providers.ProtocolMessages)

	if !strings.Contains(out, `"reasoning_details"`) || !strings.Contains(out, "sig-xyz") {
		t.Fatalf("Messages signature did not reach the Chat client: %s", out)
	}
}

// TestStreamedResponsesEncryptedReasoningReachesChatClient covers a Responses
// upstream feeding a Chat client: the encrypted reasoning item must become
// reasoning_details so it can be replayed.
func TestStreamedResponsesEncryptedReasoningReachesChatClient(t *testing.T) {
	s := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\"}}\n\n" +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"encrypted_content\":\"enc-123\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"plan\"}]}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\"}}\n\n"
	out := translatedSSE(t, s, providers.ProtocolChat, providers.ProtocolResponses)

	if !strings.Contains(out, `"reasoning_details"`) || !strings.Contains(out, "enc-123") {
		t.Fatalf("Responses encrypted reasoning did not reach the Chat client: %s", out)
	}
	if !strings.Contains(out, "plan") {
		t.Fatalf("reasoning summary text lost: %s", out)
	}
}

// TestAggregatedStreamPreservesReasoningDetails covers the stream:false
// aggregation path (used when a target forces stream:true upstream, e.g.
// Codex): the accumulated opaque state must appear in the single JSON response.
func TestAggregatedStreamPreservesReasoningDetails(t *testing.T) {
	s := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning\":\"plan\",\"reasoning_details\":[{\"type\":\"reasoning.encrypted\",\"data\":\"agg-blob\",\"format\":\"openai-responses-v1\"}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"

	usage := &usageCapture{}
	state := &streamState{id: "c1", model: "client-model", reasoningIndex: -1, messageIndex: -1, toolIndex: -1}
	reader := bufio.NewReader(strings.NewReader(s))
	for {
		event, err := readSSEEvent(reader)
		if len(event.Data) > 0 && string(event.Data) != "[DONE]" {
			var payload map[string]any
			if json.Unmarshal(event.Data, &payload) == nil {
				deltas, done := canonicalDeltas(event.Name, payload, providers.ProtocolChat, state)
				for _, delta := range deltas {
					if delta.Kind == "error" {
						t.Fatal("unexpected stream error")
					}
					if err := state.applyDelta(delta); err != nil {
						t.Fatal(err)
					}
				}
				if done {
					break
				}
			}
		}
		if err != nil {
			break
		}
	}
	body := state.aggregatedNonstreamChat(providers.ProtocolResponses, "client-model", "stop", usage)
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "agg-blob") {
		t.Fatalf("aggregated response lost opaque reasoning state: %s", encoded)
	}
}

// TestStreamedIncompatibleReasoningStateFailsLoudly proves an incompatible
// direction fails the stream rather than dropping the state silently: a Chat
// Claude signature cannot be represented to a Responses client, matching the
// non-streaming gate.
func TestStreamedIncompatibleReasoningStateFailsLoudly(t *testing.T) {
	s := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning\":\"plan\",\"reasoning_details\":[{\"type\":\"reasoning.text\",\"text\":\"plan\",\"signature\":\"sig\",\"format\":\"anthropic-claude-v1\"}]}}]}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	err := translateSSE(rec, nil, bufio.NewReader(strings.NewReader(s)), providers.ProtocolResponses, providers.ProtocolChat, "client-model", nil)
	if err == nil {
		t.Fatal("incompatible reasoning state streamed without error")
	}
	// The failure must carry no state content.
	if strings.Contains(err.Error(), "sig") {
		t.Fatalf("error leaked reasoning state: %v", err)
	}
}
