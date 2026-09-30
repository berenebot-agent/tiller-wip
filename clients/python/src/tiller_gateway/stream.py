"""Incremental bounded SSE parsing with explicit upstream-response ownership."""

from dataclasses import replace
from typing import Any, Iterator

import httpx

from .client import decode
from .errors import GatewayError, response_error, request_id
from .normalize import usage
from .types import StreamEvent


def frames(response: httpx.Response, limits: Any) -> Iterator[str]:
    line = bytearray()
    data: list[bytes] = []
    total = event_size = 0
    after_cr = False
    for chunk in response.iter_bytes():
        total += len(chunk)
        if total > limits.body_bytes:
            raise GatewayError("stream")
        for byte in chunk:
            if after_cr and byte == 10:
                after_cr = False
                continue
            after_cr = False
            if byte not in (10, 13):
                line.append(byte)
                if len(line) > limits.line_bytes:
                    raise GatewayError("stream")
                continue
            after_cr = byte == 13
            value = bytes(line)
            line.clear()
            if not value:
                if data:
                    try:
                        yield b"\n".join(data).decode("utf-8")
                    except UnicodeError:
                        raise GatewayError("malformed_response") from None
                data.clear()
                event_size = 0
            elif value.startswith(b"data:") or value == b"data":
                part = value.partition(b":")[2]
                if part.startswith(b" "):
                    part = part[1:]
                event_size += len(part) + 1
                if event_size > limits.event_bytes:
                    raise GatewayError("stream")
                data.append(part)
    pending = b"\n".join(data + ([bytes(line).removeprefix(b"data:").lstrip(b" ")] if line.startswith(b"data:") else []))
    if pending == b"[DONE]":
        yield "[DONE]"


class ChatStream:
    """Context-managed event iterator; early close cancels without marking done."""
    def __init__(self, client: Any, body: dict[str, Any]):
        self._client = client
        self._body = body
        self._response: httpx.Response | None = None
        self._iterator: Iterator[StreamEvent] | None = None
        self._closed = False
        self._done = False
        self.request_id: str | None = None

    def __repr__(self) -> str:
        return "ChatStream()"

    def __enter__(self) -> "ChatStream":
        if self._closed or self._response is not None:
            raise GatewayError("invalid_request")
        response = self._client._send("POST", self._client._url("chat/completions"), self._body)
        self._body = {}
        self._response = response
        self.request_id = request_id(response.headers, self._client._api_key)
        try:
            self._client._status(response)
            if response.headers.get("content-type", "").split(";", 1)[0].strip().lower() != "text/event-stream":
                raise GatewayError("malformed_response")
        except BaseException:
            self.close()
            raise
        self._client._streams.add(self)
        self._iterator = self._events()
        return self

    def __exit__(self, *args: Any) -> None:
        self.close()

    def close(self) -> None:
        """Close upstream immediately; injected HTTP clients remain caller-owned."""
        if not self._closed:
            self._closed = True
            self._body = {}
            if self._response is not None:
                self._response.close()
            self._client._streams.discard(self)

    def __iter__(self) -> "ChatStream":
        return self

    def __next__(self) -> StreamEvent:
        if self._done:
            raise StopIteration
        if self._closed:
            raise GatewayError("cancelled", request_id=self.request_id)
        if self._iterator is None:
            raise GatewayError("invalid_request")
        try:
            event = next(self._iterator)
            return replace(event, request_id=self.request_id)
        except StopIteration:
            self.close()
            raise
        except httpx.TimeoutException:
            self.close()
            raise GatewayError("timeout", request_id=self.request_id) from None
        except httpx.RequestError:
            self.close()
            raise GatewayError("transport", request_id=self.request_id) from None
        except (KeyError, TypeError, ValueError, IndexError, AttributeError):
            self.close()
            raise GatewayError("malformed_response", request_id=self.request_id) from None
        except GatewayError as error:
            self.close()
            error.request_id = error.request_id or self.request_id
            raise
        except BaseException:
            self.close()
            raise

    def _events(self) -> Iterator[StreamEvent]:
        assert self._response is not None
        argument_bytes = 0
        for frame in frames(self._response, self._client.limits):
            if frame == "[DONE]":
                self._done = True
                self.close()
                yield StreamEvent("done", raw_frame=frame)
                return
            native = decode(frame)
            if not isinstance(native, dict):
                raise GatewayError("malformed_response")
            if native.get("error") is not None:
                raise response_error(200, native, self._response.headers, self._client._api_key, "stream")
            choices = native.get("choices")
            if not isinstance(choices, list):
                raise GatewayError("malformed_response")
            events = []
            for choice in choices:
                if not isinstance(choice, dict):
                    raise GatewayError("malformed_response")
                if "delta" not in choice and choice.get("finish_reason") is None:
                    raise GatewayError("malformed_response")
                index = choice.get("index", 0)
                if type(index) is not int or index < 0:
                    raise GatewayError("malformed_response")
                delta = choice.get("delta", {})
                if not isinstance(delta, dict):
                    raise GatewayError("malformed_response")
                finish = choice.get("finish_reason")
                if finish == "error":
                    raise GatewayError("stream", status=200)
                for key, kind in (("content", "text"), ("reasoning_content", "reasoning"), ("reasoning", "reasoning")):
                    text = delta.get(key)
                    if text is not None and not isinstance(text, str):
                        raise GatewayError("malformed_response")
                    if text:
                        events.append(StreamEvent(kind, text=text, choice_index=index, raw=native, raw_frame=frame))
                if delta.get("reasoning_details") is not None:
                    events.append(StreamEvent("reasoning", choice_index=index, reasoning_details=delta["reasoning_details"],
                                              raw=native, raw_frame=frame))
                calls = delta.get("tool_calls", [])
                if not isinstance(calls, list):
                    raise GatewayError("malformed_response")
                for call in calls:
                    function = call.get("function", {})
                    arguments = function.get("arguments", "")
                    tool_index = call.get("index")
                    if not isinstance(arguments, str) or type(tool_index) is not int or tool_index < 0:
                        raise GatewayError("malformed_response")
                    argument_bytes += len(arguments.encode())
                    if argument_bytes > self._client.limits.argument_bytes:
                        raise GatewayError("stream")
                    events.append(StreamEvent("tool", choice_index=index, tool_index=tool_index,
                                              tool_id=call.get("id"), tool_name=function.get("name"),
                                              arguments=arguments, raw=native, raw_frame=frame))
                if finish is not None:
                    if not isinstance(finish, str):
                        raise GatewayError("malformed_response")
                    events.append(StreamEvent("finish", choice_index=index, finish_reason=finish, raw=native, raw_frame=frame))
            if native.get("usage") is not None:
                events.append(StreamEvent("usage", usage=usage(native["usage"]), raw=native, raw_frame=frame))
            if not events:
                events.append(StreamEvent("frame", raw=native, raw_frame=frame))
            yield from events
        raise GatewayError("stream", status=200)
