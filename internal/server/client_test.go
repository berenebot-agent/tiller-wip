package server

import (
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
		name string
		ct   string
		want bool
	}{
		{name: "translated JSON", ct: "application/json", want: false},
		{name: "translated SSE", ct: "text/event-stream", want: true},
		{name: "JSON with stream request", ct: "application/json", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{"Content-Type": []string{test.ct}}}
			got := isStreamingResponse(resp)
			if got != test.want {
				t.Fatalf("isStreamingResponse(%q) = %v, want %v", test.ct, got, test.want)
			}
		})
	}
}
