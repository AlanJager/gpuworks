"""Auditable, read-only Docker Registry v2 for an owned docker-save image.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Only the supplied repository, tag and content digests exist. No upstream proxy,
authentication, upload or tag discovery is provided by this fixture.
"""
from __future__ import annotations

import gzip
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import io
import json
import re
import tarfile
import threading
from urllib.parse import urlsplit


MANIFEST_TYPE = "application/vnd.docker.distribution.manifest.v2+json"
CONFIG_TYPE = "application/vnd.docker.container.image.v1+json"
LAYER_TYPE = "application/vnd.docker.image.rootfs.diff.tar.gzip"


def digest(data):
    return "sha256:" + hashlib.sha256(data).hexdigest()


class RegistryServer:
    """Serve the actual config/layers from docker_archive without changing identity."""
    def __init__(self, payload: bytes, repository: str, tag: str = "runtime"):
        if not re.fullmatch(r"[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*", repository):
            raise ValueError("fixture repository must be a lowercase Docker repository name")
        if not re.fullmatch(r"[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}", tag):
            raise ValueError("invalid Docker tag")
        self.repository, self.tag = repository, tag
        with tarfile.open(fileobj=io.BytesIO(payload)) as archive:
            entries = json.load(archive.extractfile("manifest.json"))
            if len(entries) != 1:
                raise ValueError("Registry fixture requires exactly one archived image")
            entry = entries[0]
            config = archive.extractfile(entry["Config"]).read()
            layers = [archive.extractfile(name).read() for name in entry["Layers"]]
        rootfs = json.loads(config).get("rootfs", {})
        if rootfs.get("type") != "layers" or rootfs.get("diff_ids") != [digest(layer) for layer in layers]:
            raise ValueError("archive rootfs diff_ids do not match the actual layers")
        self.image_id = digest(config)
        self.blobs = {self.image_id: config}
        descriptors = []
        for layer in layers:
            compressed = gzip.compress(layer, mtime=0)
            layer_digest = digest(compressed)
            self.blobs[layer_digest] = compressed
            descriptors.append({"mediaType": LAYER_TYPE, "size": len(compressed), "digest": layer_digest})
        self.manifest = {"schemaVersion": 2, "mediaType": MANIFEST_TYPE,
            "config": {"mediaType": CONFIG_TYPE, "size": len(config), "digest": self.image_id},
            "layers": descriptors}
        self.manifest_bytes = json.dumps(self.manifest, sort_keys=True, separators=(",", ":")).encode()
        self.manifest_digest = digest(self.manifest_bytes)
        self.requests = []
        self._connection_count = 0
        self.lock = threading.Lock()
        fixture = self

        class Server(ThreadingHTTPServer):
            def get_request(self):
                connection, address = super().get_request()
                # Count before HTTP parsing: Docker may probe HTTPS first, and
                # a failed TLS handshake must not look like an unused origin.
                with fixture.lock:
                    fixture._connection_count += 1
                return connection, address

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_args):
                pass

            def do_HEAD(self):
                self.respond(False)

            def do_GET(self):
                self.respond(True)

            def respond(self, send_body):
                path = urlsplit(self.path).path
                status, content_type, content_digest = 200, "application/json", ""
                body, error_code = b"{}", ""
                prefix = "/v2/" + fixture.repository + "/"
                is_blob = False
                if path == "/v2/":
                    pass
                elif path.startswith(prefix + "manifests/"):
                    reference = path[len(prefix + "manifests/"):]
                    if reference in (fixture.tag, fixture.manifest_digest):
                        body, content_type = fixture.manifest_bytes, MANIFEST_TYPE
                        content_digest = fixture.manifest_digest
                    else:
                        error_code = "MANIFEST_UNKNOWN"
                elif path.startswith(prefix + "blobs/"):
                    content_digest = path[len(prefix + "blobs/"):]
                    if content_digest in fixture.blobs:
                        body, content_type = fixture.blobs[content_digest], "application/octet-stream"
                        is_blob = True
                    else:
                        content_digest, error_code = "", "BLOB_UNKNOWN"
                else:
                    error_code = "NAME_UNKNOWN"
                if error_code:
                    status = 404
                    body = json.dumps({"errors": [{"code": error_code,
                        "message": "requested fixture content does not exist"}]}).encode()
                range_header, content_range = self.headers.get("Range", ""), ""
                total = len(body)
                if is_blob and send_body and range_header:
                    match = re.fullmatch(r"bytes=(\d*)-(\d*)", range_header)
                    if match and (match[1] or match[2]):
                        start = int(match[1]) if match[1] else max(0, total - int(match[2]))
                        end = min(int(match[2]), total - 1) if match[1] and match[2] else total - 1
                    else:
                        start, end = total, -1
                    if 0 <= start <= end < total:
                        status, body = 206, body[start:end + 1]
                        content_range = f"bytes {start}-{end}/{total}"
                    else:
                        status, body = 416, b""
                        content_range = f"bytes */{total}"
                self.send_response(status)
                self.send_header("Docker-Distribution-Api-Version", "registry/2.0")
                self.send_header("Content-Type", content_type)
                self.send_header("Content-Length", str(len(body)))
                if content_digest:
                    self.send_header("Docker-Content-Digest", content_digest)
                    self.send_header("ETag", '"' + content_digest + '"')
                if is_blob:
                    self.send_header("Accept-Ranges", "bytes")
                if content_range:
                    self.send_header("Content-Range", content_range)
                self.end_headers()
                written, disconnected = 0, False
                try:
                    if send_body:
                        for offset in range(0, len(body), 65536):
                            chunk = body[offset:offset + 65536]
                            self.wfile.write(chunk)
                            written += len(chunk)
                        self.wfile.flush()
                except (BrokenPipeError, ConnectionResetError):
                    disconnected = True
                finally:
                    with fixture.lock:
                        fixture.requests.append({"method": self.command, "path": self.path,
                            "range": range_header, "status": status, "digest": content_digest,
                            "content_length": len(body), "bytes_sent": written,
                            "disconnected": disconnected})

        self.server = Server(("127.0.0.1", 0), Handler)
        self.origin = f"127.0.0.1:{self.server.server_port}"
        self.url = "http://" + self.origin
        self.image_ref = f"{self.origin}/{repository}:{tag}"
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def snapshot(self):
        with self.lock:
            return [dict(item) for item in self.requests]

    @property
    def connection_count(self):
        with self.lock:
            return self._connection_count

    def stop(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)
        if self.thread.is_alive():
            raise RuntimeError("owned Registry listener did not stop")
