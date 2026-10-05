import json
from pathlib import Path
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import unittest
from unittest.mock import patch
import sys

import httpx

from tiller_gateway import GatewayClient, GatewayError, Limits, ModelFilter


FIXTURES = Path(__file__).resolve().parents[2] / "fixtures"
MESSAGES = [{"role": "user", "content": "hello"}]
sys.path.insert(0, str(FIXTURES.parent / "examples" / "python"))
from billbot_adapter import tool_roundtrip


class Fragmented(httpx.SyncByteStream):
    def __init__(self, data, failure=None):
        self.data = data
        self.closed = False
        self.failure = failure

    def __iter__(self):
        for byte in self.data:
            yield bytes([byte])
        if self.failure:
            raise self.failure

    def close(self):
        self.closed = True


class GatewayTests(unittest.TestCase):
    def client(self, handler, **kwargs):
        def typed_handler(request):
            response = handler(request)
            if "content-type" not in response.headers:
                response.headers["content-type"] = "application/json"
            return response
        http = httpx.Client(transport=httpx.MockTransport(typed_handler), follow_redirects=True)
        self.addCleanup(http.close)
        client = GatewayClient("tiller", "https://gateway.test/prefix/v1", "test-secret", http_client=http, **kwargs)
        self.addCleanup(client.close)
        return client, http

    def fixture(self, name):
        return (FIXTURES / name).read_bytes()

    def test_tiller_catalog_cache_and_filter(self):
        requests = []
        def handler(request):
            requests.append(request)
            return httpx.Response(200, content=self.fixture("tiller-models.json"))
        client, _ = self.client(handler)
        models = client.list_models()
        self.assertEqual(str(requests[0].url), "https://gateway.test/prefix/v1/models")
        self.assertEqual(models[0].reasoning_metadata.budget_min, 1024)
        self.assertIs(models[0].structured_output, False)
        self.assertIsNone(models[1].tools)
        self.assertFalse(models[1].reasoning_metadata.efforts_known)
        self.assertTrue(models[2].reasoning_metadata.efforts_known)
        self.assertIsNone(models[2].reasoning_metadata.efforts)
        self.assertEqual(models[3].reasoning_metadata.efforts, ())
        self.assertEqual(len(client.list_models(ModelFilter(tools=True, min_context=100))), 1)
        self.assertEqual(client.list_models(ModelFilter(structured_output=True)), ())
        models[0].raw.clear()
        self.assertEqual(client.list_models()[0].id, "virtual/free")
        self.assertIn("id", client.list_models()[0].raw)
        self.assertEqual(len(requests), 1)

    def test_refresh_failure_keeps_snapshot_and_timestamp(self):
        count = 0
        def handler(request):
            nonlocal count
            count += 1
            return httpx.Response(200, content=self.fixture("tiller-models.json")) if count == 1 else httpx.Response(503)
        client, _ = self.client(handler)
        before = client.refresh_models()
        with self.assertRaises(GatewayError):
            client.refresh_models()
        self.assertEqual(client.cached_catalog, before)
        self.assertEqual(len(client.list_models()), 4)

    def test_openrouter_user_catalog_and_explicit_flags(self):
        paths = []
        def handler(request):
            paths.append(request.url.path)
            data = json.loads(self.fixture("openrouter-models.json"))
            data["data"][0]["supports_tools"] = 0
            return httpx.Response(200, json=data)
        http = httpx.Client(transport=httpx.MockTransport(handler))
        self.addCleanup(http.close)
        with GatewayClient("openrouter", "https://gateway.test/api/v1", "key", http_client=http) as client:
            models = client.list_models()
        self.assertEqual(paths, ["/api/v1/models/user"])
        self.assertTrue(models[0].vision)
        self.assertFalse(models[0].tools)
        self.assertTrue(models[0].structured_output)
        self.assertFalse(models[1].vision)
        self.assertFalse(models[1].tools)
        self.assertIsNone(models[2].tools)
        self.assertEqual(models[0].max_output_tokens, 16000)

    def test_empty_catalog(self):
        client, _ = self.client(lambda _: httpx.Response(200, json={"data": []}))
        self.assertEqual(client.list_models(), ())

    def test_pagination_and_off_origin(self):
        requests = []
        def handler(request):
            requests.append(request)
            if len(requests) == 1:
                return httpx.Response(200, json={"data": [{"id": "one"}], "links": {"next": "?page=2"}})
            return httpx.Response(200, json={"data": [{"id": "two"}], "links": {"next": "https://evil.test/models"}})
        client, _ = self.client(handler)
        with self.assertRaises(GatewayError):
            client.list_models()
        self.assertEqual(len(requests), 2)
        self.assertIsNone(client.cached_catalog)
        self.assertEqual(requests[1].url.params["page"], "2")

    def test_redirects_never_followed_with_injected_client(self):
        requests = []
        def handler(request):
            requests.append(request)
            return httpx.Response(302, headers={"Location": "https://evil.test"})
        client, _ = self.client(handler)
        with self.assertRaises(GatewayError) as caught:
            client.list_models()
        self.assertEqual(caught.exception.status, 302)
        self.assertEqual(len(requests), 1)

    def test_chat_native_roundtrip_and_extensions(self):
        bodies = []
        def handler(request):
            bodies.append(json.loads(request.content))
            self.assertEqual(request.headers["authorization"], "Bearer test-secret")
            return httpx.Response(200, content=self.fixture("chat-result.json"), headers={"x-request-id": "req-1"})
        client, _ = self.client(handler)
        schema = {"type": "json_schema", "json_schema": {"name": "reply", "strict": True, "schema": {"type": "object"}}}
        result = client.chat("virtual/free", [{"role": "user", "content": "hello"}], response_format=schema,
                             tools=[], temperature=0, reasoning={"effort": "low"}, extra_body={"provider": {"sort": "price"}})
        self.assertEqual(result.tool_calls[0].arguments, '{"value":1}')
        self.assertEqual(result.usage.raw["prompt_tokens_details"]["cached_tokens"], 3)
        self.assertEqual(result.request_id, "req-1")
        self.assertTrue(result.raw["extra_fixture_field"])
        client.chat("virtual/free", [result.assistant_message, {"role": "tool", "tool_call_id": "call_1", "content": "1"}])
        self.assertEqual(bodies[1]["messages"][0], result.assistant_message)
        self.assertFalse(bodies[0]["stream"])
        self.assertEqual(bodies[0]["response_format"], schema)
        self.assertNotIn("Fixture", repr(result))
        self.assertNotIn("test-secret", repr(client))

    def test_extension_conflicts(self):
        client, _ = self.client(lambda _: self.fail("unexpected request"))
        for extra in ({"Model": "other"}, {"STREAM": True}, {"Tools": []}, {"Authorization": "key"}, {"api_key": "key"}):
            with self.subTest(extra=extra), self.assertRaises(GatewayError):
                client.chat("x", MESSAGES, extra_body=extra)
        with self.assertRaises(GatewayError):
            client.chat("x", MESSAGES, extra_body={"foo": 1}, foo=2)

    def test_unknown_usage(self):
        client, _ = self.client(lambda _: httpx.Response(200, json={"choices": [{"message": {"role": "assistant", "content": None}}]}))
        self.assertIsNone(client.chat("x", MESSAGES).usage)

    def test_safe_errors_and_no_retry(self):
        for status, code, category in ((401, "invalid_api_key", "authentication"), (402, None, "billing"),
                                       (400, "context_length_exceeded", "context_limit"),
                                       (400, "unsupported_feature", "unsupported_feature"),
                                       (429, None, "rate_limit"), (503, "server_error", "http")):
            calls = []
            def handler(request):
                calls.append(request)
                return httpx.Response(status, json={"error": {"code": code, "message": "test-secret PROMPT"}},
                                      headers={"x-request-id": "req-safe", "retry-after": "30"})
            client, _ = self.client(handler)
            with self.subTest(status=status), self.assertRaises(GatewayError) as caught:
                client.chat("x", MESSAGES)
            error = caught.exception
            self.assertEqual(error.category, category)
            self.assertEqual(error.retry_after, "30")
            self.assertEqual(error.request_id, "req-safe")
            self.assertNotIn("test-secret", repr(error))
            self.assertNotIn("PROMPT", str(error))
            self.assertIsNone(error.__cause__)
            self.assertEqual(len(calls), 1)

    def test_config_validation(self):
        for url in ("ftp://gateway.test", "https://user:pass@gateway.test", "https://gateway.test/#frag", "https://gateway.test/?key=secret"):
            with self.subTest(url=url), self.assertRaises(GatewayError):
                GatewayClient("tiller", url, "key")
        for key in ("", "a\nb", "a\x00b"):
            with self.assertRaises(GatewayError):
                GatewayClient("tiller", "https://gateway.test/v1", key)
        for timeout in (None, 0, float("inf"), httpx.Timeout(None)):
            with self.assertRaises(GatewayError):
                GatewayClient("tiller", "https://gateway.test/v1", "key", timeout=timeout)

    def test_request_exception_redaction(self):
        for exception, category in ((httpx.ConnectError("test-secret PROMPT"), "transport"), (httpx.ReadTimeout("test-secret PROMPT"), "timeout")):
            def handler(request):
                raise exception
            client, _ = self.client(handler)
            with self.assertRaises(GatewayError) as caught:
                client.chat("x", MESSAGES)
            self.assertEqual(caught.exception.category, category)
            self.assertNotIn("PROMPT", repr(caught.exception))
            self.assertTrue(caught.exception.__suppress_context__)

    def stream_client(self, data, limits=None, failure=None):
        upstream = Fragmented(data, failure)
        client, http = self.client(lambda _: httpx.Response(200, stream=upstream, headers={"content-type": "text/event-stream"}), limits=limits)
        return client, http, upstream

    def test_fixture_stream_unicode_fragments_finish_usage_done(self):
        client, _, upstream = self.stream_client(self.fixture("chat-stream.sse").replace(b"\n", b"\r\n"))
        with client.stream("virtual/free", MESSAGES) as stream:
            events = list(stream)
        self.assertTrue(upstream.closed)
        kinds = [event.kind for event in events]
        self.assertEqual(kinds.count("done"), 1)
        self.assertEqual(kinds.count("finish"), 2)
        self.assertLess(kinds.index("finish"), kinds.index("usage"))
        self.assertEqual(next(e.text for e in events if e.kind == "text"), "Hello 🌍")
        self.assertEqual("".join(e.arguments for e in events if e.kind == "tool"), '{"value":1}')
        self.assertEqual(next(e.usage.total_tokens for e in events if e.kind == "usage"), 20)
        self.assertTrue(all(e.raw_frame is not None for e in events))

    def test_multiline_comments_and_empty_frames(self):
        data = b': comment\r\ndata: {"choices":\r\ndata: [{"delta":{"role":"assistant"}}]}\r\n\r\ndata: [DONE]\r\n\r\n'
        client, _, _ = self.stream_client(data)
        with client.stream("x", MESSAGES) as stream:
            self.assertEqual([e.kind for e in stream], ["frame", "done"])

    def test_stream_failure_and_malformed(self):
        cases = [self.fixture("stream-error.sse") + b"\n", b'data: {"choices": [{"finish_reason":"error"}]}\n\n',
                 b'data: nope\n\n', b'data: {"choices":[]}\n\n', b'data: {"choices":"bad"}\n\n',
                 b'data: \xff\n\n', b'data: {"choices":[]}\n']
        for data in cases:
            client, _, upstream = self.stream_client(data)
            with self.subTest(data=data), self.assertRaises(GatewayError):
                with client.stream("x", MESSAGES) as stream:
                    list(stream)
            self.assertTrue(upstream.closed)

    def test_error_type_only_and_untrusted_metadata(self):
        client, _ = self.client(lambda _: httpx.Response(400, json={"error": {
            "type": "context_length_exceeded", "code": "PROMPT-secret", "message": "test-secret"}},
            headers={"x-request-id": "test-secret", "retry-after": "test-secret"}))
        with self.assertRaises(GatewayError) as caught:
            client.chat("x", MESSAGES)
        self.assertEqual(caught.exception.category, "context_limit")
        self.assertIsNone(caught.exception.code)
        self.assertIsNone(caught.exception.request_id)
        self.assertIsNone(caught.exception.retry_after)

    def test_client_close_closes_active_stream(self):
        client, http, upstream = self.stream_client(self.fixture("chat-stream.sse"))
        with client.stream("x", MESSAGES) as stream:
            next(stream)
            client.close()
            self.assertTrue(upstream.closed)
            with self.assertRaises(GatewayError):
                next(stream)
        self.assertFalse(http.is_closed)

    def test_empty_choices_usage_chunk(self):
        data = b'data: {"choices":[],"usage":{"total_tokens":4}}\n\ndata: [DONE]\n\n'
        client, _, upstream = self.stream_client(data)
        with client.stream("x", MESSAGES) as stream:
            events = list(stream)
        self.assertEqual([e.kind for e in events], ["usage", "done"])
        self.assertEqual(events[0].usage.total_tokens, 4)
        self.assertTrue(upstream.closed)

    def test_stream_limit_gates(self):
        for limits in (Limits(line_bytes=10), Limits(event_bytes=20), Limits(body_bytes=512), Limits(argument_bytes=2)):
            client, _, upstream = self.stream_client(self.fixture("chat-stream.sse"), limits)
            with self.subTest(limits=limits), self.assertRaises(GatewayError):
                with client.stream("x", MESSAGES) as stream:
                    list(stream)
            self.assertTrue(upstream.closed)

    def test_stream_cancellation_and_client_ownership(self):
        client, http, upstream = self.stream_client(self.fixture("chat-stream.sse"))
        with client.stream("x", MESSAGES) as stream:
            next(stream)
        self.assertTrue(upstream.closed)
        with self.assertRaises(GatewayError) as caught:
            next(stream)
        self.assertEqual(caught.exception.category, "cancelled")
        client.close()
        self.assertFalse(http.is_closed)
        owned = GatewayClient("tiller", "https://gateway.test/v1", "key")
        owned.close()
        self.assertTrue(owned._http.is_closed)

    def test_stream_read_timeout(self):
        client, _, upstream = self.stream_client(b': comment\n\n', failure=httpx.ReadTimeout("test-secret"))
        with self.assertRaises(GatewayError) as caught:
            with client.stream("x", MESSAGES) as stream:
                list(stream)
        self.assertEqual(caught.exception.category, "timeout")
        self.assertTrue(upstream.closed)

    def test_http_body_limits_close(self):
        upstream = Fragmented(b"x" * 100)
        client, _ = self.client(lambda _: httpx.Response(200, stream=upstream), limits=Limits(body_bytes=10))
        with self.assertRaises(GatewayError):
            client.list_models()
        self.assertTrue(upstream.closed)

    def test_real_local_http_transport(self):
        fixture = self.fixture("chat-result.json")
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_POST(self):
                self.rfile.read(int(self.headers["Content-Length"]))
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(fixture)))
                self.end_headers()
                self.wfile.write(fixture)
        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with GatewayClient("tiller", f"http://127.0.0.1:{server.server_port}/v1", "key") as client:
                self.assertEqual(client.chat("virtual/free", MESSAGES).usage.total_tokens, 20)
        finally:
            server.shutdown()
            server.server_close()
            thread.join()

    def test_cache_ttl_expiration_and_failed_refresh(self):
        calls = []
        def handler(request):
            calls.append(request)
            if len(calls) >= 3:
                return httpx.Response(503)
            return httpx.Response(200, content=self.fixture("tiller-models.json"))
        client, _ = self.client(handler)
        with patch("tiller_gateway.client.time.monotonic", return_value=100):
            client.list_models()
        before = client.cached_catalog
        with patch("tiller_gateway.client.time.monotonic", return_value=399):
            client.list_models()
        self.assertEqual(len(calls), 1)
        with patch("tiller_gateway.client.time.monotonic", return_value=400):
            client.list_models()
        self.assertEqual(len(calls), 2)
        before = client.cached_catalog
        with patch("tiller_gateway.client.time.monotonic", return_value=700):
            with self.assertRaises(GatewayError):
                client.list_models()
            self.assertEqual(client.cached_catalog, before)
            self.assertEqual(client._cached_at_monotonic, 400)
            with self.assertRaises(GatewayError):
                client.list_models(refresh=True)
        self.assertEqual(client.cached_catalog, before)

    def test_min_output(self):
        client, _ = self.client(lambda _: httpx.Response(200, content=self.fixture("tiller-models.json")))
        self.assertEqual(len(client.list_models(ModelFilter(min_output=8192))), 1)
        self.assertEqual(client.list_models(ModelFilter(min_output=8193)), ())

    def test_reasoning_aliases_options_and_explicit_null(self):
        entries = [
            {"id": "a", "reasoning_options": [{"type": "budget_tokens", "min": 10, "max": 20}, {"type": "enabled"}]},
            {"id": "b", "reasoning": {"min_budget_tokens": 30, "max_budget_tokens": 40, "budget_min": 1, "budget_max": 2}},
            {"id": "c", "reasoning": {"budget_min": 50, "budget_max": 60}},
            {"id": "d", "supports_tools": None, "supports_vision": "invalid", "supports_structured_output": None,
             "supported_parameters": ["tools", "structured_outputs"], "architecture": {"input_modalities": ["image"]}},
        ]
        http = httpx.Client(transport=httpx.MockTransport(lambda _: httpx.Response(200, json={"data": entries})))
        self.addCleanup(http.close)
        with GatewayClient("openrouter", "https://gateway.test/v1", "key", http_client=http) as client:
            models = client.list_models()
        self.assertTrue(models[0].reasoning_metadata.supports_max_tokens)
        self.assertTrue(models[0].reasoning_metadata.toggle)
        self.assertEqual(models[0].reasoning_metadata.budget_min, 10)
        self.assertEqual(models[1].reasoning_metadata.budget_min, 30)
        self.assertEqual(models[1].reasoning_metadata.budget_max, 40)
        self.assertEqual(models[2].reasoning_metadata.budget_min, 50)
        self.assertIsNone(models[3].tools)
        self.assertIsNone(models[3].vision)
        self.assertIsNone(models[3].structured_output)

    def test_response_format_not_schema_support(self):
        http = httpx.Client(transport=httpx.MockTransport(lambda _: httpx.Response(200, json={"data": [
            {"id": "a", "supported_parameters": ["response_format"]},
            {"id": "b", "supported_parameters": ["structured_outputs"]}]})))
        self.addCleanup(http.close)
        with GatewayClient("openrouter", "https://gateway.test/v1", "key", http_client=http) as client:
            models = client.list_models()
        self.assertFalse(models[0].structured_output)
        self.assertTrue(models[1].structured_output)

    def test_request_and_catalog_path_validation(self):
        client, _ = self.client(lambda _: self.fail("unexpected HTTP"))
        for model, messages in (("", MESSAGES), ("  ", MESSAGES), (None, MESSAGES), ("x", []), ("x", None)):
            for operation in (client.chat, client.stream):
                with self.assertRaises(GatewayError):
                    operation(model, messages)
        for path in ("models?key=x", "models\n", "%2e%2e/models", "models/%2E", "%252e%252e/models", "models%3fkey=x", "models%00", "models/../user"):
            with self.subTest(path=path), self.assertRaises(GatewayError):
                GatewayClient("tiller", "https://gateway.test/v1", "key", catalog_path=path)

    def test_timeout_status_and_request_id_fallbacks(self):
        for status, category in ((408, "timeout"), (504, "timeout"), (422, "invalid_request")):
            client, _ = self.client(lambda _: httpx.Response(status, headers={"x-request-id": "test-secret", "x-generation-id": "gen-1"}))
            with self.assertRaises(GatewayError) as caught:
                client.chat("x", MESSAGES)
            self.assertEqual(caught.exception.category, category)
            self.assertEqual(caught.exception.request_id, "gen-1")
        for header in ("x-request-id", "request-id", "x-generation-id", "x-tiller-request-id"):
            client, _ = self.client(lambda _: httpx.Response(200, content=self.fixture("chat-result.json"), headers={header: "id-1"}))
            self.assertEqual(client.chat("x", MESSAGES).request_id, "id-1")

    def test_cache_usage_counters(self):
        data = json.loads(self.fixture("chat-result.json"))
        data["usage"].update(cache_read_input_tokens=2, cache_creation_input_tokens=5)
        client, _ = self.client(lambda _: httpx.Response(200, json=data))
        usage = client.chat("x", MESSAGES).usage
        self.assertEqual((usage.cached_tokens, usage.cache_read_input_tokens, usage.cache_creation_input_tokens), (3, 2, 5))

    def test_sse_content_type_null_error_opaque_reasoning(self):
        native = {"error": None, "choices": [{"delta": {"reasoning_details": [{"type": "encrypted", "data": "opaque"}]}}]}
        data = ("data: " + json.dumps(native) + "\n\ndata: [DONE]\n\n").encode()
        for content_type in ("text/event-stream; charset=utf-8", "TEXT/EVENT-STREAM", "application/text/event-stream", "text/event-stream-bad"):
            upstream = Fragmented(data)
            client, _ = self.client(lambda _: httpx.Response(200, stream=upstream, headers={"content-type": content_type}))
            if content_type in ("text/event-stream; charset=utf-8", "TEXT/EVENT-STREAM"):
                with client.stream("x", MESSAGES) as stream:
                    events = list(stream)
                self.assertEqual(events[0].kind, "reasoning")
                self.assertEqual(events[0].reasoning_details, native["choices"][0]["delta"]["reasoning_details"])
                self.assertEqual(events[0].raw, native)
            else:
                with self.assertRaises(GatewayError):
                    with client.stream("x", MESSAGES):
                        pass
            self.assertTrue(upstream.closed)

    def test_tool_example_bounded_auto_native_replay(self):
        bodies = []
        data = json.loads(self.fixture("chat-result.json"))
        def handler(request):
            bodies.append(json.loads(request.content))
            reply = json.loads(json.dumps(data))
            if len(bodies) == 2:
                reply["choices"][0]["message"].pop("tool_calls")
            return httpx.Response(200, json=reply)
        client, _ = self.client(handler)
        tool = {"type": "function", "function": {"name": "lookup"}}
        result = tool_roundtrip(client, "x", MESSAGES, tool, lambda name, args: {"result": 1})
        self.assertFalse(result.tool_calls)
        self.assertTrue(all(body["tool_choice"] == "auto" and body["max_tokens"] == 256 for body in bodies))
        self.assertEqual(bodies[1]["messages"][-2], data["choices"][0]["message"])
        self.assertEqual(bodies[1]["messages"][-1]["tool_call_id"], "call_1")
        client, _ = self.client(lambda _: httpx.Response(200, json=data))
        with self.assertRaises(ValueError):
            tool_roundtrip(client, "x", MESSAGES, tool, lambda name, args: {}, max_turns=2)
        with self.assertRaises(ValueError):
            tool_roundtrip(client, "x", MESSAGES, tool, lambda name, args: "x" * 100, max_result_bytes=10)


    def test_json_object_explicit_modalities_and_bad_efforts(self):
        entries = [
            {"id": "json", "supported_parameters": ["response_format"]},
            {"id": "schema", "supported_parameters": ["structured_outputs"]},
            {"id": "null", "supports_json_object": None, "supports_tools": "bad", "supported_parameters": ["response_format", "tools"],
             "input_modalities": None, "output_modalities": [], "architecture": {"input_modalities": ["image"], "output_modalities": ["text"]}},
            {"id": "bad", "reasoning": {"supported_efforts": "invalid"}, "reasoning_options": [{"type": "effort", "values": ["high"]}]},
        ]
        http = httpx.Client(transport=httpx.MockTransport(lambda _: httpx.Response(200, json={"data": entries})))
        self.addCleanup(http.close)
        with GatewayClient("openrouter", "https://gateway.test/v1", "key", http_client=http) as client:
            models = client.list_models()
            self.assertEqual([m.id for m in client.list_models(ModelFilter(json_object=True))], ["json"])
        self.assertFalse(models[0].structured_output)
        self.assertIsNone(models[2].json_object)
        self.assertIsNone(models[2].tools)
        self.assertIsNone(models[2].input_modalities)
        self.assertEqual(models[2].output_modalities, ())
        self.assertIsNone(models[2].vision)
        self.assertFalse(models[3].reasoning_metadata.efforts_known)

    def test_nonstream_content_type_and_null_error(self):
        data = json.loads(self.fixture("chat-result.json"))
        data["error"] = None
        for content_type in ("application/json", "application/json; charset=utf-8", "application/problem+json", "text/html", "application/json-bad", ""):
            client, _ = self.client(lambda _: httpx.Response(200, json=data, headers={"content-type": content_type}))
            if content_type.startswith("application/json;") or content_type in ("application/json", "application/problem+json"):
                self.assertEqual(client.chat("x", MESSAGES).id, "fixture-response")
            else:
                with self.assertRaises(GatewayError) as caught:
                    client.chat("x", MESSAGES)
                self.assertEqual(caught.exception.category, "malformed_response")

    def test_malformed_completed_tools_and_messages(self):
        mutations = [lambda m: m.update(role="user"), lambda m: m.update(content=123),
            lambda m: m.update(content=[{"type": "text", "text": 123}]),
            lambda m: m.update(tool_calls={"bad": "test-secret"}),
            lambda m: m["tool_calls"][0].update(id=123), lambda m: m["tool_calls"][0].update(type="bad"),
            lambda m: m["tool_calls"][0]["function"].update(name=123),
            lambda m: m["tool_calls"][0]["function"].update(arguments="test-secret"),
            lambda m: m["tool_calls"][0]["function"].update(arguments="[]"),
            lambda m: m["tool_calls"][0]["function"].update(arguments='{"x":NaN}')]
        for mutate in mutations:
            data = json.loads(self.fixture("chat-result.json"))
            mutate(data["choices"][0]["message"])
            client, _ = self.client(lambda _: httpx.Response(200, json=data))
            with self.assertRaises(GatewayError) as caught:
                client.chat("x", MESSAGES)
            self.assertEqual(caught.exception.category, "malformed_response")
            self.assertNotIn("test-secret", str(caught.exception))
            self.assertTrue(caught.exception.__suppress_context__)

    def test_native_image_schema_tool_result_and_extensions(self):
        bodies = []
        def handler(request):
            bodies.append(json.loads(request.content))
            return httpx.Response(200, content=self.fixture("chat-result.json"))
        client, _ = self.client(handler)
        messages = [{"role": "user", "content": [{"type": "text", "text": "你好 🌍"},
                    {"type": "image_url", "image_url": {"url": "data:image/png;base64,AA==", "detail": "low"}}]},
                    {"role": "assistant", "content": None, "tool_calls": [{"id": "a", "type": "function", "function": {"name": "lookup", "arguments": "{}"}}], "reasoning_details": [{"data": "opaque"}]},
                    {"role": "tool", "tool_call_id": "a", "content": '{"answer":1}'}]
        tools = [{"type": "function", "function": {"name": "lookup", "strict": True, "parameters": {"type": "object", "additionalProperties": False}}}]
        schema = {"type": "json_schema", "json_schema": {"name": "x", "strict": True, "schema": {"type": "object"}}}
        client.chat("x", messages, tools=tools, response_format=schema, tool_choice="auto", extra_body={"provider": {"order": ["a"]}})
        self.assertEqual(bodies[0]["messages"], messages)
        self.assertEqual(bodies[0]["tools"], tools)
        self.assertEqual(bodies[0]["response_format"], schema)
        self.assertEqual(bodies[0]["provider"], {"order": ["a"]})
        for extra in ({"MODEL": "unsafe"}, {"Response_Format": {}}, {"Headers": {"key": "test-secret"}}, {1: "unsafe"}):
            with self.assertRaises(GatewayError):
                client.chat("x", MESSAGES, extra_body=extra)

    def test_zero_cache_ttl_and_usage_aliases(self):
        calls = []
        def handler(request):
            calls.append(request)
            return httpx.Response(200, json={"data": []})
        client, _ = self.client(handler, cache_ttl=0)
        client.list_models()
        client.list_models()
        self.assertEqual(len(calls), 2)
        data = json.loads(self.fixture("chat-result.json"))
        data["usage"] = {"input_tokens": 10, "output_tokens": 3, "cache_read_input_tokens": 5, "cache_creation_input_tokens": 2}
        client, _ = self.client(lambda _: httpx.Response(200, json=data))
        usage = client.chat("x", MESSAGES).usage
        self.assertEqual((usage.prompt_tokens, usage.completion_tokens, usage.cached_tokens), (10, 3, 5))
        self.assertEqual(usage.cache_creation_input_tokens, 2)
        self.assertIsNone(usage.total_tokens)


if __name__ == "__main__":
    unittest.main()
