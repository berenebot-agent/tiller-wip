package server

import "testing"

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
