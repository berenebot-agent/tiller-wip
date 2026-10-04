package server

import "testing"

// TestHostedRejectsHostedDisabledProviderType is the regression guard for
// pre-SaaS review TR-001: a provider type flagged HostedDisabled (currently
// opencode-free) must be refused by the hosted provider-create endpoint, not
// merely hidden from the hosted add-provider UI. Without this, a direct API
// call could relay anonymous free-tier traffic under the operator's egress IP,
// contradicting the recorded provider-terms decision.
func TestHostedRejectsHostedDisabledProviderType(t *testing.T) {
	_, api, _ := hostedServerHarness(t, false)

	status, payload, _ := api.request("POST", "/api/admin/providers", map[string]any{
		"name": "free-relay", "type": "opencode-free", "base_url": "https://opencode.ai/zen/v1",
	})
	if status != 400 {
		t.Fatalf("hosted opencode-free create = %d %v, want 400", status, payload)
	}
	if errObj, ok := payload["error"].(map[string]any); !ok || errObj["code"] != "provider_type_disabled" {
		t.Fatalf("hosted opencode-free create error = %v, want code provider_type_disabled", payload)
	}
}

// TestInferenceBufferBudgetBounded is the regression guard for pre-SaaS review
// TR-002: the product of each admission gate and its per-request cap is the
// process's worst-case buffer budget and must stay inside a small container.
// A future edit that raises a cap or widens a gate without re-checking the
// other fails here rather than re-opening the OOM.
func TestInferenceBufferBudgetBounded(t *testing.T) {
	const budget = int64(1) << 30 // 1 GiB aggregate worst case per direction

	if inbound := int64(maxConcurrentBodyReads) * maxInferenceBodyBytes; inbound > budget {
		t.Fatalf("inbound buffer budget %d exceeds %d", inbound, budget)
	}
	if outbound := int64(cap(nonStreamBufferGate)) * maxUpstreamNonStreamBytes; outbound > budget {
		t.Fatalf("outbound buffer budget %d exceeds %d", outbound, budget)
	}
}
