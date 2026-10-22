"""Audited external OCI tag-list and docker-save archive fixture (no management API).

E2E standard: test/e2e/browser-e2e-testing-standard.md
TLS trust is scoped to the runner environment; no host CA store is modified.
"""
from __future__ import annotations

from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import io
import json
import re
import ssl
import subprocess
import tarfile
import threading
from urllib.parse import urlsplit


class ImageSelectionFixture:
    def __init__(self, directory, run_id):
        self.records = []
        self.configure(run_id)
        cert, key = directory / "fixture-ca.pem", directory / "fixture-key.pem"
        subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
            "-keyout", str(key), "-out", str(cert), "-subj", "/CN=127.0.0.1",
            "-addext", "subjectAltName=IP:127.0.0.1"], check=True, capture_output=True)
        self.cert = cert
        owner = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_args):
                pass

            def do_HEAD(self):
                self.respond()

            def do_GET(self):
                self.respond()

            def respond(self):
                path = urlsplit(self.path).path
                status, content, headers = 404, b"not found", {}
                if path in {"/tags/a", "/tags/b"} and self.command == "GET":
                    status = 200
                    tags = ["nightly", "v1.2.3"] if path.endswith("a") else ["v4.5.6", "latest"]
                    content = json.dumps({"name": "fixture", "tags": tags}).encode()
                    headers["Content-Type"] = "application/json"
                elif path in owner.archives:
                    if owner.failed:
                        status, content = 503, b"fixture archive temporarily unavailable"
                    else:
                        status, content = 200, owner.archives[path]
                        headers.update({"Accept-Ranges": "bytes", "Content-Type": "application/x-tar"})
                        if self.command == "GET" and self.headers.get("Range"):
                            match = re.fullmatch(r"bytes=(\d+)-(\d+)", self.headers["Range"])
                            if not match or int(match[1]) >= len(content) or int(match[1]) > int(match[2]):
                                status, content = 416, b"invalid range"
                            else:
                                start, end = int(match[1]), min(int(match[2]), len(content) - 1)
                                headers["Content-Range"] = f"bytes {start}-{end}/{len(content)}"
                                content, status = content[start:end + 1], 206
                owner.records.append({"method": self.command, "path": self.path,
                    "range": self.headers.get("Range"), "status": status, "bytes": len(content)})
                self.send_response(status)
                for name, value in headers.items():
                    self.send_header(name, value)
                self.send_header("Content-Length", str(len(content)))
                self.end_headers()
                if self.command != "HEAD":
                    self.wfile.write(content)

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(cert, key)
        self.server.socket = context.wrap_socket(self.server.socket, server_side=True)
        self.origin = f"https://127.0.0.1:{self.server.server_port}"
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def configure(self, run_id):
        # OCI repository names are lowercase; the runId timestamp carries "T"/"Z".
        scope = f"e2e-{run_id.lower()}"
        self.repo_a = f"registry.example/{scope}/alpha"
        self.repo_b = f"registry.example/{scope}/beta"
        self.refs = [self.repo_a + ":v1.2.3", self.repo_b + ":v4.5.6"]
        self.failed = False
        self.archives = {}
        for path, ref in zip(["/a.tar", "/b.tar"], self.refs):
            data = io.BytesIO()
            with tarfile.open(fileobj=data, mode="w") as archive:
                for name, content in [("config.json", b'{}'), ("layer.tar", b'fixture-layer'),
                        ("manifest.json", json.dumps([{"Config": "config.json", "RepoTags": [ref], "Layers": ["layer.tar"]}]).encode())]:
                    info = tarfile.TarInfo(name)
                    info.size, info.mtime = len(content), 0
                    archive.addfile(info, io.BytesIO(content))
            self.archives[path] = data.getvalue()

    def close(self):
        if self.thread.is_alive():
            self.server.shutdown()
            self.server.server_close()
            self.thread.join(timeout=5)
        return not self.thread.is_alive()
