"""Audited loopback HTTP model directory; metadata only, not executable weights.

E2E standard: test/e2e/browser-e2e-testing-standard.md
"""
from __future__ import annotations

import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import re
import threading
from urllib.parse import urlsplit


class ModelDirectory:
    def __init__(self, run_id):
        self.root = f"/e2e-{run_id}/tiny-model"
        self.config = {"architectures": ["Qwen3ForCausalLM"], "model_type": "qwen3",
                       "hidden_size": 1024, "num_hidden_layers": 28,
                       "max_position_embeddings": 40960, "torch_dtype": "bfloat16"}
        self.files = {"config.json": json.dumps(self.config).encode(),
                      "model.safetensors": b"fixture-weight-size-only\n" * 433}
        self.available = True
        self.records = []
        self.lock = threading.Lock()
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_args):
                pass

            def do_GET(self):
                self.respond()

            def do_HEAD(self):
                self.respond()

            def do_POST(self):
                self.respond()

            def respond(self):
                path = urlsplit(self.path).path
                status, content_type, headers = 200, "application/octet-stream", {}
                with fixture.lock:
                    available = fixture.available
                if self.command not in {"GET", "HEAD"}:
                    status, body = 405, b"method not allowed"
                    headers["Allow"] = "GET, HEAD"
                elif not available:
                    status, body = 503, b"scoped model fixture unavailable"
                elif path == fixture.root:
                    status, body = 301, b""
                    headers["Location"] = fixture.root + "/"
                elif path == fixture.root + "/":
                    content_type = "text/html; charset=utf-8"
                    body = ('<html><body><table>' + ''.join(
                        f'<tr><td><a href="{name}">{name}</a></td><td>2026-10-01 00:00</td><td>{len(data)}</td></tr>'
                        for name, data in fixture.files.items()) + '</table></body></html>').encode()
                else:
                    name = path.removeprefix(fixture.root + "/")
                    body = fixture.files.get(name)
                    if body is None or not path.startswith(fixture.root + "/"):
                        status, body = 404, b"not found"
                    else:
                        content_type = "application/json" if name.endswith(".json") else content_type
                        headers["Accept-Ranges"] = "bytes"
                        headers["ETag"] = '"' + hashlib.sha256(body).hexdigest() + '"'
                        value = self.headers.get("Range")
                        if value:
                            match = re.fullmatch(r"bytes=(\d+)-(\d*)", value)
                            start = int(match[1]) if match else len(body)
                            end = min(int(match[2]) if match and match[2] else len(body) - 1, len(body) - 1)
                            if start > end:
                                headers["Content-Range"] = f"bytes */{len(body)}"
                                status, body = 416, b""
                            else:
                                headers["Content-Range"] = f"bytes {start}-{end}/{len(body)}"
                                status, body = 206, body[start:end + 1]
                with fixture.lock:
                    fixture.records.append({"method": self.command, "path": self.path, "status": status,
                        "range": self.headers.get("Range"), "bytes": len(body),
                        "sha256": hashlib.sha256(body).hexdigest()})
                self.send_response(status)
                self.send_header("Content-Type", content_type)
                self.send_header("Content-Length", str(len(body)))
                for key, value in headers.items():
                    self.send_header(key, value)
                self.end_headers()
                if self.command != "HEAD":
                    self.wfile.write(body)

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.url = f"http://127.0.0.1:{self.server.server_port}{self.root}"
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    def start(self):
        self.thread.start()
        return self

    def set_available(self, available):
        with self.lock:
            self.available = available

    def audit(self):
        with self.lock:
            return [dict(record) for record in self.records]

    def stop(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)
        return not self.thread.is_alive()
