package server

import "testing"

func TestFallbackStatusIncludesUnavailableUpstreams(t *testing.T) {
	for _, status := range []int{401, 403, 404, 429, 500, 502, 503, 504} {
		if !fallbackStatus(status) {
			t.Errorf("fallbackStatus(%d) = false, want true", status)
		}
	}
	for _, status := range []int{200, 400, 422} {
		if fallbackStatus(status) {
			t.Errorf("fallbackStatus(%d) = true, want false", status)
		}
	}
}
