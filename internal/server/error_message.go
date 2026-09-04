package server

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
