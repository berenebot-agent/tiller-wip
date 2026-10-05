import json
import unittest

import httpx

from tiller_gateway import GatewayClient, GatewayError


class ReviewTests(unittest.TestCase):
    def client(self, response):
        http = httpx.Client(transport=httpx.MockTransport(lambda request: response))
        self.addCleanup(http.close)
        client = GatewayClient("tiller", "https://gateway.test/v1", "test-secret", http_client=http)
        self.addCleanup(client.close)
        return client

    def test_repr_does_not_show_echoed_credentials(self):
        payload = {"id": "test-secret", "model": "test-secret", "choices": [{
            "message": {"role": "assistant", "content": "private", "tool_calls": [{
                "id": "test-secret", "type": "function", "function": {"name": "test-secret", "arguments": "{}"}}]},
            "finish_reason": "test-secret"}]}
        result = self.client(httpx.Response(200, json=payload)).chat("x", [{"role": "user", "content": "x"}])
        self.assertNotIn("test-secret", repr(result))
        self.assertNotIn("test-secret", repr(result.tool_calls[0]))

    def test_surrogate_argument_error_is_sanitized(self):
        payload = {"choices": [{"message": {"role": "assistant", "tool_calls": [{
            "id": "a", "type": "function", "function": {"name": "f", "arguments": '{"value":"\ud800"}'}}]}}]}
        client = self.client(httpx.Response(200, content=json.dumps(payload), headers={"content-type": "application/json"}))
        with self.assertRaises(GatewayError) as caught:
            client.chat("x", [{"role": "user", "content": "x"}])
        self.assertEqual(caught.exception.category, "malformed_response")
        self.assertTrue(caught.exception.__suppress_context__)

    def test_stream_identifiers_and_malformed_choice(self):
        for choice in (None, {}, {"delta": None}):
            body = ('data: ' + json.dumps({"choices": [choice]}) + '\n\ndata: [DONE]\n\n').encode()
            client = self.client(httpx.Response(200, content=body, headers={"content-type": "text/event-stream", "x-generation-id": "gen-safe"}))
            with self.assertRaises(GatewayError) as caught:
                with client.stream("x", [{"role": "user", "content": "x"}]) as stream:
                    list(stream)
            self.assertEqual(caught.exception.request_id, "gen-safe")
        body = b'data: {"choices":[{"delta":{"content":"test-secret"}}]}\n\ndata: [DONE]\n\n'
        client = self.client(httpx.Response(200, content=body, headers={"content-type": "text/event-stream", "x-generation-id": "gen-safe"}))
        with client.stream("x", [{"role": "user", "content": "x"}]) as stream:
            events = list(stream)
        self.assertEqual(events[0].request_id, "gen-safe")
        self.assertNotIn("test-secret", repr(events[0]))


if __name__ == "__main__":
    unittest.main()
