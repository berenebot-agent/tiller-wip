package server

import (
	"bufio"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/providers"
)

// responsesStreamToClient replays a captured Responses SSE body through the
// translator and returns the events the client would see.
func responsesStreamToClient(t *testing.T, s string, incoming providers.Protocol) string {
	t.Helper()
	state := &streamState{id: "c1", model: "client-model", reasoningIndex: -1, messageIndex: -1, toolIndex: -1}
	reader := bufio.NewReader(strings.NewReader(s))
	var out strings.Builder
	for {
		event, err := readSSEEvent(reader)
		if err != nil && len(event.Data) == 0 {
			break
		}
		if len(event.Data) > 0 && string(event.Data) != "[DONE]" {
			var payload map[string]any
			if json.Unmarshal(event.Data, &payload) == nil {
				evType, _ := payload["type"].(string)
				deltas, done := canonicalDeltas(evType, payload, providers.ProtocolResponses, state)
				for _, delta := range deltas {
					if delta.Kind == "error" {
						t.Fatalf("unexpected stream error for event %s", event.Name)
					}
					if err := writeTranslatedEvent(&out, incoming, state, delta); err != nil {
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
	return out.String()
}

func assertResponsesCompletionReasons(t *testing.T, s, chatFinish, messagesFinish string) {
	t.Helper()
	for _, tc := range []struct {
		protocol providers.Protocol
		field    string
		want     string
	}{
		{providers.ProtocolChat, "finish_reason", chatFinish},
		{providers.ProtocolMessages, "stop_reason", messagesFinish},
	} {
		t.Run(string(tc.protocol), func(t *testing.T) {
			out := responsesStreamToClient(t, s, tc.protocol)
			var reasons []string
			for _, line := range strings.Split(out, "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var payload map[string]any
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
					t.Fatal(err)
				}
				if tc.protocol == providers.ProtocolChat {
					for _, raw := range asSlice(payload["choices"]) {
						choice, _ := raw.(map[string]any)
						if reason, ok := choice[tc.field].(string); ok {
							reasons = append(reasons, reason)
						}
					}
				} else if delta, ok := payload["delta"].(map[string]any); ok {
					if reason, ok := delta[tc.field].(string); ok {
						reasons = append(reasons, reason)
					}
				}
			}
			if len(reasons) != 1 || reasons[0] != tc.want {
				t.Fatalf("%s = %v, want exactly [%s]", tc.field, reasons, tc.want)
			}
		})
	}
}

func TestResponsesTextCompletionReasons(t *testing.T) {
	t.Parallel()
	s := `data: {"type":"response.output_text.delta","delta":"Hello"}` + "\n\n" +
		`data: {"type":"response.completed","response":{}}` + "\n\n"
	assertResponsesCompletionReasons(t, s, "stop", "end_turn")
}

// argumentsOf collects every tool_call argument fragment the client received,
// keyed by the ordinal the encoder assigned. Concatenating per ordinal mirrors
// what a Chat client does with a streamed tool call.
func argumentsOf(t *testing.T, out string) map[float64]string {
	t.Helper()
	collected := map[float64]string{}
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		line = strings.TrimSpace(line)
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    float64 `json:"index"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatalf("translated chunk is not JSON: %v (%s)", err, line)
		}
		for _, choice := range chunk.Choices {
			for _, call := range choice.Delta.ToolCalls {
				collected[call.Index] += call.Function.Arguments
			}
		}
	}
	return collected
}

// TestResponsesParallelToolCallsCarryArguments is the regression for the
// parallel-call argument loss: a Responses upstream that answers with two tool
// calls emits no function_call_arguments.delta events at all, only
// function_call_arguments.done and output_item.done. The translator used to keep
// only the call name from those events, so every parallel call reached a Chat
// client with empty arguments — which a strict client rejects as malformed and
// a lenient one executes with defaults.
func TestResponsesParallelToolCallsCarryArguments(t *testing.T) {
	t.Parallel()
	s := `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":"","status":"in_progress"}}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc_1","arguments":"{\"path\": \"/etc/hostname\"}","type":"response.function_call_arguments.done"}` + "\n\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":"{\"path\": \"/etc/hostname\"}","status":"completed"}}` + "\n\n" +
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_2","call_id":"call_2","name":"read","arguments":"","status":"in_progress"}}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.done","output_index":1,"item_id":"fc_2","arguments":"{\"path\": \"/etc/hosts\"}","type":"response.function_call_arguments.done"}` + "\n\n" +
		`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_2","call_id":"call_2","name":"read","arguments":"{\"path\": \"/etc/hosts\"}","status":"completed"}}` + "\n\n" +
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":5}}}` + "\n\n"

	assertResponsesCompletionReasons(t, s, "tool_calls", "tool_use")
	args := argumentsOf(t, responsesStreamToClient(t, s, providers.ProtocolChat))
	if len(args) != 2 {
		t.Fatalf("expected 2 tool calls, got %d: %v", len(args), args)
	}
	if args[0] != `{"path": "/etc/hostname"}` {
		t.Fatalf("call 0 arguments = %q", args[0])
	}
	if args[1] != `{"path": "/etc/hosts"}` {
		t.Fatalf("call 1 arguments = %q", args[1])
	}
}

// TestResponsesStreamedArgumentsAreNotDuplicated is the other half of the
// contract: a call that DOES stream deltas must reach the client with those
// fragments, and a terminal payload arriving afterwards (the same arguments a
// second time) must not be appended to them. Upstreams normally emit deltas XOR
// the terminal payload; one that emits both must still yield exactly one copy,
// because `{...}{...}` is an argument string no client can parse.
func TestResponsesStreamedArgumentsAreNotDuplicated(t *testing.T) {
	t.Parallel()
	s := `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":"","status":"in_progress"}}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":"{\"path\":"}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":" \"/etc/hostname\"}"}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc_1","arguments":"{\"path\": \"/etc/hostname\"}"}` + "\n\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":"{\"path\": \"/etc/hostname\"}","status":"completed"}}` + "\n\n" +
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":5}}}` + "\n\n"

	assertResponsesCompletionReasons(t, s, "tool_calls", "tool_use")
	args := argumentsOf(t, responsesStreamToClient(t, s, providers.ProtocolChat))
	if len(args) != 1 {
		t.Fatalf("expected 1 tool call, got %d: %v", len(args), args)
	}
	// The client holds the fragments and then the terminal copy; both spell the
	// same arguments, so exactly one complete JSON value must be present.
	if strings.Count(args[0], `"path"`) != 1 {
		t.Fatalf("streamed arguments were duplicated: %q", args[0])
	}
	if !strings.Contains(args[0], "/etc/hostname") {
		t.Fatalf("streamed arguments were lost: %q", args[0])
	}
}

// TestResponsesParallelToolCallsAggregateNonstream covers the stream:false
// side of the same defect: an aggregated response (used when a Codex target
// forces stream:true upstream) must carry both parallel calls' arguments too.
func TestResponsesParallelToolCallsAggregateNonstream(t *testing.T) {
	t.Parallel()
	s := `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":"","status":"in_progress"}}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc_1","arguments":"{\"path\": \"/etc/hostname\"}"}` + "\n\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":"{\"path\": \"/etc/hostname\"}","status":"completed"}}` + "\n\n" +
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_2","call_id":"call_2","name":"read","arguments":"","status":"in_progress"}}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.done","output_index":1,"item_id":"fc_2","arguments":"{\"path\": \"/etc/hosts\"}"}` + "\n\n" +
		`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_2","call_id":"call_2","name":"read","arguments":"{\"path\": \"/etc/hosts\"}","status":"completed"}}` + "\n\n" +
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":5}}}` + "\n\n"

	state := &streamState{id: "c1", model: "client-model", reasoningIndex: -1, messageIndex: -1, toolIndex: -1}
	reader := bufio.NewReader(strings.NewReader(s))
	for {
		event, err := readSSEEvent(reader)
		if err != nil && len(event.Data) == 0 {
			break
		}
		if len(event.Data) > 0 && string(event.Data) != "[DONE]" {
			var payload map[string]any
			if json.Unmarshal(event.Data, &payload) == nil {
				evType, _ := payload["type"].(string)
				deltas, done := canonicalDeltas(evType, payload, providers.ProtocolResponses, state)
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
	encoded, err := json.Marshal(state.aggregatedNonstreamChat(providers.ProtocolChat, "client-model", "tool_calls", &usageCapture{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`/etc/hostname`, `/etc/hosts`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("aggregated response lost %s: %s", want, encoded)
		}
	}
}
