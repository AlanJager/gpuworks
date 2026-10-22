"""Behavior tests for the assistant OpenAI-compatible fixture."""
from __future__ import annotations

import json
import socket
import threading
import unittest
import urllib.error
import urllib.request

from assistant_fixture import DENIED_CALL_ID, DENIED_PATH, TOOL_CALL_ID, AssistantEndpoint, cluster_answer


def request(endpoint: AssistantEndpoint, body: dict, api_key: str = "fixture-key"):
    raw = json.dumps(body).encode()
    req = urllib.request.Request(endpoint.base + "/chat/completions", data=raw, method="POST",
                                 headers={"Content-Type": "application/json",
                                          "Authorization": f"Bearer {api_key}"})
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        response = opener.open(req, timeout=10)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        return response.status, response.read()


def body(messages, tools=True):
    return {"model": "fixture-assistant", "stream": True, "messages": messages,
            "tools": ([{"type": "function", "function": {"name": "cluster_overview"}}]
                      if tools else [])}


class AssistantFixtureTests(unittest.TestCase):
    def setUp(self):
        self.endpoint = AssistantEndpoint(slow_seconds=0.1).start()

    def tearDown(self):
        self.endpoint.stop()

    def test_first_request_streams_a_tool_call(self):
        status, raw = request(self.endpoint, body([{"role": "user", "content": "how many nodes"}]))
        self.assertEqual(status, 200, raw)
        self.assertIn(b'"name": "cluster_overview"', raw)
        self.assertIn(b'"finish_reason": "tool_calls"', raw)
        self.assertTrue(raw.rstrip().endswith(b"data: [DONE]"))
        recorded = self.endpoint.requests[0]
        self.assertEqual(recorded["kind"], "tool")
        self.assertEqual(recorded["authorization"], "Bearer fixture-key")
        self.assertEqual(recorded["body"]["model"], "fixture-assistant")

    def test_second_request_with_tool_result_streams_the_answer(self):
        content = json.dumps({
            "summary": {"nodes": {"total": 2}},
            "nodes_needing_attention": {"items": [{"name": "node-a"},
                                                  {"name": "node-b"}]},
        })
        messages = [
            {"role": "user", "content": "how many nodes"},
            {"role": "assistant", "content": "", "tool_calls": [
                {"id": TOOL_CALL_ID, "type": "function",
                 "function": {"name": "cluster_overview", "arguments": "{}"}}]},
            {"role": "tool", "tool_call_id": TOOL_CALL_ID, "name": "cluster_overview",
             "content": content},
        ]
        status, raw = request(self.endpoint, body(messages))
        self.assertEqual(status, 200, raw)
        self.assertIn(b"The cluster overview reports 2 node(s); attention includes node-a.", raw)
        self.assertIn(b'"finish_reason": "stop"', raw)
        recorded = self.endpoint.requests[0]
        self.assertEqual(recorded["kind"], "final")
        self.assertEqual(len(recorded["tool_messages"]), 1)
        self.assertEqual(recorded["tool_messages"][0]["name"], "cluster_overview")
        self.assertEqual(recorded["tool_messages"][0]["tool_call_id"], TOOL_CALL_ID)

    def test_tool_error_is_not_reported_as_success(self):
        messages = [
            {"role": "user", "content": "how many nodes"},
            {"role": "assistant", "content": "", "tool_calls": [
                {"id": TOOL_CALL_ID, "type": "function",
                 "function": {"name": "cluster_overview", "arguments": "{}"}}]},
            {"role": "tool", "tool_call_id": TOOL_CALL_ID, "name": "cluster_overview",
             "content": "error: /api/v1/ssh-key is not readable by the assistant"},
        ]
        status, raw = request(self.endpoint, body(messages))
        self.assertEqual(status, 200, raw)
        self.assertIn(b"could not be read", raw)
        self.assertNotIn(b"read successfully", raw)

    def test_mismatched_call_id_is_rejected(self):
        messages = [
            {"role": "user", "content": "how many nodes"},
            {"role": "assistant", "content": "", "tool_calls": [
                {"id": TOOL_CALL_ID, "type": "function",
                 "function": {"name": "cluster_overview", "arguments": "{}"}}]},
            {"role": "tool", "tool_call_id": "call_somebody_else",
             "name": "cluster_overview",
             "content": '{"summary":{"nodes":{"total":1}}}'},
        ]
        status, raw = request(self.endpoint, body(messages))
        self.assertEqual(status, 400, raw)
        self.assertIn(b"tool_call_id", raw)
        self.assertNotIn(b"read successfully", raw)

    def test_missing_preceding_assistant_call_is_rejected(self):
        messages = [
            {"role": "user", "content": "how many nodes"},
            {"role": "tool", "tool_call_id": TOOL_CALL_ID,
             "name": "cluster_overview",
             "content": '{"summary":{"nodes":{"total":2}}}'},
        ]
        status, raw = request(self.endpoint, body(messages))
        self.assertEqual(status, 400, raw)
        self.assertIn(b"tool_calls", raw)
        self.assertNotIn(b"reports 2 node", raw)

    def test_out_of_order_tool_result_is_rejected(self):
        messages = [
            {"role": "user", "content": "how many nodes"},
            {"role": "tool", "tool_call_id": TOOL_CALL_ID,
             "name": "cluster_overview",
             "content": '{"summary":{"nodes":{"total":2}}}'},
            {"role": "assistant", "content": "", "tool_calls": [
                {"id": TOOL_CALL_ID, "type": "function",
                 "function": {"name": "cluster_overview", "arguments": "{}"}}]},
        ]
        status, raw = request(self.endpoint, body(messages))
        self.assertEqual(status, 400, raw)
        self.assertIn(b"tool_calls", raw)

    # Mainline: keep result values plausible while corrupting the original
    # call or leaving an unanswered call -> reject the protocol, not just IDs.
    def test_incomplete_and_altered_tool_rounds_are_rejected(self):
        valid = [
            {"role": "user", "content": "how many nodes"},
            {"role": "assistant", "content": "", "tool_calls": [
                {"id": TOOL_CALL_ID, "type": "function", "function": {
                    "name": "cluster_overview", "arguments": "{}"}}]},
            {"role": "tool", "tool_call_id": TOOL_CALL_ID, "name": "cluster_overview",
             "content": '{"summary":{"nodes":{"total":1}},"nodes_needing_attention":{"items":[{"name":"owned"}]}}'},
        ]
        variants = {"missing-result": valid[:-1],
                    "trailing-user": valid + [{"role": "user", "content": "new question"}],
                    "orphan-call": [valid[0], valid[1], *valid[1:]]}
        for title, field, value in (("altered-args", "arguments", '{"unexpected":true}'),
                                     ("altered-name", "name", "another_tool")):
            messages = json.loads(json.dumps(valid))
            messages[1]["tool_calls"][0]["function"][field] = value
            variants[title] = messages
        for title, messages in variants.items():
            with self.subTest(title=title):
                status, raw = request(self.endpoint, body(messages))
                self.assertEqual(status, 400, raw)

    def test_invalid_fact_types_and_partial_failure_are_not_success(self):
        for total, extra in ((True, {}), (1, {"gpus": "unavailable (HTTP 500)"})):
            content = json.dumps({"summary": {"nodes": {"total": total}},
                                  "nodes_needing_attention": {"items": [{"name": "owned"}]},
                                  **extra})
            self.assertIn("could not be read", cluster_answer(content))

    def test_denied_request_asks_for_the_secret_route(self):
        status, raw = request(self.endpoint,
                              body([{"role": "user", "content": "denied please"}]))
        self.assertEqual(status, 200, raw)
        self.assertIn(b'"name": "call_api"', raw)
        self.assertIn(DENIED_PATH.encode(), raw)
        self.assertEqual(self.endpoint.requests[0]["kind"], "denied")

    def test_denied_tool_error_is_reported_in_the_final_answer(self):
        messages = [
            {"role": "user", "content": "denied please"},
            {"role": "assistant", "content": "", "tool_calls": [
                {"id": DENIED_CALL_ID, "type": "function",
                 "function": {"name": "call_api",
                              "arguments": json.dumps({"path": DENIED_PATH})}}]},
            {"role": "tool", "tool_call_id": DENIED_CALL_ID, "name": "call_api",
             "content": "error: /api/v1/ssh-key is not readable by the assistant"},
        ]
        status, raw = request(self.endpoint, body(messages))
        self.assertEqual(status, 200, raw)
        self.assertIn(b"The denied route was rejected", raw)
        self.assertNotIn(b"unexpectedly readable", raw)

    def test_slow_request_is_classified_for_the_stop_path(self):
        self.endpoint.slow_release.set()
        status, raw = request(self.endpoint, body([{"role": "user", "content": "slow please"}]))
        self.assertEqual(status, 200, raw)
        self.assertIn(b"late", raw)
        self.assertEqual(self.endpoint.requests[0]["kind"], "slow")

    def test_invalid_requests_are_rejected_and_recorded(self):
        status, raw = request(self.endpoint,
                              body([{"role": "user", "content": "hi"}], tools=False))
        self.assertEqual(status, 400)
        self.assertIn(b"tools", raw)
        self.assertEqual(self.endpoint.requests[0]["kind"], "tool")
        status, _ = request(self.endpoint, body([{"role": "user", "content": "hi"}]),
                            api_key="wrong")
        self.assertEqual(status, 401)

    def test_stop_releases_the_listener(self):
        port = int(self.endpoint.base.rsplit(":", 1)[1])
        self.endpoint.stop()
        with socket.socket() as probe:
            self.assertNotEqual(probe.connect_ex(("127.0.0.1", port)), 0)

    # Mainline: shutdown while an upstream stream is held -> release and join
    # the handler before returning; the client has no lingering response.
    def test_stop_releases_a_held_handler(self):
        result = {}
        reader = threading.Thread(target=lambda: result.setdefault("reply", request(
            self.endpoint, body([{"role": "user", "content": "slow please"}]))))
        reader.start()
        self.assertTrue(self.endpoint.wait_slow_started(2))
        try:
            self.endpoint.stop()
            self.assertTrue(self.endpoint.slow_release.is_set())
        finally:
            self.endpoint.release_slow()
            reader.join(timeout=3)
        self.assertFalse(reader.is_alive())

    def test_slow_stream_reports_a_real_client_abort(self):
        address = ("127.0.0.1", int(self.endpoint.base.rsplit(":", 1)[1]))
        connection = socket.create_connection(address, timeout=3)
        raw = json.dumps(body([{"role": "user", "content": "slow please"}])).encode()
        try:
            connection.sendall(("POST /chat/completions HTTP/1.1\r\nHost: fixture\r\n"
                                "Authorization: Bearer fixture-key\r\n"
                                f"Content-Length: {len(raw)}\r\n\r\n").encode() + raw)
            self.assertTrue(self.endpoint.wait_slow_started(2))
        finally:
            connection.close()
            self.endpoint.release_slow()
        self.assertTrue(self.endpoint.wait_aborted(3))
        self.assertEqual(self.endpoint.snapshot()["aborted"], 1)


if __name__ == "__main__":
    unittest.main()
