"""Auditable OTLP/HTTP collector fixture for browser acceptance.

E2E standard: test/e2e/browser-e2e-testing-standard.md
The fixture replaces only the tracing collector. GPUWorks' server, settings,
encrypted header storage, browser and SQLite stay real.

The exporter posts a gzipped OTLP/JSON document to /v1/traces. A base path
ending in /fail answers 500 so the test-span failure path can be exercised.
"""
from __future__ import annotations

import gzip
import json
import math
import re
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

TRACE_ID = re.compile(r"^[0-9a-f]{32}$")
SPAN_ID = re.compile(r"^[0-9a-f]{16}$")


def reject_json_constant(_constant):
    # Python accepts NaN/Infinity by default; these are not JSON wire tokens.
    raise ValueError("non-JSON numeric constant")



def valid_value(value) -> bool:
    """The exporter's OTLP AnyValue union, including typed array members."""
    if not isinstance(value, dict) or len(value) != 1:
        return False
    kind, item = next(iter(value.items()))
    if kind == "arrayValue":
        return (isinstance(item, dict) and set(item) == {"values"}
                and isinstance(item["values"], list)
                and all(valid_value(member) for member in item["values"]))
    if kind == "intValue":
        return (isinstance(item, str) and bool(re.fullmatch(r"-?[0-9]{1,19}", item))
                and -(2**63) <= int(item) < 2**63)
    if kind == "doubleValue":
        try:
            return type(item) in (int, float) and math.isfinite(item)
        except OverflowError:
            return False
    return {"stringValue": isinstance(item, str),
            "boolValue": isinstance(item, bool)}.get(kind, False)


def valid_attributes(attributes) -> bool:
    return isinstance(attributes, list) and all(
        isinstance(attribute, dict) and set(attribute) == {"key", "value"}
        and isinstance(attribute["key"], str) and valid_value(attribute["value"])
        for attribute in attributes)


def valid_span_shape(span) -> bool:
    if not isinstance(span, dict):
        return False
    if not valid_attributes(span.get("attributes", [])):
        return False
    for field, maximum in (("kind", 5), ("flags", 2**32 - 1)):
        if field in span and (type(span[field]) is not int or not 0 <= span[field] <= maximum):
            return False
    if not isinstance(span.get("traceState", ""), str):
        return False
    parent = span.get("parentSpanId", "")
    if not isinstance(parent, str) or (parent and not SPAN_ID.fullmatch(parent)):
        return False
    status = span.get("status", {})
    if (not isinstance(status, dict) or type(status.get("code", 0)) is not int
            or not 0 <= status.get("code", 0) <= 2
            or not isinstance(status.get("message", ""), str)):
        return False
    events = span.get("events", [])
    return isinstance(events, list) and all(
        isinstance(event, dict) and isinstance(event.get("name"), str)
        and isinstance(event.get("timeUnixNano"), str)
        and bool(re.fullmatch(r"[0-9]{1,20}", event["timeUnixNano"]))
        and int(event["timeUnixNano"]) <= 2**64 - 1
        and valid_attributes(event.get("attributes", [])) for event in events)


def otlp_errors(body) -> list[str]:
    """Validate the OTLP/JSON shape and the IDs a collector would reject."""
    if not isinstance(body, dict):
        return ["body must be an object"]
    resource_spans = body.get("resourceSpans")
    if not isinstance(resource_spans, list) or not resource_spans:
        return ["resourceSpans must be a non-empty list"]
    errors: list[str] = []
    seen: set[tuple[str, str]] = set()
    total = 0
    for resource_index, resource in enumerate(resource_spans):
        if not isinstance(resource, dict) or not isinstance(resource.get("resource"), dict):
            errors.append(f"resourceSpans[{resource_index}].resource missing")
            continue
        if not valid_attributes(resource["resource"].get("attributes", [])):
            errors.append("invalid resource attributes")
        scopes = resource.get("scopeSpans")
        if not isinstance(scopes, list) or not scopes:
            errors.append(f"resourceSpans[{resource_index}].scopeSpans is empty")
            continue
        for scope_index, scope in enumerate(scopes):
            instrumentation = scope.get("scope", {}) if isinstance(scope, dict) else None
            if (not isinstance(instrumentation, dict)
                    or any(not isinstance(instrumentation.get(field, ""), str)
                           for field in ("name", "version"))):
                errors.append("invalid instrumentation scope")
            spans = scope.get("spans") if isinstance(scope, dict) else None
            if not isinstance(spans, list) or not spans:
                errors.append(f"scopeSpans[{scope_index}].spans is empty")
                continue
            for span in spans:
                total += 1
                if not valid_span_shape(span):
                    errors.append("invalid span shape")
                trace_id = span.get("traceId") if isinstance(span, dict) else None
                span_id = span.get("spanId") if isinstance(span, dict) else None
                if (not isinstance(trace_id, str) or not TRACE_ID.fullmatch(trace_id)
                        or set(trace_id) == {"0"}):
                    errors.append("invalid traceId")
                if (not isinstance(span_id, str) or not SPAN_ID.fullmatch(span_id)
                        or set(span_id) == {"0"}):
                    errors.append("invalid spanId")
                elif isinstance(trace_id, str) and (trace_id, span_id) in seen:
                    errors.append("duplicate spanId")
                if isinstance(span, dict) and (not isinstance(span.get("name"), str)
                                              or not span["name"]):
                    errors.append("span name missing")
                if isinstance(span, dict):
                    for field in ("startTimeUnixNano", "endTimeUnixNano"):
                        value = span.get(field)
                        if (not isinstance(value, str)
                                or not re.fullmatch(r"[0-9]{1,20}", value)
                                or int(value) > 2**64 - 1):
                            errors.append(f"invalid {field}")
                if isinstance(trace_id, str) and isinstance(span_id, str):
                    seen.add((trace_id, span_id))
    if total == 0:
        errors.append("no spans")
    return errors


class OTLPEndpoint:
    def __init__(self):
        self.requests: list[dict] = []
        self.lock = threading.Lock()
        self.server: ThreadingHTTPServer | None = None
        self.thread: threading.Thread | None = None

    @property
    def base(self) -> str:
        if self.server is None:
            raise RuntimeError("OTLP fixture is not started")
        host, port = self.server.server_address
        return f"http://{host}:{port}"

    @property
    def fail_base(self) -> str:
        return self.base + "/fail"

    def start(self) -> "OTLPEndpoint":
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *_args):
                pass

            def _reply(self, status: int, body: dict):
                raw = json.dumps(body).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def do_POST(self):
                length = int(self.headers.get("Content-Length", "0"))
                raw = self.rfile.read(length)
                if self.headers.get("Content-Encoding") == "gzip":
                    try:
                        raw = gzip.decompress(raw)
                    except (OSError, EOFError):
                        self._reply(400, {"error": "bad gzip"})
                        return
                try:
                    body = json.loads(raw, parse_constant=reject_json_constant)
                except (ValueError, UnicodeDecodeError):
                    self._reply(400, {"error": "invalid JSON"})
                    return
                errors = otlp_errors(body)
                content_type = self.headers.get("Content-Type", "")
                if content_type.partition(";")[0].strip().lower() != "application/json":
                    errors.append("Content-Type must be application/json")
                failed = self.path.startswith("/fail/")
                with fixture.lock:
                    fixture.requests.append({
                        "path": self.path,
                        "headers": {name.lower(): value
                                    for name, value in self.headers.items()},
                        "body": body,
                        "errors": errors,
                        "failed": failed,
                    })
                if errors:
                    self._reply(400, {"error": "; ".join(errors)})
                    return
                if failed:
                    self._reply(500, {"error": "collector unavailable"})
                    return
                if not self.path.endswith("/v1/traces"):
                    self._reply(404, {"error": "not found"})
                    return
                self._reply(200, {"partialSuccess": {}})

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.server.daemon_threads = True
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        return self

    def stop(self) -> None:
        if self.server is not None:
            self.server.shutdown()
            self.server.server_close()
            self.server = None
        if self.thread is not None:
            self.thread.join(timeout=5)
            self.thread = None

    def snapshot(self) -> list[dict]:
        with self.lock:
            return list(self.requests)

    def __enter__(self) -> "OTLPEndpoint":
        return self.start()

    def __exit__(self, *_exc) -> None:
        self.stop()
