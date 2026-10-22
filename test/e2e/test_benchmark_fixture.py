"""Behavior tests for the benchmark OpenAI-compatible fixture."""
from __future__ import annotations

import json
import socket
import threading
import time
import unittest
import urllib.error
import urllib.request

from benchmark_fixture import BenchmarkEndpoint

STREAM_BODY = {"model": "fixture-model",
               "messages": [{"role": "user", "content": "hello benchmark"}],
               "max_tokens": 8, "stream": True,
               "stream_options": {"include_usage": True}}


def request(endpoint: BenchmarkEndpoint, method: str, path: str,
            body: dict | None = None, api_key: str | None = "fixture-key"):
    headers = {"Content-Type": "application/json"}
    if api_key is not None:
        headers["Authorization"] = f"Bearer {api_key}"
    raw = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(endpoint.base + path, data=raw, method=method, headers=headers)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        response = opener.open(req, timeout=10)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        return response.status, response.headers.get("Content-Type", ""), response.read()


class BenchmarkFixtureTests(unittest.TestCase):
    def setUp(self):
        self.endpoint = BenchmarkEndpoint().start()

    def tearDown(self):
        self.endpoint.stop()

    def test_models_requires_key_and_advertises_the_fixture_model(self):
        status, _, raw = request(self.endpoint, "GET", "/v1/models", api_key=None)
        self.assertEqual(status, 401)
        self.assertEqual(json.loads(raw)["error"]["type"], "fixture_authentication_error")

        status, content_type, raw = request(self.endpoint, "GET", "/v1/models")
        self.assertEqual(status, 200)
        self.assertTrue(content_type.startswith("application/json"))
        self.assertEqual(json.loads(raw)["data"], [
            {"id": "fixture-model", "object": "model", "created": 0, "owned_by": "fixture"}])
        self.assertEqual(self.endpoint.models_requests[-1]["authorization"], "Bearer fixture-key")

    def test_streamed_completion_emits_usage_and_done(self):
        body = {"model": "fixture-model",
                "messages": [{"role": "user", "content": "hello benchmark"}],
                "max_tokens": 8, "stream": True,
                "stream_options": {"include_usage": True}}
        status, content_type, raw = request(self.endpoint, "POST", "/v1/chat/completions", body)
        self.assertEqual(status, 200, raw)
        self.assertTrue(content_type.startswith("text/event-stream"))
        events = [json.loads(line.removeprefix("data: "))
                  for line in raw.decode().splitlines()
                  if line.startswith("data: ") and line != "data: [DONE]"]
        self.assertEqual([event["choices"][0]["delta"]["content"]
                          for event in events if event["choices"]], ["fixture ", "answer"])
        usage = events[-1]["usage"]
        self.assertEqual(usage["completion_tokens"], 2)
        self.assertGreater(usage["prompt_tokens"], 0)
        self.assertEqual(usage["total_tokens"], usage["prompt_tokens"] + 2)
        self.assertEqual(raw.decode().rstrip().endswith("data: [DONE]"), True)
        recorded = self.endpoint.requests[0]
        self.assertEqual(recorded["body"], body)
        self.assertEqual(recorded["validation"], [])
        self.assertEqual(recorded["usage"], usage)

    def test_invalid_requests_are_rejected_and_recorded(self):
        body = {"model": "other-model",
                "messages": [],
                "max_tokens": 8, "stream": False}
        status, _, raw = request(self.endpoint, "POST", "/v1/chat/completions", body)
        self.assertEqual(status, 400)
        self.assertIn("model", json.loads(raw)["error"]["message"])
        self.assertIn("messages", json.loads(raw)["error"]["message"])
        self.assertIn("stream", json.loads(raw)["error"]["message"])
        self.assertIn("include_usage", json.loads(raw)["error"]["message"])
        self.assertEqual(len(self.endpoint.requests[0]["validation"]), 4)

    def test_unknown_path_is_not_found(self):
        status, _, raw = request(self.endpoint, "GET", "/v1/unknown")
        self.assertEqual(status, 404)
        self.assertEqual(json.loads(raw)["error"]["type"], "fixture_not_found")

    def test_held_stream_completes_when_the_client_stays(self):
        self.endpoint.hold_next_request()
        result = {}

        def read():
            result["value"] = request(self.endpoint, "POST", "/v1/chat/completions",
                                      STREAM_BODY)

        reader = threading.Thread(target=read, daemon=True)
        reader.start()
        try:
            self.assertTrue(self.endpoint.hold_reached.wait(5),
                            "the fixture did not hold")
        finally:
            self.endpoint.release_held()
        reader.join(timeout=10)
        self.assertFalse(reader.is_alive(), "the release did not finish the stream")
        status, _, raw = result["value"]
        self.assertEqual(status, 200)
        self.assertIn(b"data: [DONE]", raw)
        events = [json.loads(line.removeprefix("data: "))
                  for line in raw.decode().splitlines()
                  if line.startswith("data: ") and line != "data: [DONE]"]
        chunks = [event["choices"][0]["delta"]["content"]
                  for event in events if event["choices"]]
        usage = events[-1]["usage"]
        self.assertEqual(chunks, ["fixture ", "answer"])
        self.assertEqual(usage["completion_tokens"], len(chunks))
        self.assertEqual(self.endpoint.requests[0]["usage"], usage)
        snapshot = self.endpoint.snapshot()
        self.assertEqual(snapshot["completed"], 1)
        self.assertEqual(snapshot["aborted"], [])

    def test_a_canceled_client_is_recorded_as_an_abort(self):
        self.endpoint.hold_next_request()
        port = int(self.endpoint.base.rsplit(":", 1)[1])
        raw = json.dumps(STREAM_BODY).encode()
        connection = socket.create_connection(("127.0.0.1", port), timeout=5)
        try:
            connection.sendall(
                (f"POST /v1/chat/completions HTTP/1.1\r\nHost: fixture\r\n"
                 f"Authorization: Bearer fixture-key\r\n"
                 f"Content-Type: application/json\r\nContent-Length: {len(raw)}\r\n"
                 f"Connection: close\r\n\r\n").encode() + raw)
            received = b""
            while b"fixture " not in received:
                chunk = connection.recv(4096)
                if not chunk:
                    self.fail(f"the connection closed early: {received!r}")
                received += chunk
            self.assertTrue(self.endpoint.hold_reached.wait(5),
                            "the fixture did not hold the first chunk")
        finally:
            connection.close()
            self.endpoint.release_held()
        deadline = time.time() + 5
        while time.time() < deadline and not self.endpoint.snapshot()["aborted"]:
            time.sleep(0.05)
        snapshot = self.endpoint.snapshot()
        self.assertEqual(len(snapshot["aborted"]), 1, snapshot)
        self.assertIn(snapshot["aborted"][0]["error"],
                      {"BrokenPipeError", "ConnectionResetError"})
        self.assertEqual(snapshot["completed"], 0)

    def test_strict_json_types_are_rejected(self):
        for patch in ({"max_tokens": True},
                      {"stream_options": {"include_usage": "false"}},
                      {"stream_options": 1}, {"messages": ["not a message"]}):
            with self.subTest(patch=patch):
                status, _, _ = request(self.endpoint, "POST", "/v1/chat/completions",
                                       {**STREAM_BODY, **patch})
                self.assertEqual(status, 400)

    def test_output_respects_the_requested_token_limit(self):
        status, _, raw = request(self.endpoint, "POST", "/v1/chat/completions",
                                 {**STREAM_BODY, "max_tokens": 1})
        self.assertEqual(status, 200)
        events = [json.loads(line[6:]) for line in raw.decode().splitlines()
                  if line.startswith("data: ") and line != "data: [DONE]"]
        self.assertEqual(events[-1]["usage"]["completion_tokens"], 1)

    def test_stop_releases_held_handlers(self):
        self.endpoint.hold_next_request()
        failures = []
        def read():
            try:
                request(self.endpoint, "POST", "/v1/chat/completions", STREAM_BODY)
            except (OSError, ValueError) as error:
                failures.append(type(error).__name__)
        reader = threading.Thread(target=read, daemon=True)
        reader.start()
        try:
            self.assertTrue(self.endpoint.hold_reached.wait(5))
            self.assertTrue(self.endpoint.stop())
            reader.join(timeout=5)
            self.assertFalse(reader.is_alive())
            self.assertEqual(self.endpoint.active_handler_count, 0)
        finally:
            self.endpoint.release_held()
            reader.join(timeout=5)

    def test_max_tokens_caps_content_and_usage(self):
        body = {**STREAM_BODY, "max_tokens": 1}
        status, _, raw = request(self.endpoint, "POST", "/v1/chat/completions", body)
        self.assertEqual(status, 200, raw)
        events = [json.loads(line.removeprefix("data: "))
                  for line in raw.decode().splitlines()
                  if line.startswith("data: ") and line != "data: [DONE]"]
        chunks = [event["choices"][0]["delta"]["content"]
                  for event in events if event["choices"]]
        usage = events[-1]["usage"]
        self.assertEqual(chunks, ["fixture "])
        self.assertEqual(usage["completion_tokens"], 1)
        self.assertEqual(usage["total_tokens"], usage["prompt_tokens"] + 1)
        self.assertEqual(self.endpoint.requests[0]["usage"], usage)


    def test_max_tokens_and_include_usage_types_are_strict(self):
        for key, value in (("max_tokens", "false"), ("max_tokens", True),
                           ("include_usage", "true"), ("include_usage", 1)):
            with self.subTest(key=key, value=value):
                body = {**STREAM_BODY,
                        "stream_options": {"include_usage": key != "include_usage"}}
                if key == "max_tokens":
                    body["max_tokens"] = value
                else:
                    body["stream_options"]["include_usage"] = value
                status, _, raw = request(self.endpoint, "POST",
                                         "/v1/chat/completions", body)
                self.assertEqual(status, 400, (key, value, raw))
                self.assertIn(key, json.loads(raw)["error"]["message"])


    def test_stop_releases_a_held_handler(self):
        self.endpoint.hold_next_request()
        port = int(self.endpoint.base.rsplit(":", 1)[1])
        raw = json.dumps(STREAM_BODY).encode()
        connection = socket.create_connection(("127.0.0.1", port), timeout=5)
        try:
            connection.sendall(
                (f"POST /v1/chat/completions HTTP/1.1\r\nHost: fixture\r\n"
                 f"Authorization: Bearer fixture-key\r\n"
                 f"Content-Type: application/json\r\nContent-Length: {len(raw)}\r\n"
                 f"Connection: close\r\n\r\n").encode() + raw)
            received = b""
            while b"fixture " not in received:
                chunk = connection.recv(4096)
                if not chunk:
                    self.fail(f"the connection closed early: {received!r}")
                received += chunk
            self.assertTrue(self.endpoint.hold_reached.wait(5),
                            "the fixture did not hold the first chunk")
            self.assertEqual(self.endpoint.snapshot()["active_handlers"], 1)
            # The client stays connected and never releases the barrier; stop
            # must wake the handler, close its socket and wait for it to exit.
            self.assertTrue(self.endpoint.stop(), "stop did not reclaim the held handler")
            snapshot = self.endpoint.snapshot()
            self.assertEqual(snapshot["active_handlers"], 0)
            self.assertTrue(self.endpoint.release.is_set())
        finally:
            connection.close()


    def test_stop_releases_the_listener(self):
        base = self.endpoint.base
        port = int(base.rsplit(":", 1)[1])
        self.endpoint.stop()
        with socket.socket() as probe:
            self.assertNotEqual(probe.connect_ex(("127.0.0.1", port)), 0)


if __name__ == "__main__":
    unittest.main()
