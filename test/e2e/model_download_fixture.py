"""Exact loopback repository bytes for real model-download E2E.

Run self-tests: python3 -B -m unittest discover -s test/e2e -p test_model_download_fixture.py
Only the external repository is replaced. Synthetic tensors validate file transfer,
not model inference. Body audit counts socket.send returns, never Content-Length.
"""
from __future__ import annotations

import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import re
import socket
import struct
import threading
import time
from urllib.parse import parse_qs, unquote, urlsplit

REVISION = "1234567890abcdef1234567890abcdef12345678"


def encoded(value):
    return json.dumps(value, separators=(",", ":")).encode()


def tiny_files():
    config = {"architectures": ["Qwen2ForCausalLM"], "model_type": "qwen2",
              "torch_dtype": "float16", "max_position_embeddings": 4096,
              "hidden_size": 128, "num_hidden_layers": 2, "num_attention_heads": 4,
              "num_key_value_heads": 2, "vocab_size": 256}
    # Valid safetensors envelope, one deterministic synthetic U8 tensor.
    payload = bytes(range(256)) * 8192
    header = encoded({"owned_fixture": {"dtype": "U8", "shape": [len(payload)],
                                       "data_offsets": [0, len(payload)]}})
    header += b" " * (-len(header) % 8)
    return {"config.json": encoded(config),
            "model.safetensors": struct.pack("<Q", len(header)) + header + payload}


class ModelDownload:
    """URL directory plus HF/MS-compatible owned repository routes.

    start() returns self; URL source uses .url, HF endpoint .base+'/hf',
    MS endpoint .base+'/ms', and both hubs use .repo and REVISION.
    stop() releases barriers, shuts active sockets, observes all handlers and
    listener. A false result must remain a cleanup failure in the caller.
    """
    def __init__(self, run_id, repo=None):
        if not re.fullmatch(r"[A-Za-z0-9-]+", run_id):
            raise ValueError("run_id must contain only letters, digits and hyphens")
        self.repo = repo or f"e2e-{run_id.lower()}/tiny-model"
        if not re.fullmatch(r"e2e-[a-z0-9-]+/[a-z0-9-]+", self.repo):
            raise ValueError("repository must have an owned e2e identity")
        self.root = f"/e2e-{run_id}/tiny-model"
        self.files = tiny_files()
        self.base = self.url = ""
        self.server = self.thread = None
        self._cv = threading.Condition()
        self._records, self._sockets = [], set()
        self._failure = False
        self._interrupt = None
        self._interrupted = threading.Event()
        self._release = threading.Event()
        self._stopping = False

    def set_weight_failure(self, value):
        if type(value) is not bool:
            raise ValueError("weight failure must be boolean")
        with self._cv:
            self._failure = value

    def interrupt_next_weight(self, after_bytes=65536, hold=True):
        if type(after_bytes) is not int or not 0 < after_bytes < len(self.files['model.safetensors']):
            raise ValueError("interruption offset must be inside the weight file")
        if type(hold) is not bool:
            raise ValueError("hold must be boolean")
        with self._cv:
            if self._interrupt is not None or self._stopping:
                raise RuntimeError("interruption already armed or fixture stopping")
            self._interrupted.clear()
            self._release.clear()
            self._interrupt = (after_bytes, hold)

    def wait_interrupted(self, timeout=5):
        return self._interrupted.wait(timeout)

    def release(self):
        self._release.set()

    def audit(self):
        with self._cv:
            return json.loads(json.dumps(self._records))

    def wait_idle(self, timeout=5):
        with self._cv:
            return self._cv.wait_for(lambda: not self._sockets, timeout)

    def _route(self, target):
        parts = urlsplit(target)
        path, query = unquote(parts.path), parse_qs(parts.query, keep_blank_values=True)
        if parts.fragment:
            return 400, b"fragment forbidden", None
        if path == self.root and not query:
            return 301, b"", {"Location": self.root + "/"}
        if path == self.root + "/" and not query:
            rows = ''.join(f'<tr><td><a href="{n}">{n}</a></td><td>2026-10-01 00:00</td><td>{len(b)}</td></tr>'
                           for n, b in self.files.items())
            return 200, ('<html><body><table>' + rows + '</table></body></html>').encode(), {"Content-Type": "text/html; charset=utf-8"}
        if path.startswith(self.root + "/") and not query:
            name = path[len(self.root) + 1:]
            if name in self.files:
                return 200, self.files[name], {"file": name}
        hf = "/hf/api/models/" + self.repo
        if path in (hf, hf + "/revision/main", hf + "/revision/" + REVISION):
            if query not in ({}, {"blobs": ["true"]}) or (path != hf and query):
                return 400, encoded({"error": "Invalid repository query"}), {}
            return 200, encoded({"sha": REVISION, "siblings": [
                {"rfilename": n, "size": len(b)} for n, b in self.files.items()]}), {}
        # Agent uses the HF tree API rather than the metadata siblings view.
        if path in (hf + "/tree/main", hf + "/tree/" + REVISION):
            if set(query) - {"recursive", "expand"} or any(v not in (["true"], ["false"]) for v in query.values()):
                return 400, encoded({"error": "Invalid tree query"}), {}
            return 200, encoded([{"type": "file", "path": n, "size": len(b)} for n, b in self.files.items()]), {}
        ms = "/ms/api/v1/models/" + self.repo
        if path == ms and not query:
            return 200, encoded({"Code": 200, "Data": {"CreatedTime": 1735787045, "LastUpdatedTime": 1735877106}}), {}
        if path == ms + "/repo/files":
            if query.get("Recursive") != ["true"] or set(query) - {"Recursive", "Revision"} or query.get("Revision", ["master"]) not in (["master"], [REVISION]):
                return 400, encoded({"Code": 400, "Message": "Invalid listing query"}), {}
            return 200, encoded({"Code": 200, "Data": {"Files": [
                {"Path": n, "Name": n, "Type": "blob", "Size": len(b), "Revision": REVISION}
                for n, b in self.files.items()]}}), {}
        name = None
        if path == ms + "/repo":
            if set(query) != {"FilePath"} or len(query["FilePath"]) != 1:
                return 400, encoded({"Code": 400, "Message": "Invalid file query"}), {}
            name = query["FilePath"][0]
        elif not query:
            for prefix, refs, modes in (("/hf/" + self.repo, ("main", REVISION), ("raw", "resolve")),
                                        ("/ms/models/" + self.repo, ("master", REVISION), ("resolve",))):
                for revision in refs:
                    for mode in modes:
                        root = f"{prefix}/{mode}/{revision}/"
                        if path.startswith(root):
                            name = path[len(root):]
        if name in self.files:
            return 200, self.files[name], {"file": name}
        return 404, encoded({"error": "Repository or file not found"}), {}

    def _answer(self, method, target, headers):
        if method not in ("GET", "HEAD"):
            return 405, b"method not allowed", {"Allow": "GET, HEAD"}, None
        if headers.get("Content-Length", "0") != "0" or headers.get("Transfer-Encoding"):
            return 400, b"request body forbidden", {}, None
        if headers.get("Authorization") or headers.get("Cookie"):
            return 400, b"credentials forbidden", {}, None
        status, body, extra = self._route(target)
        extra = dict(extra or {})
        name = extra.pop("file", None)
        cut = None
        if name:
            extra.update({"Accept-Ranges": "bytes", "ETag": '"' + hashlib.sha256(body).hexdigest() + '"'})
            extra["Content-Type"] = "application/json" if name.endswith('.json') else "application/octet-stream"
            with self._cv:
                failed = self._failure
            if name == "model.safetensors" and failed:
                return 503, b"owned weight source unavailable", {}, None
            value = headers.get("Range")
            if value:
                match = re.fullmatch(r"bytes=(\d+)-(\d*)", value)
                start = int(match[1]) if match else len(body)
                end = min(int(match[2]) if match and match[2] else len(body)-1, len(body)-1)
                if start > end:
                    return 416, b"", {"Content-Range": f"bytes */{len(body)}"}, None
                extra["Content-Range"] = f"bytes {start}-{end}/{len(body)}"
                status, body = 206, body[start:end+1]
            if name == 'model.safetensors' and method == 'GET':
                with self._cv:
                    if self._interrupt and len(body) > self._interrupt[0]:
                        cut, self._interrupt = self._interrupt, None
        return status, body, extra, cut

    def start(self):
        if self.server is not None:
            raise RuntimeError("fixture can only start once")
        owner = self
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def do_GET(self):
                self.respond()
            do_HEAD = do_POST = do_PUT = do_DELETE = do_PATCH = do_OPTIONS = do_GET

            def respond(self):
                record = {"method": self.command, "target": self.path, "range": self.headers.get('Range'),
                          "bytes_sent": 0, "complete": False, "interrupted": False}
                sent = hashlib.sha256()
                with owner._cv:
                    owner._records.append(record)
                try:
                    self.connection.settimeout(3)
                    status, body, headers, cut = owner._answer(self.command, self.path, self.headers)
                    origin = self.headers.get('Origin', '')
                    if origin and not re.fullmatch(r'http://127\.0\.0\.1:[0-9]+', origin):
                        status, body, headers, cut = 403, b'origin forbidden', {}, None
                    elif origin:
                        headers.update({'Access-Control-Allow-Origin': origin, 'Vary': 'Origin'})
                    record.update(status=status, planned_bytes=len(body))
                    self.send_response(status)
                    self.send_header('Content-Length', str(len(body)))
                    self.send_header('Content-Type', headers.pop('Content-Type', 'application/json; charset=utf-8'))
                    self.send_header('Connection', 'close')
                    for key, value in headers.items():
                        self.send_header(key, value)
                    self.end_headers()
                    limit = cut[0] if cut else len(body)
                    if self.command != 'HEAD':
                        while record['bytes_sent'] < limit:
                            offset = record['bytes_sent']
                            chunk = body[offset:min(offset + 32768, limit)]
                            count = self.connection.send(chunk)
                            if not count:
                                raise ConnectionError('zero-byte socket send')
                            sent.update(chunk[:count])
                            with owner._cv:
                                record['bytes_sent'] += count
                    if cut:
                        with owner._cv:
                            record['interrupted'] = True
                        owner._interrupted.set()
                        if cut[1] and not owner._release.wait(30):
                            raise TimeoutError('interrupted response barrier timed out')
                        self.connection.shutdown(socket.SHUT_RDWR)
                    else:
                        with owner._cv:
                            record['complete'] = True
                except (OSError, TimeoutError) as error:
                    with owner._cv:
                        record['transport_error'] = type(error).__name__
                finally:
                    with owner._cv:
                        record['sha256_sent'] = sent.hexdigest()
                        owner._cv.notify_all()
                self.close_connection = True
        class Server(ThreadingHTTPServer):
            def get_request(self):
                connection, address = super().get_request()
                connection.settimeout(3)
                # Register synchronously before ThreadingMixIn can dispatch the
                # handler: setup() may block before Python's streams exist.
                with owner._cv:
                    owner._sockets.add(connection)
                    owner._cv.notify_all()
                return connection, address

            def shutdown_request(self, request):
                try:
                    super().shutdown_request(request)
                finally:
                    with owner._cv:
                        owner._sockets.discard(request)
                        owner._cv.notify_all()

        self.server = Server(('127.0.0.1', 0), Handler)
        self.server.daemon_threads = True
        self.base = f'http://127.0.0.1:{self.server.server_port}'
        self.url = self.base + self.root
        self.thread = threading.Thread(target=self.server.serve_forever, kwargs={'poll_interval': .05}, daemon=True)
        self.thread.start()
        return self

    def stop(self, timeout=5):
        deadline = time.monotonic() + timeout
        with self._cv:
            self._stopping = True
        self.release()
        if self.server:
            self.server.shutdown()
            self.server.server_close()
        with self._cv:
            sockets = list(self._sockets)
        for connection in sockets:
            try:
                connection.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
        if self.thread:
            self.thread.join(max(0, deadline - time.monotonic()))
        with self._cv:
            zero = self._cv.wait_for(lambda: not self._sockets, max(0, deadline - time.monotonic()))
            thread_stopped = self.thread is None or not self.thread.is_alive()
            listener_stopped = self.server is None or self.server.fileno() == -1
            return {'handlers': len(self._sockets), 'thread_stopped': thread_stopped,
                    'listener_stopped': listener_stopped, 'stopped': zero and thread_stopped and listener_stopped}
