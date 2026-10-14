package server

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tiller-router/tiller-router/internal/providers"
)

const (
	maxClientErrorMessageRunes = 2000
	maxPlainErrorBodyBytes     = 4 << 10
	// maxErrorDetailUnwrapDepth bounds how many nested error envelopes are
	// peeled apart. Each unwrap strictly shrinks the message (the inner text is
	// a substring of the level that held it), so this is a guard against a
	// deceptive or accidentally self-similar chain, not a termination condition.
	maxErrorDetailUnwrapDepth = 3
)

// upstreamErrorDetail is the summary extracted from a provider's error
// response. Fields are raw until parseUpstreamErrorDetail's final sanitized
// step; parseErrorEnvelope's output is not yet bounded.
type upstreamErrorDetail struct {
	Message string
	Code    string
	Param   string
	Type    string
}

// parseUpstreamErrorDetail extracts a bounded, client-safe summary from an
// upstream error body. It understands the OpenAI/Chat shape
// ({"error":{message,type,code,param}}), the Anthropic shape
// ({"type":"error","error":{type,message}}), a string error ({"error":"..."}),
// a top-level message, and short printable text/plain bodies. Anything else
// yields the zero value so the caller falls back to the generic router-owned
// message.
//
// Relays that return an upstream error as a JSON *string* inside their own
// error.message (Command Code does this) are unwrapped one level at a time, so
// the client sees the provider's message instead of a nested blob and the
// failure classifiers (isContextLimitError, isReasoningRoundtripError) can see
// through the wrap. The result is NOT credential-redacted; callers must run it
// through redactProviderSecrets.
func parseUpstreamErrorDetail(body []byte, contentType string) upstreamErrorDetail {
	return unwrapErrorDetail(parseErrorEnvelope(body, contentType), 0).sanitized()
}

// parseErrorEnvelope performs one level of extraction and returns RAW fields;
// sanitized applies the bounds and control-character stripping once, at the
// end. Sanitizing here would let the rune cap truncate valid JSON before
// unwrapErrorDetail could parse it. Bodies are already bounded by
// maxUpstreamErrorBytes.
func parseErrorEnvelope(body []byte, contentType string) upstreamErrorDetail {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return upstreamErrorDetail{}
	}
	if strings.HasPrefix(trimmed, "{") {
		var top struct {
			Error   json.RawMessage `json:"error"`
			Message string          `json:"message"`
		}
		if err := json.Unmarshal(body, &top); err != nil {
			return upstreamErrorDetail{}
		}
		if len(top.Error) > 0 {
			var obj struct {
				Message string          `json:"message"`
				Type    string          `json:"type"`
				Param   string          `json:"param"`
				Code    json.RawMessage `json:"code"`
			}
			if err := json.Unmarshal(top.Error, &obj); err == nil {
				detail := upstreamErrorDetail{
					Message: obj.Message,
					Code:    decodeErrorCode(obj.Code),
					Param:   obj.Param,
					Type:    obj.Type,
				}
				if detail.usable() {
					return detail
				}
			}
			var str string
			if err := json.Unmarshal(top.Error, &str); err == nil {
				return upstreamErrorDetail{Message: str}
			}
		}
		if top.Message != "" {
			return upstreamErrorDetail{Message: top.Message}
		}
		return upstreamErrorDetail{}
	}
	if strings.Contains(contentType, "text/plain") && len(body) <= maxPlainErrorBodyBytes && utf8.Valid(body) {
		return upstreamErrorDetail{Message: trimmed}
	}
	return upstreamErrorDetail{}
}

// unwrapErrorDetail peels a relay that returned an upstream error as JSON text
// inside its own error.message. Inner fields win; the outer level fills any gap
// so a wrapper's code is not lost when the inner envelope omits one.
func unwrapErrorDetail(detail upstreamErrorDetail, depth int) upstreamErrorDetail {
	for depth < maxErrorDetailUnwrapDepth && strings.HasPrefix(strings.TrimSpace(detail.Message), "{") {
		inner := parseErrorEnvelope([]byte(detail.Message), "application/json")
		if !inner.usable() {
			return detail
		}
		inner.Code = firstNonEmpty(inner.Code, detail.Code)
		inner.Param = firstNonEmpty(inner.Param, detail.Param)
		inner.Type = firstNonEmpty(inner.Type, detail.Type)
		detail = inner
		depth++
	}
	return detail
}

// usable reports whether the detail carries anything worth surfacing, ignoring
// whitespace-only values. It mirrors clientMessage's emptiness check without
// rendering the message.
func (d upstreamErrorDetail) usable() bool {
	return sanitizeErrorText(d.Message) != "" || sanitizeErrorText(d.Code) != "" || sanitizeErrorText(d.Param) != ""
}

// sanitized bounds every field and strips control characters. The parser keeps
// fields raw until the very end so unwrapping never operates on truncated JSON.
func (d upstreamErrorDetail) sanitized() upstreamErrorDetail {
	return upstreamErrorDetail{
		Message: sanitizeErrorText(d.Message),
		Code:    sanitizeErrorText(d.Code),
		Param:   sanitizeErrorText(d.Param),
		Type:    sanitizeErrorText(d.Type),
	}
}

// firstNonEmpty returns the first value carrying non-whitespace content.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// isOpenCodeFreeTierRejection reports whether an upstream error body is
// OpenCode's Console-wrapped free-tier policy rejection ("from within
// OpenCode"). The inner FreeTierError type is matched directly; the message
// substring is a fallback so minor upstream rewording still trips detection.
// Matching is case-insensitive and confined to OpenCode's explicit gate text —
// ordinary provider errors never contain it.
func isOpenCodeFreeTierRejection(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	lowered := strings.ToLower(string(body))
	if !strings.Contains(lowered, "from within opencode") {
		return false
	}
	return strings.Contains(lowered, "freetiererror") || strings.Contains(lowered, "free tier")
}

func decodeErrorCode(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return sanitizeErrorText(s)
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	return ""
}

// clientMessage renders the detail as a router-framed, client-facing string.
// It returns "" when there is nothing usable, so callers keep the existing
// generic message.
func (d upstreamErrorDetail) clientMessage() string {
	if d.Message == "" && d.Code == "" && d.Param == "" {
		return ""
	}
	var b strings.Builder
	if d.Message != "" {
		b.WriteString("Upstream provider error: ")
		b.WriteString(d.Message)
	} else {
		b.WriteString("Upstream provider error")
	}
	var qualifiers []string
	if d.Code != "" {
		qualifiers = append(qualifiers, "code: "+d.Code)
	}
	if d.Param != "" {
		qualifiers = append(qualifiers, "param: "+d.Param)
	}
	if len(qualifiers) > 0 {
		b.WriteString(" (")
		b.WriteString(strings.Join(qualifiers, ", "))
		b.WriteString(")")
	}
	return b.String()
}

// isContextLimitError recognizes provider-reported context exhaustion without
// relying on model names or provider-specific endpoint assumptions. The raw
// body has already been reduced to bounded, sanitized metadata by the parser.
func isContextLimitError(d upstreamErrorDetail) bool {
	code := strings.ToLower(strings.TrimSpace(d.Code))
	switch code {
	case "context_length_exceeded", "context_window_exceeded", "input_too_long", "prompt_too_long":
		return true
	}
	message := strings.ToLower(d.Message)
	for _, phrase := range []string{
		"maximum context length",
		"context length exceeded",
		"exceeds the context window",
		"exceeded the context window",
		"context window exceeded",
		"input is too long",
		"prompt is too long",
	} {
		if strings.Contains(message, phrase) {
			return true
		}
	}
	return false
}

// isReasoningRoundtripError recognizes a provider rejecting a request because
// reasoning state from a previous turn was not passed back. DeepSeek's thinking
// mode requires this when a request carries tools, and returns it as a 400 with
// code invalid_request_error. The condition belongs to the request shape, not
// the target's health: treating it as a generic http_400 would open a cooldown
// and hide a working model from every client. The raw body has already been
// reduced to bounded, sanitized metadata by the parser.
func isReasoningRoundtripError(d upstreamErrorDetail) bool {
	message := strings.ToLower(d.Message)
	return strings.Contains(message, "reasoning_content") && strings.Contains(message, "must be passed back")
}

// redactProviderSecrets removes every known active secret for the provider
// instance from text. Only secrets the router actually sent for this instance
// are redacted (the API key/OAuth token, OAuth provider-data token values, and
// the account id), which keeps legitimate provider text intact.
func redactProviderSecrets(text string, p providers.Instance) string {
	if text == "" {
		return text
	}
	secrets := []string{p.Credential, p.OAuthAccountID}
	for _, value := range p.OAuthProviderData {
		if s, ok := value.(string); ok && s != "" {
			secrets = append(secrets, s)
		}
	}
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	return text
}

// sanitizeErrorText strips control characters, collapses whitespace, and caps
// length so provider text cannot smuggle terminal escapes or unbounded content
// to a client.
func sanitizeErrorText(s string) string {
	if s == "" {
		return ""
	}
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return ' '
		case unicode.IsControl(r):
			return -1
		default:
			return r
		}
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if runes := []rune(s); len(runes) > maxClientErrorMessageRunes {
		s = string(runes[:maxClientErrorMessageRunes])
	}
	return strings.TrimSpace(s)
}

// exhaustedRouteMessage renders the per-target failure list shown to a client
// when every target of a virtual model failed or was skipped. Each line names
// the target and carries its sanitized provider detail, or the reason it was
// skipped. The fallback is the router-owned generic sentence so the client
// always gets a usable message.
func exhaustedRouteMessage(attempts []requestAttempt) string {
	const generic = "The virtual model could not be served by its configured targets."
	lines := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		route := attempt.provider
		if attempt.model != "" {
			if route != "" {
				route += "/"
			}
			route += attempt.model
		}
		if route == "" {
			route = "target"
		}
		switch {
		case attempt.failureClass == "context_limit_exceeded":
			reason := fixedUpstreamErrorMessage(attempt.failureClass)
			if attempt.clientError != "" {
				reason += " " + attempt.clientError
			}
			lines = append(lines, fmt.Sprintf("%s: %s", route, reason))
		case attempt.failureClass == "reasoning_roundtrip_required":
			lines = append(lines, fmt.Sprintf("%s: %s", route, fixedUpstreamErrorMessage(attempt.failureClass)))
		case attempt.result == "failed" && attempt.clientError != "":
			prefix := ""
			if attempt.httpStatus > 0 {
				prefix = fmt.Sprintf("HTTP %d ", attempt.httpStatus)
			}
			lines = append(lines, fmt.Sprintf("%s: %s%s", route, prefix, attempt.clientError))
		case attempt.result == "failed":
			reason := "request failed"
			if attempt.errorMessage != nil && *attempt.errorMessage != "" {
				reason = *attempt.errorMessage
			} else if attempt.failureClass != "" {
				reason = attempt.failureClass
			}
			lines = append(lines, fmt.Sprintf("%s: %s", route, reason))
		case attempt.failureClass != "":
			lines = append(lines, fmt.Sprintf("%s: skipped (%s)", route, attempt.failureClass))
		default:
			lines = append(lines, fmt.Sprintf("%s: skipped", route))
		}
	}
	if len(lines) == 0 {
		return generic
	}
	return "All targets for the virtual model failed:\n" + strings.Join(lines, "\n")
}
