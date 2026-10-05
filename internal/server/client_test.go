package server

import (
	"bytes"
	"io"
	"net/http"
	"testing"
)

func TestUpstreamHTTPFailuresAreFallbackEligible(t *testing.T) {
	for _, status := range []int{0, 199, 400, 401, 403, 404, 409, 422, 429, 500, 502, 503, 504, 599} {
		if !fallbackStatus(status) {
			t.Errorf("fallbackStatus(%d) = false, want true", status)
		}
	}
	for _, status := range []int{200, 201, 204, 299} {
		if fallbackStatus(status) {
			t.Errorf("fallbackStatus(%d) = true, want false", status)
		}
	}
}

func TestAllAttemptsFailureClass(t *testing.T) {
	base := []requestAttempt{
		{result: "failed", failureClass: "context_limit_exceeded"},
		{result: "failed", failureClass: "context_limit_exceeded"},
	}
	if !allAttemptsFailureClass(base, "context_limit_exceeded") {
		t.Fatal("expected all context-limit attempts to match")
	}
	base[1].failureClass = "upstream_timeout"
	if allAttemptsFailureClass(base, "context_limit_exceeded") {
		t.Fatal("mixed failure classes must not match")
	}
}

func TestStreamingMetadataUsesActualSSEResponse(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		ct     string
		body   string
		want   bool
	}{
		{name: "translated JSON", status: http.StatusOK, ct: "application/json", body: `{"choices":[]}`, want: false},
		{name: "translated SSE", status: http.StatusOK, ct: "text/event-stream", body: "data: {}\n\n", want: true},
		{name: "JSON with stream request", status: http.StatusOK, ct: "application/json", body: `{"choices":[]}`, want: false},
		{name: "headerless event SSE", status: http.StatusOK, body: "event: response.created\ndata: {}\n\n", want: true},
		{name: "headerless data SSE", status: http.StatusOK, body: "data: {\"choices\":[]}\n\n", want: true},
		{name: "headerless JSON", status: http.StatusOK, body: `{"choices":[]}`, want: false},
		{name: "declared JSON wins", status: http.StatusOK, ct: "application/json", body: "event: response.created\ndata: {}\n\n", want: false},
		{name: "error response is not sniffed", status: http.StatusBadGateway, body: "event: response.failed\ndata: {}\n\n", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			resp := &http.Response{
				StatusCode:    test.status,
				ContentLength: -1,
				Header:        make(http.Header),
				Body:          io.NopCloser(bytes.NewBufferString(test.body)),
			}
			if test.ct != "" {
				resp.Header.Set("Content-Type", test.ct)
			}
			defer resp.Body.Close()
			sniffAndClassify(resp)
			got := isStreamingResponse(resp)
			if got != test.want {
				t.Fatalf("isStreamingResponse(%q, %q) = %v, want %v", test.ct, test.body, got, test.want)
			}
		})
	}
}

func TestSniffAndClassifyPreservesResponseBytes(t *testing.T) {
	for _, body := range []string{
		"event: response.created\ndata: {}\n\n",
		"data: {\"choices\":[]}\n\n",
		`{"choices":[]}`,
	} {
		t.Run(body, func(t *testing.T) {
			resp := &http.Response{
				StatusCode:    http.StatusOK,
				ContentLength: -1,
				Header:        make(http.Header),
				Body:          io.NopCloser(bytes.NewBufferString(body)),
			}
			defer resp.Body.Close()
			sniffAndClassify(resp)
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != body {
				t.Fatalf("body changed after sniff: got %q, want %q", got, body)
			}
		})
	}
}

// dribbleReader returns one byte per Read so the sniffer sees the stream
// fragmented across many small reads, as a real TCP segment boundary can.
type dribbleReader struct {
	data []byte
	pos  int
}

func (d *dribbleReader) Read(p []byte) (int, error) {
	if d.pos >= len(d.data) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = d.data[d.pos]
	d.pos++
	return 1, nil
}

// TestSniffAndClassifyHandlesFragmentedSSE is the regression for a headerless
// stream whose first read ends mid-line: the old single-Read sniffer classified
// it as non-streaming, which buffered the whole stream as JSON and dropped the
// SSE content type.
func TestSniffAndClassifyHandlesFragmentedSSE(t *testing.T) {
	const body = "event: response.created\ndata: {}\n\n"
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		ContentLength: -1,
		Header:        make(http.Header),
		Body:          io.NopCloser(&dribbleReader{data: []byte(body)}),
	}
	defer resp.Body.Close()
	sniffAndClassify(resp)
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("fragmented headerless SSE content type = %q, want text/event-stream", got)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("body changed after fragmented sniff: got %q, want %q", got, body)
	}
}

// TestSniffAndClassifyFragmentedJSONStaysNonStreaming is the counterpart: a
// fragmented JSON body must not be mistaken for a stream.
func TestSniffAndClassifyFragmentedJSONStaysNonStreaming(t *testing.T) {
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		ContentLength: -1,
		Header:        make(http.Header),
		Body:          io.NopCloser(&dribbleReader{data: []byte(`{"choices":[]}`)}),
	}
	defer resp.Body.Close()
	sniffAndClassify(resp)
	if got := resp.Header.Get("Content-Type"); got != "" {
		t.Fatalf("fragmented JSON content type = %q, want empty", got)
	}
}

// TestSniffAndClassifyStopsAtBudget proves the sniffer cannot buffer without
// bound when a headerless body never yields a complete line.
func TestSniffAndClassifyStopsAtBudget(t *testing.T) {
	// A single 4 KiB line with no newline: the sniffer must stop at the sniff
	// budget rather than consuming the whole body.
	body := bytes.Repeat([]byte("x"), 4096)
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		ContentLength: -1,
		Header:        make(http.Header),
		Body:          io.NopCloser(bytes.NewReader(body)),
	}
	defer resp.Body.Close()
	sniffAndClassify(resp)
	if got := resp.Header.Get("Content-Type"); got != "" {
		t.Fatalf("unterminated line content type = %q, want empty", got)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(body) {
		t.Fatalf("body length after sniff = %d, want %d", len(got), len(body))
	}
}
