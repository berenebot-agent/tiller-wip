package server

import (
	"encoding/json"
	"strings"
	"unicode"
)

const maxStoredErrorMessageBytes = 1024

// extractProviderErrorMessage accepts only common structured provider error
// shapes. The response body itself is never returned or persisted.
func extractProviderErrorMessage(body []byte) string {
	var payload map[string]json.RawMessage
	if len(body) == 0 || json.Unmarshal(body, &payload) != nil {
		return ""
	}
	var message string
	if raw, ok := payload["error"]; ok {
		var detail map[string]json.RawMessage
		if json.Unmarshal(raw, &detail) == nil {
			_ = json.Unmarshal(detail["message"], &message)
		}
	}
	if message == "" {
		_ = json.Unmarshal(payload["message"], &message)
	}
	return cleanStoredErrorMessage(message)
}

func cleanStoredErrorMessage(message string) string {
	var b strings.Builder
	for _, r := range message {
		if unicode.IsControl(r) {
			continue
		}
		if b.Len()+len(string(r)) > maxStoredErrorMessageBytes {
			break
		}
		b.WriteRune(r)
	}
	message = strings.TrimSpace(b.String())
	if message == "" || looksLikeCredential(message) {
		return ""
	}
	return message
}

func looksLikeCredential(message string) bool {
	lower := strings.ToLower(message)
	for _, marker := range []string{"authorization:", "authorization=", "bearer ", "api_key=", "api-key=", "sk-"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func fixedUpstreamErrorMessage(class string) string {
	switch class {
	case "upstream_timeout":
		return "Upstream request timed out"
	case "upstream_unreachable":
		return "Could not reach upstream provider"
	case "upstream_read_error":
		return "Could not read upstream provider response"
	case "upstream_response_too_large":
		return "Upstream provider response exceeded Tiller's size limit"
	default:
		return ""
	}
}

func strPtrIfNonEmpty(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
