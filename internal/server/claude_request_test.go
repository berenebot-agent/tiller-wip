package server

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/providers"
)

func TestNormalizeClaudeRequest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		system string
		want   int
	}{
		{"absent", "", 1},
		{"empty string", `"system":"",`, 1},
		{"empty array", `"system":[],`, 1},
		{"existing string", `"system":"` + claudeSubscriptionSystem + `",`, 1},
		{"existing blocks", `"system":[{"type":"text","text":"` + claudeSubscriptionSystem + `"}],`, 1},
		{"string", `"system":"caller instructions",`, 2},
		{"blocks", `"system":[{"type":"text","text":"caller instructions","cache_control":{"type":"ephemeral"}}],`, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{` + tc.system + `"model":"discovered-model","stream":true,"max_tokens":16,"metadata":{"sequence":9007199254740993},"messages":[{"role":"user","content":"synthetic input"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`)
			got, err := normalizeClaudeRequest(body)
			if err != nil {
				t.Fatal(err)
			}
			var before, after map[string]json.RawMessage
			_ = json.Unmarshal(body, &before)
			_ = json.Unmarshal(got, &after)
			var system []map[string]json.RawMessage
			if err := json.Unmarshal(after["system"], &system); err != nil || len(system) != tc.want+1 {
				t.Fatal("wrong system block count")
			}
			var billing string
			_ = json.Unmarshal(system[0]["text"], &billing)
			if !strings.HasPrefix(billing, "x-anthropic-billing-header: cc_version=") {
				t.Fatal("missing billing identity")
			}
			var identity string
			_ = json.Unmarshal(system[1]["text"], &identity)
			if identity != claudeSubscriptionSystem {
				t.Fatal("missing subscription envelope")
			}
			if tc.want == 2 {
				var text string
				_ = json.Unmarshal(system[2]["text"], &text)
				if text != "caller instructions" {
					t.Fatal("caller instructions changed")
				}
				if tc.name == "blocks" && !bytes.Equal(system[2]["cache_control"], []byte(`{"type":"ephemeral"}`)) {
					t.Fatal("cache control changed")
				}
			}
			delete(before, "system")
			delete(after, "system")
			var metadata map[string]json.RawMessage
			if err := json.Unmarshal(after["metadata"], &metadata); err != nil || len(metadata["user_id"]) == 0 || string(metadata["sequence"]) != "9007199254740993" {
				t.Fatal("metadata was not preserved and extended")
			}
			delete(metadata, "user_id")
			after["metadata"], _ = json.Marshal(metadata)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("unrelated request fields changed")
			}
			again, err := normalizeClaudeRequest(got)
			if err != nil || !bytes.Equal(again, got) {
				t.Fatal("normalization is not idempotent")
			}
		})
	}
}

func TestNormalizeClaudeRequestRejectsMalformedSystem(t *testing.T) {
	for _, body := range []string{
		`null`, `[]`, `{`, `{"system":null}`, `{"system":42}`, `{"system":{}}`,
		`{"system":[null]}`, `{"system":[{"type":"text","text":42}]}`,
		`{"system":[{"type":"image","text":"synthetic input"}]}`,
		`{"system":[{"type":"text","text":"` + claudeSubscriptionSystem + `"},null]}`,
	} {
		if _, err := normalizeClaudeRequest([]byte(body)); err == nil {
			t.Fatal("malformed instructions accepted")
		}
	}
}

func TestNormalizeClaudeRequestPreservesUserID(t *testing.T) {
	body := []byte(`{"metadata":{"user_id":"existing","other":true}}`)
	got, err := normalizeClaudeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Metadata map[string]json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(got, &result); err != nil || string(result.Metadata["user_id"]) != `"existing"` || string(result.Metadata["other"]) != "true" {
		t.Fatal("existing metadata changed")
	}
}

func TestClaudeSubscriptionTranslation(t *testing.T) {
	for _, tc := range []struct {
		protocol providers.Protocol
		body     string
	}{
		{providers.ProtocolChat, `{"messages":[{"role":"system","content":"caller instructions"},{"role":"user","content":"synthetic input"}]}`},
		{providers.ProtocolResponses, `{"instructions":"caller instructions","input":"synthetic input"}`},
		{providers.ProtocolMessages, `{"system":"caller instructions","messages":[{"role":"user","content":"synthetic input"}]}`},
	} {
		translated, err := translateRequest([]byte(tc.body), tc.protocol, providers.ProtocolMessages, "discovered-model")
		if err != nil {
			t.Fatal(err)
		}
		got, err := normalizeClaudeRequest(translated)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]json.RawMessage
		_ = json.Unmarshal(got, &body)
		var system []map[string]string
		if err := json.Unmarshal(body["system"], &system); err != nil || len(system) != 3 || system[1]["text"] != claudeSubscriptionSystem || system[2]["text"] != "caller instructions" {
			t.Fatal("translated caller instructions were not preserved after envelope")
		}
	}
}
