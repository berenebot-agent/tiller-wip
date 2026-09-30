"""Synchronous httpx transport with bounded I/O and connection-local catalog cache."""

import copy
import json
import math
import time
from typing import TYPE_CHECKING, Any
from urllib.parse import unquote, urljoin

import httpx

from .errors import GatewayError, response_error, request_id
from .normalize import chat_result, model
from .types import Catalog, ChatResult, Limits, Model, ModelFilter

if TYPE_CHECKING:
    from .stream import ChatStream


def decode(data: bytes | str) -> Any:
    try:
        return json.loads(data, parse_constant=lambda _: (_ for _ in ()).throw(ValueError()))
    except (ValueError, UnicodeError, RecursionError):
        raise GatewayError("malformed_response") from None


class GatewayClient:
    """One principal's Chat connection; use a new client when credentials change."""
    def __init__(self, profile: str, base_url: str, api_key: str, *,
                 http_client: httpx.Client | None = None,
                 timeout: float | httpx.Timeout = 60.0, limits: Limits | None = None,
                 catalog_path: str | None = None, cache_ttl: float = 300.0):
        if profile not in ("tiller", "openrouter"):
            raise GatewayError("invalid_request")
        if not isinstance(api_key, str) or not api_key or any(ord(c) < 33 or ord(c) > 126 for c in api_key):
            raise GatewayError("invalid_request")
        try:
            url = httpx.URL(base_url)
            if url.scheme not in ("http", "https") or not url.host or url.userinfo or url.fragment or url.query:
                raise ValueError
            if any(ord(c) < 33 for c in base_url):
                raise ValueError
            timeout_obj = timeout if isinstance(timeout, httpx.Timeout) else httpx.Timeout(timeout)
            if any(v is None or not math.isfinite(v) or v <= 0 for v in timeout_obj.as_dict().values()):
                raise ValueError
            if not math.isfinite(cache_ttl) or cache_ttl < 0:
                raise ValueError
        except (ValueError, TypeError, httpx.InvalidURL):
            raise GatewayError("invalid_request") from None
        self._profile = profile
        self._base_url = str(url).rstrip("/") + "/"
        self._api_key = api_key
        self._timeout = timeout_obj
        self.limits = limits or Limits()
        path = catalog_path if catalog_path is not None else ("models/user" if profile == "openrouter" else "models")
        if not isinstance(path, str) or not path:
            raise GatewayError("invalid_request")
        decoded_path = path
        for _ in range(len(path)):
            decoded = unquote(decoded_path)
            if decoded == decoded_path:
                break
            decoded_path = decoded
        if decoded_path.startswith("/") or any(c in decoded_path for c in ":?#\\") or any(ord(c) < 33 or ord(c) == 127 for c in decoded_path) or any(segment in (".", "..") for segment in decoded_path.split("/")):
            raise GatewayError("invalid_request")
        self._catalog_url = self._url(path)
        self._owned = http_client is None
        self._http = http_client or httpx.Client(timeout=timeout_obj, follow_redirects=False)
        self._catalog: Catalog | None = None
        self._cache_ttl = cache_ttl
        self._cached_at_monotonic: float | None = None
        self._closed = False
        self._streams: set[Any] = set()

    def __repr__(self) -> str:
        return f"GatewayClient(profile={self._profile!r}, api_key=<redacted>)"

    def __enter__(self) -> "GatewayClient":
        self._check_open()
        return self

    def __exit__(self, *args: Any) -> None:
        self.close()

    def close(self) -> None:
        """Close active responses and only the HTTP client created by this SDK."""
        if not self._closed:
            self._closed = True
            for stream in tuple(self._streams):
                stream.close()
            if self._owned:
                self._http.close()

    def _check_open(self) -> None:
        if self._closed:
            raise GatewayError("invalid_request")

    def _url(self, path: str) -> str:
        url = httpx.URL(urljoin(self._base_url, path))
        base = httpx.URL(self._base_url)
        if (url.scheme, url.host, url.port) != (base.scheme, base.host, base.port) or url.userinfo or url.fragment:
            raise GatewayError("invalid_request")
        return str(url)

    def _send(self, method: str, url: str, body: dict[str, Any] | None = None) -> httpx.Response:
        self._check_open()
        try:
            request = self._http.build_request(method, url, json=body,
                                               headers={"Authorization": "Bearer " + self._api_key,
                                                        "Accept": "text/event-stream" if body and body.get("stream") else "application/json"},
                                               timeout=self._timeout)
            return self._http.send(request, stream=True, follow_redirects=False, auth=None)
        except httpx.TimeoutException:
            raise GatewayError("timeout") from None
        except httpx.RequestError:
            raise GatewayError("transport") from None
        except (TypeError, ValueError, OverflowError):
            raise GatewayError("invalid_request") from None

    def _read(self, response: httpx.Response) -> bytes:
        data = bytearray()
        try:
            for chunk in response.iter_bytes():
                if len(data) + len(chunk) > self.limits.body_bytes:
                    raise GatewayError("malformed_response")
                data.extend(chunk)
            return bytes(data)
        except httpx.TimeoutException:
            raise GatewayError("timeout") from None
        except httpx.RequestError:
            raise GatewayError("transport") from None
        finally:
            response.close()

    def _status(self, response: httpx.Response) -> None:
        if not 200 <= response.status_code < 300:
            payload = None
            try:
                payload = decode(self._read(response))
            except GatewayError as error:
                if error.category in ("transport", "timeout", "cancelled"):
                    raise
            raise response_error(response.status_code, payload, response.headers, self._api_key)

    def _json(self, method: str, url: str, body: dict[str, Any] | None = None) -> tuple[Any, str | None]:
        response = self._send(method, url, body)
        self._status(response)
        media_type = response.headers.get("content-type", "").split(";", 1)[0].strip().lower()
        if media_type != "application/json" and not (media_type.startswith("application/") and media_type.endswith("+json")):
            response.close()
            raise GatewayError("malformed_response")
        data = decode(self._read(response))
        if isinstance(data, dict) and data.get("error") is not None:
            raise response_error(response.status_code, data, response.headers, self._api_key)
        return data, request_id(response.headers, self._api_key)

    @property
    def cached_catalog(self) -> Catalog | None:
        """Return an isolated last-success snapshot, even if expired or refresh failed."""
        return copy.deepcopy(self._catalog)

    def list_models(self, requirements: ModelFilter | None = None, *, refresh: bool = False) -> tuple[Model, ...]:
        """Fetch on expiry (300s by default); cache_ttl=0 disables cache reuse."""
        self._check_open()
        if refresh or self._catalog is None or self._cached_at_monotonic is None or time.monotonic() - self._cached_at_monotonic >= self._cache_ttl:
            self.refresh_models()
        catalog = self.cached_catalog
        assert catalog is not None
        return catalog.models if requirements is None else catalog.filter(requirements)

    def refresh_models(self) -> Catalog:
        """Force discovery; failures raise without replacing the last successful cache."""
        url = self._catalog_url
        seen = set()
        models = []
        total_bytes = 0
        for _ in range(self.limits.catalog_pages):
            if url in seen:
                raise GatewayError("malformed_response")
            seen.add(url)
            data, _ = self._json("GET", url)
            if not isinstance(data, dict) or not isinstance(data.get("data"), list):
                raise GatewayError("malformed_response")
            total_bytes += len(json.dumps(data).encode())
            if total_bytes > self.limits.body_bytes:
                raise GatewayError("malformed_response")
            models.extend(model(item, self._profile) for item in data["data"])
            links = data.get("links")
            next_page = links.get("next") if isinstance(links, dict) else None
            if not next_page:
                self._catalog = Catalog(tuple(models), time.time())
                self._cached_at_monotonic = time.monotonic()
                return copy.deepcopy(self._catalog)
            if not isinstance(next_page, str):
                raise GatewayError("malformed_response")
            url = self._url(urljoin(url, next_page))
        raise GatewayError("malformed_response")

    def _body(self, model: str, messages: list[dict[str, Any]], stream: bool, *,
              tools: Any = None, response_format: Any = None, max_tokens: int | None = None,
              temperature: float | None = None, top_p: float | None = None, reasoning: Any = None,
              tool_choice: Any = None, extra_body: dict[str, Any] | None = None, **extensions: Any) -> dict[str, Any]:
        if not isinstance(model, str) or not model.strip() or not isinstance(messages, list) or not messages:
            raise GatewayError("invalid_request")
        core = {"model", "messages", "stream", "tools", "response_format", "max_tokens",
                "temperature", "top_p", "reasoning", "tool_choice", "stream_options"}
        forbidden = {"authorization", "api_key", "headers", "base_url"}
        if extra_body is not None and not isinstance(extra_body, dict):
            raise GatewayError("invalid_request")
        extra = dict(extra_body or {})
        if set(extra) & set(extensions):
            raise GatewayError("invalid_request")
        extra.update(extensions)
        if any(not isinstance(k, str) or k.lower() in core | forbidden for k in extra):
            raise GatewayError("invalid_request")
        body = {"model": model, "messages": messages, "stream": stream}
        for key, value in (("tools", tools), ("response_format", response_format), ("max_tokens", max_tokens),
                           ("temperature", temperature), ("top_p", top_p), ("reasoning", reasoning), ("tool_choice", tool_choice)):
            if value is not None:
                body[key] = value
        if stream:
            body["stream_options"] = {"include_usage": True}
        body.update(extra)
        try:
            encoded = json.dumps(body, allow_nan=False).encode()
            if len(encoded) > self.limits.body_bytes:
                raise ValueError
        except (TypeError, ValueError, OverflowError, RecursionError, UnicodeError):
            raise GatewayError("invalid_request") from None
        return body

    def chat(self, model: str, messages: list[dict[str, Any]], **kwargs: Any) -> ChatResult:
        """Send native Chat JSON; never refresh models, retry, or execute returned tools."""
        body = self._body(model, messages, False, **kwargs)
        data, request_id = self._json("POST", self._url("chat/completions"), body)
        result = chat_result(data, request_id)
        if result.finish_reason == "error":
            raise GatewayError("http", status=200, request_id=request_id)
        try:
            if sum(len(call.arguments.encode()) for call in result.tool_calls) > self.limits.argument_bytes:
                raise GatewayError("malformed_response")
        except UnicodeError:
            raise GatewayError("malformed_response") from None
        return result

    def stream(self, model: str, messages: list[dict[str, Any]], **kwargs: Any) -> "ChatStream":
        """Create a stream to be opened with with; done is emitted only for [DONE]."""
        from .stream import ChatStream
        self._check_open()
        return ChatStream(self, self._body(model, messages, True, **kwargs))
