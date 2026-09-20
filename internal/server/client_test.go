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
