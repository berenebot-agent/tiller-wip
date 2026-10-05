"""Fixed-message errors that never include provider prose or request bodies."""

import re
from typing import Any


_MESSAGES = {
    "invalid_request": "Gateway request is invalid",
    "authentication": "Gateway authentication failed",
    "authorization": "Gateway access denied",
    "billing": "Gateway billing requirement not met",
    "model_unavailable": "Gateway model is unavailable",
    "rate_limit": "Gateway rate limit exceeded",
    "context_limit": "Gateway context limit exceeded",
    "unsupported_feature": "Gateway feature is unsupported",
    "transport": "Gateway transport failed",
    "timeout": "Gateway request timed out",
    "http": "Gateway HTTP request failed",
    "stream": "Gateway stream failed",
    "malformed_response": "Gateway response is malformed",
    "cancelled": "Gateway stream was cancelled",
}
_CODES = {
    "invalid_request_error": "invalid_request",
    "invalid_request": "invalid_request",
    "invalid_api_key": "authentication",
    "authentication_error": "authentication",
    "permission_error": "authorization",
    "permission_denied": "authorization",
    "model_not_found": "model_unavailable",
    "model_unavailable": "model_unavailable",
    "rate_limit_exceeded": "rate_limit",
    "rate_limit_error": "rate_limit",
    "context_length_exceeded": "context_limit",
    "context_limit_exceeded": "context_limit",
    "unsupported_feature": "unsupported_feature",
    "unsupported_parameter": "unsupported_feature",
    "server_error": "http",
    "insufficient_quota": "billing",
    "timeout": "timeout",
    "request_timeout": "timeout",
}


def safe_identifier(value: Any, secret: str = "") -> str | None:
    if not isinstance(value, str) or not re.fullmatch(r"[A-Za-z0-9_.:-]{1,128}", value):
        return None
    if secret and secret in value:
        return None
    return value


def request_id(headers: Any, secret: str) -> str | None:
    for name in ("x-request-id", "request-id", "x-generation-id", "x-tiller-request-id"):
        value = safe_identifier(headers.get(name), secret)
        if value is not None:
            return value
    return None


class GatewayError(Exception):
    """Safe category/status/code metadata; Retry-After never triggers an SDK retry."""
    def __init__(self, category: str, *, status: int | None = None,
                 code: str | None = None, error_type: str | None = None,
                 request_id: str | None = None, retry_after: str | None = None):
        self.category = category
        self.status = status
        self.code = code
        self.error_type = error_type
        self.request_id = request_id
        self.retry_after = retry_after
        super().__init__(_MESSAGES[category])


def response_error(status: int, payload: Any, headers: Any, secret: str,
                   default: str = "http") -> GatewayError:
    error = payload.get("error", {}) if isinstance(payload, dict) else {}
    error = error if isinstance(error, dict) else {}
    code = error.get("code")
    error_type = error.get("type")
    category = {401: "authentication", 403: "authorization", 402: "billing",
                404: "model_unavailable", 429: "rate_limit", 400: "invalid_request",
                422: "invalid_request", 408: "timeout", 504: "timeout"}.get(status, default)
    if status not in (401, 403, 402, 429, 408, 504):
        if isinstance(error_type, str):
            category = _CODES.get(error_type, category)
        if isinstance(code, str):
            category = _CODES.get(code, category)
    retry = headers.get("retry-after")
    if not isinstance(retry, str) or not re.fullmatch(r"[A-Za-z0-9 ,:+-]{1,128}", retry) or (secret and secret in retry):
        retry = None
    return GatewayError(category, status=status,
                        code=safe_identifier(code, secret) if isinstance(code, str) and code in _CODES else None,
                        error_type=safe_identifier(error_type, secret) if isinstance(error_type, str) and error_type in _CODES else None,
                        request_id=request_id(headers, secret),
                        retry_after=retry)
