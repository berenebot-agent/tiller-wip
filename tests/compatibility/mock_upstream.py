import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# Optional extra upstream models that can be added/removed at runtime to
# simulate catalogue discovery changes. The default catalogue is unaffected
# until a browser/compatibility test adds a model.
_extra_models = []

# Model ids that should return HTTP 500 for chat completions (added/removed via
# the /__/models/fail and /__/models/ok controls). Lets tests exercise upstream
# failure + ordered fallback without a second mock upstream.
_failing_models = []


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_args):
        pass

    def send_json(self, value, status=200):
        body = json.dumps(value, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def catalogue(self):
        ids = ["mock-model"]
        for mid in _extra_models:
            if mid not in ids:
                ids.append(mid)
        return {"object": "list", "data": [{"id": mid} for mid in ids]}

    def do_GET(self):
        if self.path == "/v1/models":
            self.send_json(self.catalogue())
            return
        self.send_json({"error": "not found"}, 404)

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        request = json.loads(self.rfile.read(length) or b"{}")
        if self.path.startswith("/__/models/"):
            self.handle_model_control()
            return
        if self.path == "/v1/responses":
            self.handle_responses(request)
            return
        if self.path == "/v1/messages":
            self.handle_messages(request)
            return
        if self.path != "/v1/chat/completions":
            self.send_json({"error": "not found"}, 404)
            return
        model = request.get("model", "mock-model")
        if model in _failing_models:
            self.send_json({"error": {"message": "injected upstream failure", "type": "server_error"}}, 500)
            return
        if request.get("stream"):
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.end_headers()
            chunks = [
                {"id": "chatcmpl_mock", "object": "chat.completion.chunk", "created": 1, "model": model, "choices": [{"index": 0, "delta": {"role": "assistant", "content": "hello"}, "finish_reason": None}]},
                {"id": "chatcmpl_mock", "object": "chat.completion.chunk", "created": 1, "model": model, "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]},
            ]
            for chunk in chunks:
                self.wfile.write(b"data: " + json.dumps(chunk, separators=(",", ":")).encode() + b"\n\n")
                self.wfile.flush()
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
            return
        self.send_json({
            "id": "chatcmpl_mock",
            "object": "chat.completion",
            "created": 1,
            "model": model,
            "choices": [{"index": 0, "message": {"role": "assistant", "content": "hello"}, "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
        })

    def handle_model_control(self):
        # /__/models/add/{id}, /__/models/remove/{id}, /__/models/fail/{id},
        # /__/models/ok/{id}
        parts = self.path.split("/")
        if len(parts) < 5:
            self.send_json({"error": "missing model id"}, 400)
            return
        action, model_id = parts[3], parts[4]
        if not model_id:
            self.send_json({"error": "missing model id"}, 400)
            return
        if action == "add":
            if model_id not in _extra_models:
                _extra_models.append(model_id)
            self.send_json({"status": "added", "id": model_id})
            return
        if action == "remove":
            _extra_models[:] = [m for m in _extra_models if m != model_id]
            self.send_json({"status": "removed", "id": model_id})
            return
        if action == "fail":
            if model_id not in _failing_models:
                _failing_models.append(model_id)
            self.send_json({"status": "failing", "id": model_id})
            return
        if action == "ok":
            _failing_models[:] = [m for m in _failing_models if m != model_id]
            self.send_json({"status": "ok", "id": model_id})
            return
        self.send_json({"error": "unknown action"}, 400)

    def handle_responses(self, request):
        model = request.get("model", "mock-model")
        response = {
            "id": "resp_mock",
            "object": "response",
            "created_at": 1,
            "status": "completed",
            "model": model,
            "output": [{"id": "msg_mock", "type": "message", "role": "assistant", "status": "completed", "content": [{"type": "output_text", "text": "hello", "annotations": []}]}],
            "usage": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
        }
        if not request.get("stream"):
            self.send_json(response)
            return
        in_progress = dict(response)
        in_progress["status"] = "in_progress"
        in_progress["output"] = []
        events = [
            ("response.created", {"type": "response.created", "response": in_progress}),
            ("response.output_item.added", {"type": "response.output_item.added", "output_index": 0, "item": {"id": "msg_mock", "type": "message", "role": "assistant", "status": "in_progress", "content": []}}),
            ("response.content_part.added", {"type": "response.content_part.added", "output_index": 0, "content_index": 0, "part": {"type": "output_text", "text": "", "annotations": []}}),
            ("response.output_text.delta", {"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "delta": "hello"}),
            ("response.output_text.done", {"type": "response.output_text.done", "output_index": 0, "content_index": 0, "text": "hello"}),
            ("response.content_part.done", {"type": "response.content_part.done", "output_index": 0, "content_index": 0, "part": {"type": "output_text", "text": "hello", "annotations": []}}),
            ("response.output_item.done", {"type": "response.output_item.done", "output_index": 0, "item": response["output"][0]}),
            ("response.completed", {"type": "response.completed", "response": response}),
        ]
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()
        for event, data in events:
            body = json.dumps(data, separators=(",", ":")).encode()
            self.wfile.write(f"event: {event}\n".encode() + b"data: " + body + b"\n\n")
            self.wfile.flush()
        self.close_connection = True

    def handle_messages(self, request):
        model = request.get("model", "mock-model")
        message = {
            "id": "msg_mock",
            "type": "message",
            "role": "assistant",
            "model": model,
            "content": [{"type": "text", "text": "hello"}],
            "stop_reason": "end_turn",
            "stop_sequence": None,
            "usage": {"input_tokens": 1, "output_tokens": 1},
        }
        if not request.get("stream"):
            self.send_json(message)
            return
        events = [
            ("message_start", {"type": "message_start", "message": {**message, "content": [], "stop_reason": None, "usage": {"input_tokens": 1, "output_tokens": 0}}}),
            ("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}}),
            ("content_block_delta", {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "hello"}}),
            ("content_block_stop", {"type": "content_block_stop", "index": 0}),
            ("message_delta", {"type": "message_delta", "delta": {"stop_reason": "end_turn", "stop_sequence": None}, "usage": {"output_tokens": 1}}),
            ("message_stop", {"type": "message_stop"}),
        ]
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()
        for event, data in events:
            body = json.dumps(data, separators=(",", ":")).encode()
            self.wfile.write(f"event: {event}\n".encode() + b"data: " + body + b"\n\n")
            self.wfile.flush()
        self.close_connection = True


ThreadingHTTPServer(("127.0.0.1", int(os.environ.get("TILLER_MOCK_PORT", "18081"))), Handler).serve_forever()
