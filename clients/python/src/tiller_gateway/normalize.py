"""Conservative metadata normalization and strict completed-response validation."""

import json
from typing import Any

from .errors import GatewayError
from .types import ChatResult, Model, Reasoning, ToolCall, Usage


def flag(value: Any) -> bool | None:
    if isinstance(value, bool):
        return value
    if type(value) is int and value in (0, 1):
        return bool(value)
    return None


def number(value: Any) -> int | None:
    return value if type(value) is int and value >= 0 else None


def strings(value: Any) -> tuple[str, ...] | None:
    return tuple(value) if isinstance(value, list) and all(isinstance(v, str) for v in value) else None


def model(value: Any, profile: str) -> Model:
    if not isinstance(value, dict) or not isinstance(value.get("id"), str) or not value["id"]:
        raise GatewayError("malformed_response")
    architecture = value.get("architecture")
    architecture = architecture if isinstance(architecture, dict) else {}
    inputs = strings(value.get("input_modalities", architecture.get("input_modalities")))
    outputs = strings(value.get("output_modalities", architecture.get("output_modalities")))
    params = strings(value.get("supported_parameters"))
    caps = {}
    for key, parameter in (("tools", "tools"), ("reasoning", "reasoning"),
                           ("structured_output", "structured_outputs"), ("vision", "image"),
                           ("json_object", "response_format")):
        explicit = "supports_" + key
        support = flag(value.get(explicit))
        if explicit not in value and profile == "openrouter":
            if key == "vision" and inputs is not None:
                support = "image" in inputs
            elif key != "vision" and params is not None:
                support = parameter in params
        caps[key] = support
    native = value.get("reasoning")
    native = native if isinstance(native, dict) else {}
    options = value.get("reasoning_options")
    options = options if isinstance(options, list) else []
    effort_option = next((o for o in options if isinstance(o, dict) and o.get("type") == "effort"), {})
    budget = next((o for o in options if isinstance(o, dict) and o.get("type") == "budget_tokens"), {})
    effort_known = "supported_efforts" in native or "values" in effort_option
    efforts = native.get("supported_efforts", effort_option.get("values"))
    effort_known = effort_known and (efforts is None or strings(efforts) is not None)
    budget_min = number(native.get("min_budget_tokens"))
    if budget_min is None:
        budget_min = number(native.get("budget_min", budget.get("min")))
    budget_max = number(native.get("max_budget_tokens"))
    if budget_max is None:
        budget_max = number(native.get("budget_max", budget.get("max")))
    supports_max_tokens = flag(native.get("supports_max_tokens"))
    if "supports_max_tokens" not in native and budget:
        supports_max_tokens = True
    toggle = flag(native.get("supports_toggle", native.get("toggle")))
    if "supports_toggle" not in native and "toggle" not in native and any(
        isinstance(o, dict) and o.get("type") in ("enabled", "toggle") for o in options
    ):
        toggle = True
    reasoning = Reasoning(
        efforts=strings(efforts), efforts_known=effort_known,
        budget_min=budget_min, budget_max=budget_max,
        supports_max_tokens=supports_max_tokens, toggle=toggle,
        modes=strings(native.get("modes")),
        default_effort=native.get("default_effort") if isinstance(native.get("default_effort"), str) else None,
        default_enabled=flag(native.get("default_enabled")),
        mandatory=flag(native.get("mandatory")), raw={"reasoning": native, "reasoning_options": options},
    )
    top = value.get("top_provider")
    top = top if isinstance(top, dict) else {}
    return Model(id=value["id"], name=value.get("name") if isinstance(value.get("name"), str) else None,
                 context_length=number(value.get("context_length")),
                 max_output_tokens=number(value.get("max_output_tokens", top.get("max_completion_tokens"))),
                 input_modalities=inputs, output_modalities=outputs,
                 reasoning_metadata=reasoning, raw=value, **caps)


def usage(value: Any) -> Usage | None:
    if value is None:
        return None
    if not isinstance(value, dict):
        raise GatewayError("malformed_response")
    details = value.get("prompt_tokens_details")
    details = details if isinstance(details, dict) else {}
    cached = number(details.get("cached_tokens"))
    if cached is None:
        input_details = value.get("input_tokens_details")
        cached = number(input_details.get("cached_tokens")) if isinstance(input_details, dict) else None
    read = number(value.get("cache_read_input_tokens"))
    if cached is None:
        cached = read
    return Usage(prompt_tokens=number(value.get("prompt_tokens", value.get("input_tokens"))),
                 completion_tokens=number(value.get("completion_tokens", value.get("output_tokens"))),
                 total_tokens=number(value.get("total_tokens")), raw=value,
                 cached_tokens=cached, cache_read_input_tokens=read,
                 cache_creation_input_tokens=number(value.get("cache_creation_input_tokens")))


def chat_result(value: Any, request_id: str | None) -> ChatResult:
    try:
        if not isinstance(value, dict) or not isinstance(value.get("choices"), list) or not value["choices"]:
            raise ValueError
        choice = value["choices"][0]
        if not isinstance(choice, dict):
            raise ValueError
        message = choice["message"]
        if not isinstance(message, dict) or message.get("role") != "assistant":
            raise ValueError
        content = message.get("content")
        if content is not None and not isinstance(content, (str, list)):
            raise ValueError
        if isinstance(content, list):
            for part in content:
                if not isinstance(part, dict) or not isinstance(part.get("type"), str):
                    raise ValueError
                if part["type"] == "text" and not isinstance(part.get("text"), str):
                    raise ValueError
        for key in ("reasoning", "reasoning_content"):
            if message.get(key) is not None and not isinstance(message[key], str):
                raise ValueError
        for key in ("model", "id"):
            if value.get(key) is not None and not isinstance(value[key], str):
                raise ValueError
        if choice.get("finish_reason") is not None and not isinstance(choice["finish_reason"], str):
            raise ValueError
        native_calls = message.get("tool_calls")
        if native_calls is not None and not isinstance(native_calls, list):
            raise ValueError
        calls = []
        for call in native_calls or []:
            if not isinstance(call, dict) or call.get("type") != "function":
                raise ValueError
            if not isinstance(call.get("id"), str) or not call["id"]:
                raise ValueError
            function = call["function"]
            if not isinstance(function, dict) or not isinstance(function.get("name"), str) or not function["name"]:
                raise ValueError
            arguments = function["arguments"]
            if not isinstance(arguments, str):
                raise ValueError
            parsed = json.loads(arguments, parse_constant=lambda _: (_ for _ in ()).throw(ValueError()))
            if not isinstance(parsed, dict):
                raise ValueError
            calls.append(ToolCall(call.get("id"), function.get("name"), arguments, call))
        return ChatResult(message.get("content"), tuple(calls), message,
                          message.get("reasoning_content", message.get("reasoning")),
                          value.get("model"), value.get("id"), choice.get("finish_reason"),
                          usage(value.get("usage")), request_id, value)
    except (KeyError, IndexError, TypeError, ValueError, UnicodeError, RecursionError):
        raise GatewayError("malformed_response") from None
