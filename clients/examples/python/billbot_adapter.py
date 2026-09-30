import json
from typing import Any, Callable

from tiller_gateway import ChatResult, GatewayClient


def text_message(text: str) -> dict[str, Any]:
    return {"role": "user", "content": text}


def image_message(text: str, image_url: str) -> dict[str, Any]:
    return {"role": "user", "content": [
        {"type": "text", "text": text},
        {"type": "image_url", "image_url": {"url": image_url}},
    ]}


def json_chat(client: GatewayClient, model: str, messages: list[dict[str, Any]], schema: dict[str, Any]) -> ChatResult:
    return client.chat(model, messages, response_format={
        "type": "json_schema", "json_schema": {"name": "billbot_reply", "strict": True, "schema": schema},
    }, max_tokens=256)


def tool_roundtrip(client: GatewayClient, model: str, messages: list[dict[str, Any]],
                   tool: dict[str, Any], execute: Callable[[str, dict[str, Any]], Any], *,
                   max_turns: int = 4, max_tool_calls: int = 8, max_result_bytes: int = 16384) -> ChatResult:
    if min(max_turns, max_tool_calls, max_result_bytes) <= 0:
        raise ValueError("Positive tool loop limits required")
    history = [{"role": "system", "content": "Use the supplied tool when requested. After receiving its result, give a brief final answer."}, *messages]
    calls = 0
    for turn in range(max_turns):
        reply = client.chat(model, history, tools=[tool], tool_choice="auto", max_tokens=256)
        if not reply.tool_calls:
            return reply
        if turn == max_turns - 1 or calls + len(reply.tool_calls) > max_tool_calls:
            raise ValueError("Tool loop limit exceeded")
        history.append(reply.assistant_message)
        for call in reply.tool_calls:
            if call.name != tool["function"]["name"] or not call.id:
                raise ValueError("Unexpected tool call")
            arguments = json.loads(call.arguments)
            if not isinstance(arguments, dict):
                raise ValueError("Invalid tool arguments")
            result = execute(call.name, arguments)
            content = json.dumps(result, allow_nan=False)
            if len(content.encode()) > max_result_bytes:
                raise ValueError("Tool result limit exceeded")
            history.append({"role": "tool", "tool_call_id": call.id, "content": content})
            calls += 1
    raise ValueError("Tool loop limit exceeded")
