package server

import (
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/providers"
)

type failedTranslationStreamReader struct {
	err error
}

func (r failedTranslationStreamReader) Read([]byte) (int, error) {
	return 0, r.err
}

func TestTranslatedStreamFailureClassification(t *testing.T) {
	readErr := errors.New("synthetic transport failure")
	cases := []struct {
		name   string
		reader io.Reader
		want   string
	}{
		{
			name:   "explicit failure",
			reader: strings.NewReader("event: response.failed\ndata: {\"type\":\"response.failed\"}\n\n"),
			want:   "upstream_stream_error",
		},
		{
			name:   "error event",
			reader: strings.NewReader("event: error\ndata: {\"type\":\"error\"}\n\n"),
			want:   "upstream_stream_error",
		},
		{
			name:   "incomplete response",
			reader: strings.NewReader("event: response.incomplete\ndata: {\"type\":\"response.incomplete\"}\n\n"),
			want:   "upstream_stream_incomplete",
		},
		{
			name:   "premature EOF",
			reader: strings.NewReader("event: response.created\ndata: {\"type\":\"response.created\"}\n\n"),
			want:   "upstream_stream_eof",
		},
		{
			name:   "empty stream",
			reader: strings.NewReader(""),
			want:   "upstream_stream_eof",
		},
		{
			name:   "unexpected EOF",
			reader: failedTranslationStreamReader{err: io.ErrUnexpectedEOF},
			want:   "upstream_stream_eof",
		},
		{
			name:   "read failure before first byte",
			reader: failedTranslationStreamReader{err: readErr},
			want:   "upstream_read_error",
		},
		{
			name: "read failure after event",
			reader: io.MultiReader(
				strings.NewReader("event: response.created\ndata: {\"type\":\"response.created\"}\n\n"),
				failedTranslationStreamReader{err: readErr},
			),
			want: "upstream_read_error",
		},
		{
			name:   "invalid JSON translation",
			reader: strings.NewReader("{invalid"),
			want:   "translation_error",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := translateResponseObserved(httptest.NewRecorder(), nil, tc.reader,
				providers.ProtocolChat, providers.ProtocolResponses,
				resolvedRoute{RequestedModel: "test-model"}, &usageCapture{}, nil)
			if err == nil {
				t.Fatal("expected a stream failure")
			}
			if got := translatedStreamFailureClass(fmt.Errorf("wrapped: %w", err)); got != tc.want {
				t.Fatalf("failure class = %q, want %q", got, tc.want)
			}
			if tc.want == "upstream_read_error" && !errors.Is(err, readErr) {
				t.Fatal("underlying read error was not preserved")
			}
			if fixedUpstreamErrorMessage(tc.want) == "" {
				t.Fatal("failure class has no fixed safe message")
			}
		})
	}
}

func TestTranslatedStreamCompletionUnchanged(t *testing.T) {
	for _, terminal := range []string{
		"event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n",
		"data: [DONE]\n\n",
	} {
		w := httptest.NewRecorder()
		stream := "event: response.output_text.delta\ndata: {\"delta\":\"synthetic output\"}\n\n" + terminal
		err := translateResponseObserved(w, nil, strings.NewReader(stream),
			providers.ProtocolChat, providers.ProtocolResponses,
			resolvedRoute{RequestedModel: "test-model"}, &usageCapture{}, nil)
		if err != nil {
			t.Fatalf("completed stream failed: %v", err)
		}
		if !strings.Contains(w.Body.String(), "synthetic output") || !strings.Contains(w.Body.String(), "[DONE]") {
			t.Fatal("completed stream lost output or terminal frame")
		}
	}
}
