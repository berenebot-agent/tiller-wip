package server

import (
	"bytes"
	"encoding/json"
	"errors"
)

const claudeSubscriptionSystem = "You are Claude Code, Anthropic's official CLI for Claude."

func normalizeClaudeRequest(body []byte) ([]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || raw == nil {
		return nil, errors.New("invalid Claude request")
	}
	var system []json.RawMessage
	if value, ok := raw["system"]; ok {
		value = bytes.TrimSpace(value)
		if len(value) > 0 && value[0] == '"' {
			var text string
			if err := json.Unmarshal(value, &text); err != nil {
				return nil, errors.New("invalid Claude system instructions")
			}
			if text != "" {
				block, _ := json.Marshal(map[string]string{"type": "text", "text": text})
				system = append(system, block)
			}
		} else if len(value) == 0 || value[0] != '[' || json.Unmarshal(value, &system) != nil {
			return nil, errors.New("invalid Claude system instructions")
		}
	}
	hasIdentity := false
	for i, value := range system {
		var block struct {
			Type string  `json:"type"`
			Text *string `json:"text"`
		}
		if err := json.Unmarshal(value, &block); err != nil || block.Type != "text" || block.Text == nil {
			return nil, errors.New("invalid Claude system instructions")
		}
		if i == 0 {
			hasIdentity = *block.Text == claudeSubscriptionSystem
		}
	}
	if hasIdentity && bytes.HasPrefix(bytes.TrimSpace(raw["system"]), []byte("[")) {
		return body, nil
	}
	// Subscription inference requires this identity as a separate leading block, not joined to caller instructions.
	if !hasIdentity {
		prefix, _ := json.Marshal(map[string]string{"type": "text", "text": claudeSubscriptionSystem})
		system = append([]json.RawMessage{prefix}, system...)
	}
	raw["system"], _ = json.Marshal(system)
	return json.Marshal(raw)
}
