"""Audited local HF/ModelScope metadata boundaries; never a management stub.

E2E standard: test/e2e/browser-e2e-testing-standard.md.
The real server's source rules and HF browser metadataSources API select
these origins. Requests hit an actual HTTP listener; no browser route mocks.
"""
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import re
import threading
from urllib.parse import parse_qs, unquote, urlsplit

CONFIG = json.dumps({"architectures": ["Qwen2ForCausalLM"], "model_type": "qwen2",
    "torch_dtype": "float16", "max_position_embeddings": 4096,
    "hidden_size": 128, "num_hidden_layers": 2, "num_attention_heads": 4,
    "num_key_value_heads": 2, "vocab_size": 256}, separators=(",", ":"))
WEIGHT_BYTES = 1048576
SHA = "1234567890abcdef1234567890abcdef12345678"
README = "# Owned model metadata\n\nAudited browser registration fixture.\n"
INDEX = json.dumps({"metadata": {"total_size": WEIGHT_BYTES},
                    "weight_map": {"a": "model.safetensors"}}, separators=(",", ":"))


def encoded(value):
    return json.dumps(value, separators=(",", ":")).encode()


class ModelHub:
    def __init__(self, repo):
        if not re.fullmatch(r"e2e-[a-z0-9-]+/[a-z0-9-]+", repo):
            raise ValueError("repository must use this run's owned e2e identity")
        self.repo = repo
        self.listener = self.thread = None
        self.base = ""
        self._available = True
        self._audit = []
        self._active = 0
        self._condition = threading.Condition()

    def available(self, value):
        if type(value) is not bool:
            raise ValueError("availability must be a boolean")
        with self._condition:
            self._available = value

    def audit(self):
        with self._condition:
            return json.loads(json.dumps(self._audit))

    def answer(self, method, target, headers=None, body=b""):
        headers = {k.lower(): v for k, v in (headers or {}).items()}
        if method != "GET":
            return 405, encoded({"error": "Method not allowed"})
        if body or headers.get("authorization") or headers.get("cookie"):
            return 400, encoded({"error": "Unexpected body or credentials"})
        parts = urlsplit(target)
        query = parse_qs(parts.query, keep_blank_values=True)
        path = unquote(parts.path)
        hf = "/hf/api/models/" + self.repo
        ms = "/ms/api/v1/models/" + self.repo
        status, response = 404, encoded({"error": "Repository or file not found"})
        if path in (hf, hf + "/revision/main", hf + "/revision/" + SHA):
            if query not in ({}, {"blobs": ["true"]}) or (path != hf and query):
                return 400, encoded({"error": "Invalid repository query"})
            status, response = 200, encoded({"sha": SHA, "siblings": [
                {"rfilename": "model.safetensors", "size": WEIGHT_BYTES}],
                "createdAt": "2025-01-02T03:04:05Z", "lastModified": "2025-01-03T04:05:06Z"})
        elif path == ms:
            if query:
                return 400, encoded({"Code": 400, "Message": "Unexpected query"})
            status, response = 200, encoded({"Code": 200, "Data": {
                "CreatedTime": 1735787045, "LastUpdatedTime": 1735877106}})
        elif path == ms + "/repo/files":
            if query.get("Recursive") != ["true"] or set(query) - {"Recursive", "Revision"} or query.get("Revision", ["master"]) not in (["master"], [SHA]):
                return 400, encoded({"Code": 400, "Message": "Invalid listing query"})
            status, response = 200, encoded({"Code": 200, "Data": {
                "LatestCommitter": {"CommittedDate": 1735877106, "ShortId": SHA[:8]},
                "Files": [{"Path": "model.safetensors", "Name": "model.safetensors",
                    "Type": "blob", "Size": WEIGHT_BYTES, "Revision": SHA,
                    "CommittedDate": 1735877106}]}})
        else:
            file = None
            for prefix, refs in (("/hf/" + self.repo, ("main", SHA)),
                                 ("/ms/models/" + self.repo, ("master", SHA))):
                for mode in (("raw", "resolve") if prefix.startswith("/hf/") else ("resolve",)):
                    for revision in refs:
                        root = f"{prefix}/{mode}/{revision}/"
                        if path.startswith(root) and not query:
                            file = path[len(root):]
            if path == ms + "/repo":
                if set(query) != {"FilePath"} or len(query["FilePath"]) != 1:
                    return 400, encoded({"Code": 400, "Message": "Invalid file query"})
                file = query["FilePath"][0]
            files = {"config.json": CONFIG, "README.md": README,
                     "model.safetensors.index.json": INDEX}
            if file in files:
                status, response = 200, files[file].encode()
        with self._condition:
            available = self._available
        if status == 200 and not available:
            return 503, encoded({"error": "Owned metadata source unavailable"})
        return status, response

    def start(self):
        owner = self
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def do_GET(self):
                self.respond()

            do_POST = do_PUT = do_DELETE = do_HEAD = do_GET

            def respond(self):
                with owner._condition:
                    owner._active += 1
                try:
                    length = int(self.headers.get("Content-Length", "0"))
                    body = self.rfile.read(min(length, 8192)) if length else b""
                    status, value = owner.answer(self.command, self.path, dict(self.headers), body)
                    origin = self.headers.get("Origin", "")
                    if origin and not re.fullmatch(r"http://127\.0\.0\.1:[0-9]+", origin):
                        status, value = 403, encoded({"error": "Origin forbidden"})
                    with owner._condition:
                        owner._audit.append({"method": self.command, "target": self.path,
                            "origin": origin, "status": status, "bytes": len(value),
                            "sha256": hashlib.sha256(value).hexdigest()})
                    self.send_response(status)
                    self.send_header("Content-Type", "application/json; charset=utf-8")
                    self.send_header("Content-Length", str(len(value)))
                    if origin and status != 403:
                        self.send_header("Access-Control-Allow-Origin", origin)
                        self.send_header("Vary", "Origin")
                    self.end_headers()
                    if self.command != "HEAD":
                        self.wfile.write(value)
                finally:
                    with owner._condition:
                        owner._active -= 1
                        owner._condition.notify_all()
        self.listener = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.listener.daemon_threads = True
        self.base = "http://127.0.0.1:" + str(self.listener.server_port)
        self.thread = threading.Thread(target=self.listener.serve_forever, daemon=True)
        self.thread.start()
        return self.base

    def stop(self):
        if self.listener is not None:
            self.listener.shutdown()
            self.listener.server_close()
        if self.thread is not None:
            self.thread.join(timeout=3)
        with self._condition:
            zero = self._condition.wait_for(lambda: self._active == 0, timeout=3)
            return {"listener_stopped": self.listener is None or self.listener.fileno() == -1,
                    "thread_stopped": self.thread is None or not self.thread.is_alive(),
                    "handlers": self._active, "stopped": zero and (self.thread is None or not self.thread.is_alive())}
