package server

import (
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/providers"
)

func TestDecodeReasoningCapabilitiesHandlesStoredJSON(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "null", raw: "", want: false},
		{name: "json null", raw: "null", want: false},
		{name: "malformed", raw: `{ "options":`, want: false},
		{name: "valid", raw: `{"options":[{"type":"effort","values":["low"]}]}`, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caps := decodeReasoningCapabilities(nullableReasoningTestString(tc.raw))
			if (caps != nil) != tc.want {
				t.Fatalf("decoded capabilities = %#v, want present=%v", caps, tc.want)
			}
		})
	}
}

func nullableReasoningTestString(raw string) (v sql.NullString) {
	if raw != "" {
		v.Valid = true
		v.String = raw
	}
	return v
}

func TestMergeReasoningCapabilitiesBuildsStableSuperset(t *testing.T) {
	minA, maxA := int64(128), int64(4096)
	minB, maxB := int64(64), int64(8192)
	merged := mergeReasoningCapabilities(
		&providers.ReasoningCapabilities{
			Options:        []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"high", "low"}}, {Type: providers.ReasoningOptionBudgetTokens, Min: &minA, Max: &maxA}},
			ThinkingModes:  []string{"enabled"},
			DefaultEffort:  "low",
			Mandatory:      boolPtr(false),
			DefaultEnabled: boolPtr(true),
			Parameters:     []string{"reasoning", "reasoning_effort"},
		},
		&providers.ReasoningCapabilities{
			Options:        []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort}, {Type: providers.ReasoningOptionToggle}, {Type: providers.ReasoningOptionBudgetTokens, Min: &minB, Max: &maxB}},
			ThinkingModes:  []string{"adaptive"},
			DefaultEffort:  "medium",
			Mandatory:      boolPtr(true),
			DefaultEnabled: boolPtr(false),
			Parameters:     []string{"include_reasoning", "reasoning_effort"},
		},
	)
	if merged == nil || len(merged.Options) != 3 {
		t.Fatalf("merged capabilities = %#v", merged)
	}
	if len(merged.Options[0].Values) != 0 || merged.Options[0].Type != providers.ReasoningOptionEffort {
		t.Fatalf("effort superset = %#v, want unrestricted effort", merged.Options[0])
	}
	budget := merged.Options[2]
	if budget.Type != providers.ReasoningOptionBudgetTokens || budget.Min == nil || *budget.Min != 64 || budget.Max == nil || *budget.Max != 8192 {
		t.Fatalf("budget superset = %#v", budget)
	}
	if merged.Options[1].Type != providers.ReasoningOptionToggle {
		t.Fatalf("toggle missing: %#v", merged.Options)
	}
	if got, want := merged.ThinkingModes, []string{"adaptive", "enabled"}; !equalStrings(got, want) {
		t.Fatalf("thinking modes = %v, want %v", got, want)
	}
	if got, want := merged.Parameters, []string{"reasoning", "reasoning_effort", "include_reasoning"}; !equalStrings(got, want) {
		t.Fatalf("parameters = %v, want %v", got, want)
	}
	if merged.DefaultEffort != "low" || merged.Mandatory == nil || !*merged.Mandatory || merged.DefaultEnabled == nil || !*merged.DefaultEnabled {
		t.Fatalf("defaults/booleans = %#v", merged)
	}
}

func TestMergeReasoningCapabilitiesHandlesNilAndFiniteEffortUnion(t *testing.T) {
	if mergeReasoningCapabilities(nil, nil) != nil {
		t.Fatal("nil inputs should produce nil capabilities")
	}
	caps := &providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low"}}}}
	if got := mergeReasoningCapabilities(nil, caps); got != caps {
		t.Fatalf("nil left input returned %#v, want original capabilities", got)
	}
	merged := mergeReasoningCapabilities(
		&providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"high", "low"}}}},
		&providers.ReasoningCapabilities{Options: []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"minimal", "medium"}}}},
	)
	if merged == nil || len(merged.Options) != 1 || !equalStrings(merged.Options[0].Values, []string{"minimal", "low", "medium", "high"}) {
		t.Fatalf("finite effort union = %#v", merged)
	}
}

func TestVirtualAdminExposesTargetAndEligibleAggregateReasoningCapabilities(t *testing.T) {
	api, db, _, _ := loggingTestHarness(t, mockUpstream(t))
	var modelA string
	if err := db.SQL.QueryRow(`SELECT id FROM provider_models WHERE upstream_model_id='model-a'`).Scan(&modelA); err != nil {
		t.Fatal(err)
	}

	providerB, modelB := "provider-b-id", "model-b-id"
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.SQL.Exec(`INSERT INTO namespaces(name,kind,entity_id) VALUES('provider-b','real',?)`, providerB); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO providers(id,name,type,base_url,credential_secret,enabled,protocols,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, providerB, "provider-b", "generic-openai", "http://127.0.0.1:1/v1", "secret-b", 1, `["chat"]`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO provider_models(id,provider_id,upstream_model_id,available,first_seen_at,last_seen_at,created_at,updated_at,reasoning_capabilities) VALUES(?,?,?,?,?,?,?,?,?)`, modelB, providerB, "model-b", 1, now, now, now, now, `{"options":[{"type":"effort"},{"type":"toggle"}],"thinking_modes":["adaptive"],"default_effort":"medium"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`UPDATE provider_models SET reasoning_capabilities=? WHERE id=?`, `{"options":[{"type":"effort","values":["low","high"]}],"thinking_modes":["enabled"],"default_effort":"low"}`, modelA); err != nil {
		t.Fatal(err)
	}

	status, payload, _ := api.request("POST", "/api/admin/virtual-groups", map[string]any{"name": "reasoning-vg"})
	if status != http.StatusCreated {
		t.Fatalf("create group: %d %v", status, payload)
	}
	status, payload, _ = api.request("POST", "/api/admin/virtual-models", map[string]any{
		"group_id": payload["id"], "name": "reasoning-vm", "routing_mode": "ordered_fallback",
		"targets": []any{
			map[string]any{"provider_model_id": modelA, "enabled": true},
			map[string]any{"provider_model_id": modelB, "enabled": false},
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("create virtual model: %d %v", status, payload)
	}
	virtualID := payload["id"].(string)

	status, payload, _ = api.request("GET", "/api/admin/virtual-models", nil)
	if status != http.StatusOK {
		t.Fatalf("list virtual models: %d %v", status, payload)
	}
	virtual := findObjectByID(t, payload["data"], virtualID)
	targets := virtual["targets"].([]any)
	if targets[0].(map[string]any)["reasoning_capabilities"] == nil || targets[1].(map[string]any)["reasoning_capabilities"] == nil {
		t.Fatalf("target capabilities missing: %#v", targets)
	}
	aggregate := virtual["reasoning_capabilities"].(map[string]any)
	options := aggregate["options"].([]any)
	if len(options) != 1 || options[0].(map[string]any)["type"] != string(providers.ReasoningOptionEffort) {
		t.Fatalf("disabled target contributed to aggregate: %#v", aggregate)
	}

	if _, err := db.SQL.Exec(`UPDATE virtual_model_targets SET enabled=1 WHERE virtual_model_id=? AND provider_model_id=?`, virtualID, modelB); err != nil {
		t.Fatal(err)
	}
	status, payload, _ = api.request("GET", "/api/admin/virtual-models", nil)
	if status != http.StatusOK {
		t.Fatalf("list merged virtual models: %d %v", status, payload)
	}
	aggregate = findObjectByID(t, payload["data"], virtualID)["reasoning_capabilities"].(map[string]any)
	options = aggregate["options"].([]any)
	if len(options) != 2 || options[0].(map[string]any)["type"] != string(providers.ReasoningOptionEffort) || options[1].(map[string]any)["type"] != string(providers.ReasoningOptionToggle) {
		t.Fatalf("eligible target capabilities were not merged: %#v", aggregate)
	}

	if _, err := db.SQL.Exec(`UPDATE providers SET enabled=0 WHERE id=?`, providerB); err != nil {
		t.Fatal(err)
	}
	status, payload, _ = api.request("GET", "/api/admin/virtual-models", nil)
	if status != http.StatusOK {
		t.Fatalf("list unavailable virtual models: %d %v", status, payload)
	}
	virtual = findObjectByID(t, payload["data"], virtualID)
	aggregate = virtual["reasoning_capabilities"].(map[string]any)
	options = aggregate["options"].([]any)
	if len(options) != 1 || options[0].(map[string]any)["type"] != string(providers.ReasoningOptionEffort) {
		t.Fatalf("unavailable target contributed to aggregate: %#v", aggregate)
	}
	if targets := virtual["targets"].([]any); targets[1].(map[string]any)["reasoning_capabilities"] == nil {
		t.Fatalf("unavailable target metadata was hidden: %#v", targets[1])
	}
	if _, err := db.SQL.Exec(`UPDATE providers SET enabled=1 WHERE id=?`, providerB); err != nil {
		t.Fatal(err)
	}

	if _, err := db.SQL.Exec(`UPDATE provider_models SET reasoning_capabilities=? WHERE id=?`, `{ "options":`, modelB); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`UPDATE provider_models SET reasoning_capabilities=NULL WHERE id=?`, modelA); err != nil {
		t.Fatal(err)
	}
	status, payload, _ = api.request("GET", "/api/admin/virtual-models", nil)
	if status != http.StatusOK {
		t.Fatalf("list unknown virtual models: %d %v", status, payload)
	}
	virtual = findObjectByID(t, payload["data"], virtualID)
	if _, exists := virtual["reasoning_capabilities"]; exists {
		t.Fatalf("unknown aggregate should be omitted: %#v", virtual)
	}
	for _, raw := range virtual["targets"].([]any) {
		if _, exists := raw.(map[string]any)["reasoning_capabilities"]; exists {
			t.Fatalf("NULL/malformed target capability should be omitted: %#v", raw)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
