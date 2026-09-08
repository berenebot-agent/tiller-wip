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

func TestFallbackCooldownSetting(t *testing.T) {
	api, _, _, _ := loggingTestHarness(t, mockUpstream(t))

	// Default from migration 025.
	status, payload, _ := api.request("GET", "/api/admin/settings", nil)
	if status != 200 {
		t.Fatalf("get settings: %d %v", status, payload)
	}
	if v, ok := payload["fallback_cooldown_seconds"].(float64); !ok || v != 300 {
		t.Fatalf("default fallback_cooldown_seconds = %v, want 300", payload["fallback_cooldown_seconds"])
	}

	// Update and confirm round-trip.
	status, _, _ = api.request("PUT", "/api/admin/settings", map[string]any{"fallback_cooldown_seconds": 60})
	if status != 204 {
		t.Fatalf("update fallback cooldown: %d", status)
	}
	status, payload, _ = api.request("GET", "/api/admin/settings", nil)
	if status != 200 {
		t.Fatalf("get settings after update: %d %v", status, payload)
	}
	if v, ok := payload["fallback_cooldown_seconds"].(float64); !ok || v != 60 {
		t.Fatalf("fallback_cooldown_seconds after update = %v, want 60", payload["fallback_cooldown_seconds"])
	}

	// 0 is accepted (disables cooldown).
	status, _, _ = api.request("PUT", "/api/admin/settings", map[string]any{"fallback_cooldown_seconds": 0})
	if status != 204 {
		t.Fatalf("fallback cooldown 0 should be accepted, got %d", status)
	}

	// Negative values are rejected.
	status, _, _ = api.request("PUT", "/api/admin/settings", map[string]any{"fallback_cooldown_seconds": -1})
	if status != 400 {
		t.Fatalf("negative fallback cooldown should be rejected, got %d", status)
	}
}
