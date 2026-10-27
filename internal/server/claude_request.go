package server

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/tiller-router/tiller-router/internal/id"
	"github.com/tiller-router/tiller-router/internal/providers/claude"
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
	hasBilling := false
	for i, value := range system {
		var block struct {
			Type string  `json:"type"`
			Text *string `json:"text"`
		}
		if err := json.Unmarshal(value, &block); err != nil || block.Type != "text" || block.Text == nil {
			return nil, errors.New("invalid Claude system instructions")
		}
		if i == 0 && strings.HasPrefix(*block.Text, "x-anthropic-billing-header:") {
			hasBilling = true
		}
		if i == 0 || i == 1 && hasBilling {
			hasIdentity = *block.Text == claudeSubscriptionSystem
		}
	}
	if !hasIdentity {
		prefix, _ := json.Marshal(map[string]string{"type": "text", "text": claudeSubscriptionSystem})
		if hasBilling {
			system = append(system[:1], append([]json.RawMessage{prefix}, system[1:]...)...)
		} else {
			system = append([]json.RawMessage{prefix}, system...)
		}
	}
	if !hasBilling {
		hash := sha256.Sum256(body)
		var build [2]byte
		if _, err := rand.Read(build[:]); err != nil {
			return nil, err
		}
		text := fmt.Sprintf("x-anthropic-billing-header: cc_version=%s.%s; cc_entrypoint=sdk-cli; cch=%s;", claude.CLIVersion, hex.EncodeToString(build[:])[:3], hex.EncodeToString(hash[:])[:5])
		block, _ := json.Marshal(map[string]string{"type": "text", "text": text})
		system = append([]json.RawMessage{block}, system...)
	}
	metadata := map[string]json.RawMessage{}
	if value, ok := raw["metadata"]; ok {
		if err := json.Unmarshal(value, &metadata); err != nil {
			return nil, errors.New("invalid Claude metadata")
		}
		if metadata == nil {
			metadata = map[string]json.RawMessage{}
		}
	}
	if _, ok := metadata["user_id"]; !ok {
		var device [32]byte
		if _, err := rand.Read(device[:]); err != nil {
			return nil, err
		}
		account, err := id.New()
		if err != nil {
			return nil, err
		}
		session, err := id.New()
		if err != nil {
			return nil, err
		}
		user, _ := json.Marshal(map[string]string{"device_id": hex.EncodeToString(device[:]), "account_uuid": account, "session_id": session})
		metadata["user_id"], _ = json.Marshal(string(user))
	}
	raw["system"], _ = json.Marshal(system)
	raw["metadata"], _ = json.Marshal(metadata)
	return json.Marshal(raw)
}
