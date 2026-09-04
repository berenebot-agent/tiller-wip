package server

import "testing"

func TestFallbackTimeoutSetting(t *testing.T) {
	api, _, _, _ := loggingTestHarness(t, mockUpstream(t))

	// Default from migration 012.
	status, payload, _ := api.request("GET", "/api/admin/settings", nil)
	if status != 200 {
		t.Fatalf("get settings: %d %v", status, payload)
	}
	if v, ok := payload["fallback_timeout_seconds"].(float64); !ok || v != 60 {
		t.Fatalf("default fallback_timeout_seconds = %v, want 60", payload["fallback_timeout_seconds"])
	}

	// Update and confirm round-trip.
	status, _, _ = api.request("PUT", "/api/admin/settings", map[string]any{"fallback_timeout_seconds": 120})
	if status != 204 {
		t.Fatalf("update fallback timeout: %d", status)
	}
	status, payload, _ = api.request("GET", "/api/admin/settings", nil)
	if status != 200 {
		t.Fatalf("get settings after update: %d %v", status, payload)
	}
	if v, ok := payload["fallback_timeout_seconds"].(float64); !ok || v != 120 {
		t.Fatalf("fallback_timeout_seconds after update = %v, want 120", payload["fallback_timeout_seconds"])
	}

	// Validation bounds.
	status, _, _ = api.request("PUT", "/api/admin/settings", map[string]any{"fallback_timeout_seconds": 0})
	if status != 400 {
		t.Fatalf("fallback timeout 0 should be rejected, got %d", status)
	}
	status, _, _ = api.request("PUT", "/api/admin/settings", map[string]any{"fallback_timeout_seconds": 3601})
	if status != 400 {
		t.Fatalf("fallback timeout 3601 should be rejected, got %d", status)
	}
}

func TestDetailedErrorLoggingSetting(t *testing.T) {
	api, _, _, _ := loggingTestHarness(t, mockUpstream(t))
	status, payload, _ := api.request("GET", "/api/admin/settings", nil)
	if status != 200 || payload["log_error_bodies"] != false {
		t.Fatalf("log_error_bodies default = %v, want false", payload["log_error_bodies"])
	}
	status, _, _ = api.request("PUT", "/api/admin/settings", map[string]any{"log_error_bodies": true})
	if status != 204 {
		t.Fatalf("enable detailed error logging: %d", status)
	}
	status, payload, _ = api.request("GET", "/api/admin/settings", nil)
	if status != 200 || payload["log_error_bodies"] != true {
		t.Fatalf("log_error_bodies after update = %v, want true", payload["log_error_bodies"])
	}
}
