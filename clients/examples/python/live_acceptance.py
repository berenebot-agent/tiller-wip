import argparse
import base64
import json
import os
from pathlib import Path
import stat

from tiller_gateway import GatewayClient, GatewayError
from billbot_adapter import image_message, json_chat, text_message, tool_roundtrip


def protected_config(path: Path) -> dict:
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077 or info.st_size > 16384:
            raise ValueError("Protected configuration required")
        with os.fdopen(fd, "r", encoding="utf-8", closefd=False) as file:
            config = json.load(file)
        if not isinstance(config, dict) or any(not isinstance(config.get(key), str) for key in ("base_url", "api_key", "model")):
            raise ValueError("Invalid configuration")
        if config["model"] != "virtual/free":
            raise ValueError("Acceptance requires virtual/free")
        return config
    finally:
        os.close(fd)


def report(name, result):
    usage = result.usage
    print(json.dumps({"check": name, "status": "passed", "tool_calls": len(result.tool_calls),
                      "usage": None if usage is None else {
                          "prompt_tokens": usage.prompt_tokens, "completion_tokens": usage.completion_tokens,
                          "total_tokens": usage.total_tokens}}))


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("config", type=Path)
    parser.add_argument("--features", action="store_true")
    args = parser.parse_args()
    try:
        config = protected_config(args.config)
        with GatewayClient("tiller", config["base_url"], config["api_key"]) as client:
            models = client.list_models()
            print(json.dumps({"check": "catalog", "status": "passed", "count": len(models)}))
            model = next((m for m in models if m.id == config["model"]), None)
            if model is None:
                raise ValueError("Model not available")
            messages = [text_message("Reply with exactly OK")]
            reply = client.chat(model.id, messages, max_tokens=128, temperature=0)
            if not isinstance(reply.content, str) or not reply.content.strip():
                raise ValueError("Chat acceptance failed")
            report("chat", reply)
            text = []
            count = 0
            done = False
            accounting = None
            with client.stream(model.id, messages, max_tokens=128, temperature=0) as stream:
                for event in stream:
                    count += 1
                    if event.kind == "text":
                        text.append(event.text)
                    if event.kind == "usage":
                        accounting = event.usage
                    done = done or event.kind == "done"
            if not done or not "".join(text).strip():
                raise ValueError("Stream acceptance failed")
            print(json.dumps({"check": "stream", "status": "passed", "events": count,
                              "usage": None if accounting is None else accounting.total_tokens}))
            if args.features:
                baseline = client.chat(model.id, [text_message('Return a JSON object with status equal to OK')],
                                       response_format={"type": "json_object"}, max_tokens=256)
                if not isinstance(json.loads(baseline.content), dict):
                    raise ValueError("JSON object acceptance failed")
                report("json_object", baseline)
                for feature, support in (("tools", model.tools), ("json", model.structured_output), ("image", model.vision)):
                    if support is not True:
                        print(json.dumps({"check": feature, "status": "skipped", "support": support}))
                        continue
                    if feature == "tools":
                        tool = {"type": "function", "function": {"name": "acceptance_ok", "description": "Return OK",
                                "parameters": {"type": "object", "properties": {}, "additionalProperties": False}}}
                        calls = []
                        def execute(name, arguments):
                            if arguments:
                                raise ValueError("Unexpected arguments")
                            calls.append(name)
                            return {"status": "OK"}
                        result = tool_roundtrip(client, model.id, [text_message("Call acceptance_ok once, then reply OK")], tool, execute)
                        if not calls:
                            raise ValueError("Tool acceptance failed")
                    elif feature == "json":
                        schema = {"type": "object", "properties": {"status": {"type": "string", "enum": ["OK"]}},
                                  "required": ["status"], "additionalProperties": False}
                        result = json_chat(client, model.id, [text_message('Return JSON with status equal to OK')], schema)
                        if json.loads(result.content) != {"status": "OK"}:
                            raise ValueError("JSON acceptance failed")
                    else:
                        png = base64.b64decode((Path(__file__).resolve().parents[2] / "fixtures" / "pixel.png.b64").read_text())
                        url = "data:image/png;base64," + base64.b64encode(png).decode()
                        result = client.chat(model.id, [image_message("If you can read this image, reply with exactly OK", url)], max_tokens=128)
                        if not isinstance(result.content, str) or not result.content.strip():
                            raise ValueError("Image acceptance failed")
                    report(feature, result)
        return 0
    except GatewayError as error:
        print(json.dumps({"status": "failed", "category": error.category, "http_status": error.status}))
    except (OSError, ValueError, TypeError, KeyError):
        print(json.dumps({"status": "failed", "category": "acceptance"}))
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
