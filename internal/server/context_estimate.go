package server

import (
	"encoding/json"
	"math"
	"unicode/utf8"
)

// estimateTokens is a conservative, provider-agnostic input-token estimator. It
// sums the rune length of content-like fields in a request body and divides by
// a chars-per-token ratio (rounding up). It is deliberately approximate: it
// over-estimates prose and CJK, which errs toward the safe direction.
//
// It is a pure function with no server state, shared by the missing-usage
// fallback (a logged request whose provider omitted usage) and any future
// preflight context-length check. It is never used for billing: provider-
// reported token counts remain authoritative whenever present.
//
// DefaultCharsPerToken is the default ratio. English prose is ~4 chars/token
// and code ~3, so 3.0 under-counts prose (conservative for a context check)
// and is close for code.
const DefaultCharsPerToken = 3.0

// EstimateTokens estimates the input token count of a request body for the
// given incoming protocol. It returns 0 when the body is empty or unparseable.
func EstimateTokens(body []byte, protocol string) int {
	return EstimateTokensWithRatio(body, protocol, DefaultCharsPerToken)
}

// EstimateTokensWithRatio is EstimateTokens with an explicit chars-per-token
// ratio. A non-positive ratio falls back to DefaultCharsPerToken.
func EstimateTokensWithRatio(body []byte, protocol string, charsPerToken float64) int {
	if charsPerToken <= 0 {
		charsPerToken = DefaultCharsPerToken
	}
	if len(body) == 0 {
		return 0
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return 0
	}
	var runes int
	switch protocol {
	case "messages":
		runes += contentRunes(payload["system"])
		runes += messagesRunes(payload["messages"])
	default:
		// chat and responses both carry content in messages/input/prompt; walk
		// whichever is present so the estimator is robust to a mismatched
		// declared protocol.
		runes += messagesRunes(payload["messages"])
		runes += responsesInputRunes(payload["input"])
		runes += stringRunes(payload["prompt"])
	}
	if runes <= 0 {
		return 0
	}
	return int(math.Ceil(float64(runes) / charsPerToken))
}

// messagesRunes sums content-like fields across a chat-style messages array.
func messagesRunes(v any) int {
	list, ok := v.([]any)
	if !ok {
		return 0
	}
	total := 0
	for _, item := range list {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		total += contentRunes(msg["content"])
	}
	return total
}

// responsesInputRunes sum content-like fields in a Responses-style input, which
// may be a plain string, an array of item objects, or an array of input_text /
// output_text blocks.
func responsesInputRunes(v any) int {
	switch t := v.(type) {
	case string:
		return utf8.RuneCountInString(t)
	case []any:
		total := 0
		for _, item := range t {
			switch node := item.(type) {
			case string:
				total += utf8.RuneCountInString(node)
			case map[string]any:
				// A message item wraps content; an input_text/output_text item
				// carries the text directly.
				total += contentRunes(node["content"])
				if s, ok := node["text"].(string); ok {
					total += utf8.RuneCountInString(s)
				}
			}
		}
		return total
	}
	return 0
}

// contentRunes counts the runes in a content value that may be a string or an
// array of content blocks. Only text-bearing blocks (type "text" or
// input_text/output_text) are counted; metadata and images are ignored.
func contentRunes(v any) int {
	switch t := v.(type) {
	case string:
		return utf8.RuneCountInString(t)
	case []any:
		total := 0
		for _, block := range t {
			b, ok := block.(map[string]any)
			if !ok {
				if s, ok := block.(string); ok {
					total += utf8.RuneCountInString(s)
				}
				continue
			}
			if s, ok := b["text"].(string); ok {
				total += utf8.RuneCountInString(s)
			}
		}
		return total
	}
	return 0
}

// stringRunes counts runes in a plain string or an array of strings.
func stringRunes(v any) int {
	switch t := v.(type) {
	case string:
		return utf8.RuneCountInString(t)
	case []any:
		total := 0
		for _, item := range t {
			if s, ok := item.(string); ok {
				total += utf8.RuneCountInString(s)
			}
		}
		return total
	}
	return 0
}
