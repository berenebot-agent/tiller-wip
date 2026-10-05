"""Run catalog, Chat and streaming with environment configuration; print summaries."""

import json
import os

from tiller_gateway import GatewayClient, GatewayError


def main() -> int:
    """Use TILLER_BASE_URL, TILLER_API_KEY, TILLER_MODEL and optional TILLER_PROFILE."""
    try:
        with GatewayClient(os.environ.get("TILLER_PROFILE", "tiller"),
                           os.environ["TILLER_BASE_URL"], os.environ["TILLER_API_KEY"]) as client:
            models = client.list_models()
            print(json.dumps({"operation": "catalog", "count": len(models)}))
            model = os.environ["TILLER_MODEL"]
            messages = [{"role": "user", "content": "Reply with exactly OK"}]
            reply = client.chat(model, messages, max_tokens=128)
            print(json.dumps({"operation": "chat", "status": "received", "tool_calls": len(reply.tool_calls),
                              "characters": len(reply.content) if isinstance(reply.content, str) else 0,
                              "total_tokens": reply.usage.total_tokens if reply.usage else None}))
            events = characters = 0
            done = False
            total_tokens = None
            with client.stream(model, messages, max_tokens=128) as stream:
                for event in stream:
                    events += 1
                    if event.kind == "text":
                        characters += len(event.text or "")
                    elif event.kind == "usage":
                        total_tokens = event.usage.total_tokens
                    elif event.kind == "done":
                        done = True
            print(json.dumps({"operation": "stream", "events": events, "characters": characters,
                              "done": done, "total_tokens": total_tokens}))
        return 0
    except GatewayError as error:
        print(json.dumps({"status": "failed", "category": error.category, "http_status": error.status}))
    except KeyError:
        print(json.dumps({"status": "failed", "category": "missing_configuration"}))
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
