package server

import (
	"bufio"
	"io"
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/providers"
)

// TestTranslateNonstreamBodyRejectsMalformedBeforeCommit is the regression for
// the non-streaming false success: the translation must return an error for a
// malformed body so the caller can answer with an error instead of committing
// the upstream 2xx and then failing silently.
func TestTranslateNonstreamBodyRejectsMalformedBeforeCommit(t *testing.T) {
	usage := &usageCapture{}
	_, isJSON, err := translateNonstreamBody(
		bufio.NewReader(strings.NewReader(`{"choices":[`)),
		providers.ProtocolChat, providers.ProtocolMessages, "client-model", usage,
	)
	if !isJSON {
		t.Fatal("malformed JSON object was not classified as JSON")
	}
	if err == nil {
		t.Fatal("malformed JSON translated without error; caller would commit a 2xx with no body")
	}
}

// TestTranslateNonstreamBodyReturnsTranslatedBytes proves a valid body is
// translated to completion before any write, so the caller can commit the
// status and the body together.
func TestTranslateNonstreamBodyReturnsTranslatedBytes(t *testing.T) {
	usage := &usageCapture{}
	body := `{"id":"m1","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`
	translated, isJSON, err := translateNonstreamBody(
		bufio.NewReader(strings.NewReader(body)),
		providers.ProtocolMessages, providers.ProtocolChat, "client-model", usage,
	)
	if !isJSON || err != nil {
		t.Fatalf("isJSON=%v err=%v", isJSON, err)
	}
	if !strings.Contains(string(translated), `"type":"message"`) || !strings.Contains(string(translated), `"client-model"`) {
		t.Fatalf("translated body missing expected fields: %s", translated)
	}
	if usage.inputTokens == nil || *usage.inputTokens != 3 || usage.outputTokens == nil || *usage.outputTokens != 4 {
		t.Fatalf("usage not captured: %+v", usage)
	}
}

// TestTranslateNonstreamBodyPassesThroughSSE proves an SSE body is not
// consumed: the caller must be able to fall through to the streaming
// translator with every byte intact.
func TestTranslateNonstreamBodyPassesThroughSSE(t *testing.T) {
	usage := &usageCapture{}
	body := "data: {\"choices\":[]}\n\n"
	reader := bufio.NewReader(strings.NewReader(body))
	_, isJSON, err := translateNonstreamBody(reader, providers.ProtocolChat, providers.ProtocolMessages, "client-model", usage)
	if isJSON || err != nil {
		t.Fatalf("SSE classified as JSON: isJSON=%v err=%v", isJSON, err)
	}
	rest, _ := io.ReadAll(reader)
	if string(rest) != body {
		t.Fatalf("SSE bytes lost: got %q, want %q", rest, body)
	}
}
