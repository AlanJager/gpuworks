"""Behavior tests for the OTLP/HTTP collector fixture.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: PYTHONPATH=test/e2e python3 -m unittest test_otlp_fixture -v
"""
from __future__ import annotations

import gzip
import json
import socket
import unittest
import urllib.error
import urllib.request

from otlp_fixture import OTLPEndpoint
from settings_telemetry import only_span

OTLP_BODY = {
    "resourceSpans": [{
        "resource": {"attributes": []},
        "scopeSpans": [{
            "scope": {"name": "gpuworks"},
            "spans": [{"traceId": "0123456789abcdef0123456789abcdef",
                       "spanId": "0123456789abcdef", "name": "gpuworks test span",
                       "startTimeUnixNano": "1700000000000000000",
                       "endTimeUnixNano": "1700000000001000000"}],
        }],
    }],
}


def post(endpoint: OTLPEndpoint, path: str, body: dict, headers: dict | None = None,
         gzip_body: bool = True):
    raw = json.dumps(body).encode()
    extra = {"Content-Type": "application/json"}
    if gzip_body:
        raw = gzip.compress(raw)
        extra["Content-Encoding"] = "gzip"
    extra.update(headers or {})
    request = urllib.request.Request(endpoint.base + path, data=raw, method="POST",
                                     headers=extra)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        response = opener.open(request, timeout=10)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        return response.status, response.read()


class OTLPFixtureTests(unittest.TestCase):
    def setUp(self):
        self.endpoint = OTLPEndpoint().start()

    def tearDown(self):
        self.endpoint.stop()

    # Mainline: a gzipped OTLP/JSON span -> fixture decompresses, records it
    # and answers like a collector.
    def test_gzipped_otlp_span_is_recorded(self):
        status, raw = post(self.endpoint, "/v1/traces", OTLP_BODY,
                           headers={"X-E2E-Token": "fixture-header"})
        self.assertEqual(status, 200, raw)
        requests = self.endpoint.snapshot()
        self.assertEqual(len(requests), 1)
        recorded = requests[0]
        self.assertFalse(recorded["failed"])
        self.assertEqual(recorded["headers"].get("x-e2e-token"), "fixture-header")
        self.assertEqual(recorded["headers"].get("content-encoding"), "gzip")
        span = recorded["body"]["resourceSpans"][0]["scopeSpans"][0]["spans"][0]
        self.assertEqual(span["name"], "gpuworks test span")

    # Mainline: the failure base answers 500 and still records the attempt.
    def test_failure_base_answers_500_and_records_the_attempt(self):
        status, _ = post(self.endpoint, "/fail/v1/traces", OTLP_BODY)
        self.assertEqual(status, 500)
        requests = self.endpoint.snapshot()
        self.assertEqual(len(requests), 1)
        self.assertTrue(requests[0]["failed"])

    # Mainline: a collector-rejected span ID is refused instead of recorded
    # as a successful export.
    def test_invalid_span_id_is_rejected(self):
        body = json.loads(json.dumps(OTLP_BODY))
        body["resourceSpans"][0]["scopeSpans"][0]["spans"][0]["spanId"] = "not-a-span-id"
        status, raw = post(self.endpoint, "/v1/traces", body)
        self.assertEqual(status, 400, raw)
        self.assertIn(b"invalid spanId", raw)
        self.assertIn("invalid spanId", self.endpoint.snapshot()[0]["errors"])

    # Mainline: duplicate span IDs across resource/scope groups are rejected.
    def test_duplicate_span_is_rejected(self):
        body = json.loads(json.dumps(OTLP_BODY))
        second = json.loads(json.dumps(OTLP_BODY["resourceSpans"][0]))
        second["scopeSpans"][0]["scope"]["name"] = "gpuworks-second"
        body["resourceSpans"].append(second)
        status, raw = post(self.endpoint, "/v1/traces", body)
        self.assertEqual(status, 400, raw)
        self.assertIn(b"duplicate spanId", raw)

    # Mainline: the lane's JSON content type is required.
    def test_json_content_type_is_required(self):
        status, raw = post(self.endpoint, "/v1/traces", OTLP_BODY,
                           headers={"Content-Type": "application/json-invalid"})
        self.assertEqual(status, 400, raw)
        self.assertIn(b"Content-Type", raw)

    def test_id_trailing_newline_and_wrong_scalar_types_are_rejected(self):
        for field, value in (("spanId", "0123456789abcdef\n"),
                             ("traceId", "0123456789abcdef0123456789abcdef\n"),
                             ("name", 7), ("traceId", []),
                             ("startTimeUnixNano", "not-time")):
            with self.subTest(field=field):
                body = json.loads(json.dumps(OTLP_BODY))
                body["resourceSpans"][0]["scopeSpans"][0]["spans"][0][field] = value
                status, raw = post(self.endpoint, "/v1/traces", body)
                self.assertEqual(status, 400, raw)

    def test_invalid_nested_otlp_shapes_are_rejected(self):
        for location, field, value in (
                ("scope", "scope", "invalid"),
                ("resource", "attributes", "invalid"),
                ("span", "attributes", [{"key": "broken", "value": "invalid"}]),
                ("span", "kind", "server"),
                ("span", "status", {"code": "ok"})):
            with self.subTest(location=location, field=field):
                body = json.loads(json.dumps(OTLP_BODY))
                resource = body["resourceSpans"][0]
                scope = resource["scopeSpans"][0]
                target = {"scope": scope, "resource": resource["resource"],
                          "span": scope["spans"][0]}[location]
                target[field] = value
                status, raw = post(self.endpoint, "/v1/traces", body)
                self.assertEqual(status, 400, raw)

    def test_non_json_numeric_constants_are_rejected(self):
        # json.dumps intentionally emits the non-JSON token for this negative
        # request, just as an invalid exporter could; a collector must reject it.
        for constant in (float("nan"), float("inf"), float("-inf")):
            with self.subTest(constant=constant):
                body = json.loads(json.dumps(OTLP_BODY))
                body["resourceSpans"][0]["resource"]["attributes"] = [
                    {"key": "invalid", "value": {"doubleValue": constant}}]
                status, raw = post(self.endpoint, "/v1/traces", body)
                self.assertEqual(status, 400, raw)
                self.assertIn(b"invalid JSON", raw)
        self.assertEqual(self.endpoint.snapshot(), [])

    def test_overflowing_json_double_is_rejected(self):
        for number in ("1e999", "-1e999"):
            with self.subTest(number=number):
                body = json.loads(json.dumps(OTLP_BODY))
                body["resourceSpans"][0]["resource"]["attributes"] = [
                    {"key": "invalid", "value": {"doubleValue": "NUMBER"}}]
                raw = json.dumps(body).replace('"NUMBER"', number).encode()
                request = urllib.request.Request(
                    self.endpoint.base + "/v1/traces", data=raw, method="POST",
                    headers={"Content-Type": "application/json"})
                opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
                try:
                    response = opener.open(request, timeout=10)
                except urllib.error.HTTPError as error:
                    response = error
                with response:
                    self.assertEqual(response.status, 400)

    def test_same_span_id_in_a_different_trace_is_valid(self):
        body = json.loads(json.dumps(OTLP_BODY))
        second = json.loads(json.dumps(body["resourceSpans"][0]["scopeSpans"][0]["spans"][0]))
        second["traceId"] = "abcdef0123456789abcdef0123456789"
        body["resourceSpans"][0]["scopeSpans"][0]["spans"].append(second)
        status, raw = post(self.endpoint, "/v1/traces", body)
        self.assertEqual(status, 200, raw)

    def test_only_span_counts_distinct_spans_in_every_scope_and_resource(self):
        for another_resource in (False, True):
            with self.subTest(another_resource=another_resource):
                body = json.loads(json.dumps(OTLP_BODY))
                scope = json.loads(json.dumps(body["resourceSpans"][0]["scopeSpans"][0]))
                scope["spans"][0]["spanId"] = "abcdef0123456789"
                if another_resource:
                    body["resourceSpans"].append({"resource": {}, "scopeSpans": [scope]})
                else:
                    body["resourceSpans"][0]["scopeSpans"].append(scope)
                with self.assertRaisesRegex(AssertionError, "2 spans"):
                    only_span({"body": body, "errors": []})

    # Mainline: malformed JSON is rejected without being recorded as a span.
    def test_invalid_json_is_rejected(self):
        raw = b"not json"
        request = urllib.request.Request(
            self.endpoint.base + "/v1/traces", data=raw, method="POST",
            headers={"Content-Type": "application/json"})
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        try:
            response = opener.open(request, timeout=10)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            self.assertEqual(response.status, 400)
        self.assertEqual(self.endpoint.snapshot(), [])

    def test_stop_releases_the_listener(self):
        port = int(self.endpoint.base.rsplit(":", 1)[1])
        self.endpoint.stop()
        with socket.socket() as probe:
            self.assertNotEqual(probe.connect_ex(("127.0.0.1", port)), 0)


if __name__ == "__main__":
    unittest.main()
