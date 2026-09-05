package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

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
	case "model_not_found":
		return "The requested model was not found or is not configured"
	case "database_error":
		return "Internal database error while resolving the request"
	case "client_cancelled":
		return "The client cancelled the request"
	case "client_timeout":
		return "The client request timed out"
	case "translation_error":
		return "Failed to translate the request for the upstream provider"
	case "unsupported_feature":
		return "The request uses a feature not supported by the upstream provider"
	case "invalid_request":
		return "The request was rejected as invalid"
	case "upstream_error":
		return "The upstream provider returned an error"
	case "protocol_unavailable":
		return "No compatible protocol is available for this request"
	case "virtual_model_unavailable":
		return "All targets for the virtual model are currently unavailable"
	case "model_unavailable":
		return "The requested model is currently unavailable"
	case "invalid_upstream":
		return "The upstream provider endpoint could not be resolved"
	default:
		return httpStatusErrorMessage(class)
	}
}

// httpStatusErrorMessage translates a dynamic http_<code> failure class
// (e.g. http_429) into "HTTP 429: Too Many Requests" using the standard
// library's status text. Unknown codes fall back to the numeric form and
// anything that isn't an http_ class returns "".
func httpStatusErrorMessage(class string) string {
	code, ok := strings.CutPrefix(class, "http_")
	if !ok {
		return ""
	}
	status, err := strconv.Atoi(code)
	if err != nil {
		return ""
	}
	if text := http.StatusText(status); text != "" {
		return fmt.Sprintf("HTTP %d: %s", status, text)
	}
	return fmt.Sprintf("HTTP %d", status)
}

func strPtrIfNonEmpty(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
