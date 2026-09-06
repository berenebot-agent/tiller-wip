package server

import (
	"bufio"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/providers"
)

func translatedSSE(t *testing.T, input string, incoming, target providers.Protocol) string {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := translateSSE(rec, bufio.NewReader(strings.NewReader(input)), incoming, target, "client-model", nil); err != nil {
		t.Fatalf("translateSSE: %v", err)
	}
	return rec.Body.String()
}

func TestTranslateMessagesReasoningFirstAllocatesIndexZero(t *testing.T) {
	s := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\"}}\n\n" +
		"event: content_block_start\ndata: {\"content_block\":{\"type\":\"thinking\"}}\n\n" +
		"event: content_block_delta\ndata: {\"delta\":{\"thinking\":\"plan\"}}\n\n" +
		"event: content_block_start\ndata: {\"content_block\":{\"type\":\"text\"}}\n\n" +
		"event: content_block_delta\ndata: {\"delta\":{\"text\":\"answer\"}}\n\n" +
		"event: message_stop\ndata: {}\n\n"
	out := translatedSSE(t, s, providers.ProtocolMessages, providers.ProtocolMessages)
	t.Logf("output: %s", out)
	if !strings.Contains(out, `"index":0`) || !strings.Contains(out, `"type":"thinking"`) {
		t.Fatalf("reasoning-first indices invalid: %s", out)
	}
	if !strings.Contains(out, `"index":1`) || !strings.Contains(out, `"type":"text"`) {
		t.Fatalf("reasoning/text delta indices invalid: %s", out)
	}
}

func TestTranslateResponsesReasoningOnlyCompletedOutput(t *testing.T) {
	s := "event: response.created\ndata: {\"response\":{\"id\":\"r1\"}}\n\n" +
		"event: response.reasoning_summary_text.delta\ndata: {\"delta\":\"thinking\"}\n\n" +
		"event: response.completed\ndata: {\"response\":{}}\n\n"
	out := translatedSSE(t, s, providers.ProtocolResponses, providers.ProtocolResponses)
	if !strings.Contains(out, `response.reasoning_summary_part.added`) || !strings.Contains(out, `response.reasoning_summary_text.delta`) {
		t.Fatalf("missing reasoning summary lifecycle: %s", out)
	}
	if !strings.Contains(out, `"type":"reasoning"`) || !strings.Contains(out, `"text":"thinking"`) {
		t.Fatalf("completed reasoning item missing: %s", out)
	}
	if strings.Contains(out, `response.output_text.done`) {
		t.Fatalf("reasoning-only response emitted text item: %s", out)
	}
}

func TestTranslateResponsesInterleavedReasoningTextUsesStableIndices(t *testing.T) {
	s := "event: response.created\ndata: {\"response\":{\"id\":\"r2\"}}\n\n" +
		"event: response.reasoning_summary_text.delta\ndata: {\"delta\":\"a\"}\n\n" +
		"event: response.output_text.delta\ndata: {\"delta\":\"b\"}\n\n" +
		"event: response.reasoning_summary_text.delta\ndata: {\"delta\":\"c\"}\n\n" +
		"event: response.output_text.delta\ndata: {\"delta\":\"d\"}\n\n" +
		"event: response.completed\ndata: {\"response\":{}}\n\n"
	out := translatedSSE(t, s, providers.ProtocolResponses, providers.ProtocolResponses)
	if strings.Count(out, `"output_index":0`) == 0 || strings.Count(out, `"output_index":1`) == 0 {
		t.Fatalf("expected distinct reasoning/message indices: %s", out)
	}
	if !strings.Contains(out, `"output_index":0,"summary_index":0,"delta":"c"`) && !strings.Contains(out, `"delta":"c","item_id":"rs_r2","output_index":0,"summary_index":0`) {
		t.Fatalf("interleaved deltas changed indices: %s", out)
	}
}

func TestTranslateNonstreamChatReasoningAlias(t *testing.T) {
	body := []byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"answer","reasoning":"plan"}}]}`)
	out, err := translateNonstreamResponse(body, providers.ProtocolChat, providers.ProtocolChat, "m")
	if err != nil || !strings.Contains(string(out), `"reasoning_content":"plan"`) || !strings.Contains(string(out), `"content":"answer"`) {
		t.Fatalf("reasoning alias not normalized: %s (%v)", out, err)
	}
	body = []byte(`{"id":"c2","choices":[{"message":{"role":"assistant","content":"answer","reasoning":"alias","reasoning_content":"canonical"}}]}`)
	out, err = translateNonstreamResponse(body, providers.ProtocolChat, providers.ProtocolChat, "m")
	if err != nil || strings.Contains(string(out), `"reasoning_content":"alias"`) || !strings.Contains(string(out), `"reasoning_content":"canonical"`) {
		t.Fatalf("canonical reasoning was duplicated/overwritten: %s (%v)", out, err)
	}
}

