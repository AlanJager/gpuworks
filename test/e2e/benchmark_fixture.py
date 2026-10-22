"""Auditable OpenAI-compatible endpoint for benchmark browser acceptance.

E2E standard: test/e2e/browser-e2e-testing-standard.md
The fixture replaces only the external model service. GPUWorks' server, routes,
SQLite, scheduler-free benchmark execution and the browser stay real.

It implements exactly the surface the benchmark load generator uses:
``GET /v1/models`` and streamed ``POST /v1/chat/completions`` with
``stream_options.include_usage``. Invalid requests are rejected with a JSON
error and recorded, so a drifted request cannot silently succeed.
"""
from __future__ import annotations

import copy
import errno
import json
import socket
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class BenchmarkEndpoint:
    def __init__(self, model: str = "fixture-model", api_key: str = "fixture-key",
                 chunk_delay: float = 0.01):
        self.model = model
        self.api_key = api_key
        self.chunk_delay = chunk_delay
        self.requests: list[dict] = []
        self.models_requests: list[dict] = []
        # A cancellation case holds the first chunk until release_held(), then
        # tries a write the abandoned connection cannot take. aborted records
        # the client that left; completed counts streams that reached [DONE].
        self.aborted: list[dict] = []
        self.completed = 0
        self.hold_reached = threading.Event()
        self.release = threading.Event()
        self._hold_next = False
        self._hold_lock = threading.Lock()
        self.lock = threading.Lock()
        self._handlers = {}
        self._stopping = False
        self.server: ThreadingHTTPServer | None = None
        self.thread: threading.Thread | None = None

    def hold_next_request(self) -> None:
        """Hold the next measured request after its first content chunk."""
        with self._hold_lock:
            self._hold_next = True
        self.hold_reached.clear()
        self.release.clear()

    def release_held(self) -> None:
        self.release.set()

    @property
    def base(self) -> str:
        if self.server is None:
            raise RuntimeError("benchmark fixture is not started")
        host, port = self.server.server_address
        return f"http://{host}:{port}"

    def start(self) -> "BenchmarkEndpoint":
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def setup(self):
                super().setup()
                with fixture.lock:
                    fixture._handlers[threading.current_thread()] = self.connection
                    if fixture._stopping:
                        self.close_connection = True
                        self.connection.close()

            def finish(self):
                try:
                    super().finish()
                finally:
                    with fixture.lock:
                        fixture._handlers.pop(threading.current_thread(), None)

            def log_message(self, *_args):
                pass

            def _json(self, status: int, body: dict):
                raw = json.dumps(body).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def _authorized(self) -> bool:
                if not fixture.api_key:
                    return True
                if self.headers.get("Authorization") == f"Bearer {fixture.api_key}":
                    return True
                self._json(401, {"error": {"message": "invalid or missing API key",
                                           "type": "fixture_authentication_error"}})
                return False

            def do_GET(self):
                if self.path != "/v1/models":
                    self._json(404, {"error": {"message": "not found", "type": "fixture_not_found"}})
                    return
                record = {"method": "GET", "path": self.path,
                          "authorization": self.headers.get("Authorization", "")}
                with fixture.lock:
                    fixture.models_requests.append(record)
                if not self._authorized():
                    return
                self._json(200, {"object": "list", "data": [
                    {"id": fixture.model, "object": "model", "created": 0, "owned_by": "fixture"}]})

            def do_POST(self):
                if self.path != "/v1/chat/completions":
                    self._json(404, {"error": {"message": "not found", "type": "fixture_not_found"}})
                    return
                length = int(self.headers.get("Content-Length", "0"))
                raw = self.rfile.read(length)
                try:
                    body = json.loads(raw)
                except json.JSONDecodeError:
                    self._json(400, {"error": {"message": "invalid JSON body",
                                               "type": "fixture_invalid_request"}})
                    return
                record = {"method": "POST", "path": self.path, "body": body,
                          "authorization": self.headers.get("Authorization", "")}
                errors = []
                if not self._authorized():
                    with fixture.lock:
                        fixture.requests.append({**record, "validation": ["authorization"]})
                    return
                if not isinstance(body, dict):
                    with fixture.lock:
                        fixture.requests.append({**record, "validation": ["body must be an object"]})
                    self._json(400, {"error": {"message": "body must be an object"}})
                    return
                messages = body.get("messages")
                if body.get("model") != fixture.model:
                    errors.append("model must equal the advertised fixture model")
                if (not isinstance(messages, list) or not messages
                        or any(not isinstance(m, dict)
                               or not isinstance(m.get("role"), str)
                               or m["role"] not in {"user", "assistant", "system"}
                               or not isinstance(m.get("content"), str) for m in messages)):
                    errors.append("messages must be a non-empty list")
                if body.get("stream") is not True:
                    errors.append("stream must be true")
                if (not isinstance(body.get("stream_options"), dict)
                        or body["stream_options"].get("include_usage") is not True):
                    errors.append("stream_options.include_usage must be true")
                if type(body.get("max_tokens")) is not int or body["max_tokens"] <= 0:
                    errors.append("max_tokens must be a positive integer")
                record["validation"] = errors
                with fixture.lock:
                    fixture.requests.append(record)
                if errors:
                    self._json(400, {"error": {"message": "; ".join(errors),
                                               "type": "fixture_invalid_request"}})
                    return

                # Two content chunks give TTFT and ITL a real, deterministic
                # timeline; the usage chunk is where the product reads tokens.
                pieces = ["fixture ", "answer"][:body["max_tokens"]]
                prompt_tokens = max(1, sum(max(1, (len(m["content"]) + 3) // 4)
                                           for m in messages))
                usage = {"prompt_tokens": prompt_tokens,
                         "completion_tokens": len(pieces),
                         "total_tokens": prompt_tokens + len(pieces)}
                with fixture.lock:
                    record["response_usage"] = usage
                held = False
                try:
                    self.send_response(200)
                    self.send_header("Content-Type", "text/event-stream")
                    self.send_header("Cache-Control", "no-cache")
                    self.send_header("Connection", "close")
                    self.end_headers()
                    for index, piece in enumerate(pieces):
                        event = {"choices": [{"index": 0, "delta": {"content": piece}}]}
                        self.wfile.write(f"data: {json.dumps(event)}\n\n".encode())
                        self.wfile.flush()
                        if index == 0:
                            with fixture._hold_lock:
                                held = fixture._hold_next
                                fixture._hold_next = False
                            if held:
                                fixture.hold_reached.set()
                                fixture.release.wait(timeout=120)
                                # A canceled client has closed the connection;
                                # force a write beyond a socket buffer so the
                                # abort is observed rather than assumed.
                                self.connection.settimeout(5)
                                # SSE comments probe a closed peer without adding
                                # generated content or exceeding max_tokens.
                                self.wfile.write(b": " + b"x" * 65536 + b"\n\n")
                                self.wfile.flush()
                        time.sleep(fixture.chunk_delay)
                    self.wfile.write(f"data: {json.dumps({'choices': [], 'usage': usage})}\n\n"
                                     .encode())
                    self.wfile.write(b"data: [DONE]\n\n")
                    self.wfile.flush()
                    with fixture.lock:
                        record["usage"] = usage
                        fixture.completed += 1
                except (BrokenPipeError, ConnectionResetError, socket.timeout) as error:
                    with fixture.lock:
                        fixture.aborted.append({"error": type(error).__name__,
                                                "held": held})

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.server.daemon_threads = True
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        return self

    @property
    def active_handler_count(self) -> int:
        with self.lock:
            return len(self._handlers)

    @property
    def active_handlers(self) -> int:
        return self.active_handler_count

    @staticmethod
    def _close_connection(connection):
        clean = True
        try:
            connection.shutdown(socket.SHUT_RDWR)
        except OSError as error:
            clean = error.errno in (errno.ENOTCONN, errno.EBADF)
        finally:
            connection.close()
        return clean

    def stop(self) -> bool:
        with self.lock:
            self._stopping = True
            active = list(self._handlers.items())
        self.release_held()
        clean = True
        for _, connection in active:
            clean = self._close_connection(connection) and clean
        if self.server is not None:
            self.server.shutdown()
            self.server.server_close()
            self.server = None
        if self.thread is not None:
            self.thread.join(timeout=5)
            if self.thread.is_alive():
                return False
            self.thread = None
        deadline = time.monotonic() + 5
        while True:
            with self.lock:
                remaining = list(self._handlers.items())
            if not remaining:
                return clean
            for handler, connection in remaining:
                clean = self._close_connection(connection) and clean
                handler.join(timeout=max(0, deadline - time.monotonic()))
            if time.monotonic() >= deadline:
                return clean and self.active_handler_count == 0

    def snapshot(self) -> dict:
        with self.lock:
            return {"base": self.base if self.server else "",
                    "models_requests": list(self.models_requests),
                    "requests": copy.deepcopy(self.requests),
                    "aborted": list(self.aborted),
                    "completed": self.completed,
                    "active_handlers": len(self._handlers)}

    def __enter__(self) -> "BenchmarkEndpoint":
        return self.start()

    def __exit__(self, *_exc) -> None:
        self.stop()
