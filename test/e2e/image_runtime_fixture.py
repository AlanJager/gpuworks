"""Owned CPU Docker + real agent + audited HTTP archive for browser image cases.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Requires Linux, sudo -n, unshare, Docker and a native static busybox binary.
The fixture never contacts the default Docker socket or any existing node.
"""
from __future__ import annotations

import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import io
import json
import os
from pathlib import Path
import platform
import re
import select
import socket
import shutil
import shlex
import subprocess
import tarfile
import tempfile
import threading
import time

from local_browser_fixture import http


def docker_archive(binary: bytes, image_ref: str, marker: str, arch: str):
    """Create an offline docker-save image whose executable prints its marker."""
    if not re.fullmatch(r"[a-zA-Z0-9_-]+", marker):
        raise ValueError("marker must be safe literal text")

    def tar(entries):
        stream = io.BytesIO()
        with tarfile.open(fileobj=stream, mode="w") as archive:
            for name, data, mode in entries:
                entry = tarfile.TarInfo(name)
                entry.size, entry.mode, entry.mtime = len(data), mode, 0
                archive.addfile(entry, io.BytesIO(data))
        return stream.getvalue()

    layer = tar([("bin/busybox", binary, 0o755)])
    layer_sha = hashlib.sha256(layer).hexdigest()
    config = json.dumps({"architecture": arch, "os": "linux",
        "config": {"Entrypoint": ["/bin/busybox", "printf", marker + "\\n"]},
        "rootfs": {"type": "layers", "diff_ids": ["sha256:" + layer_sha]},
        "history": [{"created_by": "GPUWorks owned CPU E2E fixture"}]},
        sort_keys=True, separators=(",", ":")).encode()
    config_sha = hashlib.sha256(config).hexdigest()
    manifest = json.dumps([{"Config": config_sha + ".json", "RepoTags": [image_ref],
        "Layers": [layer_sha + "/layer.tar"]}]).encode()
    payload = tar([("manifest.json", manifest, 0o644),
        (config_sha + ".json", config, 0o644), (layer_sha + "/layer.tar", layer, 0o644)])
    return payload, "sha256:" + config_sha


class ArchiveServer:
    """Real range-capable HTTP server recording responses actually sent."""
    def __init__(self, payload: bytes):
        self.payload = payload
        self.requests = []
        self.lock = threading.Lock()
        self.failure_status = None
        self.partial_barrier = False
        self.release = threading.Event()
        self.barrier_condition = threading.Condition()
        self.blocked = 0
        self.disconnected = 0
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_args):
                pass

            def do_HEAD(self):
                self.respond(False)

            def do_GET(self):
                self.respond(True)

            def respond(self, send_body):
                range_header = self.headers.get("Range", "")
                status, start, end = 200, 0, len(fixture.payload) - 1
                if self.path != "/image.tar":
                    status = 404
                elif send_body and fixture.failure_status:
                    status = fixture.failure_status
                elif range_header:
                    match = re.fullmatch(r"bytes=(\d+)-(\d*)", range_header)
                    if not match:
                        status = 416
                    else:
                        start = int(match[1])
                        end = min(int(match[2]) if match[2] else end, end)
                        status = 206 if 0 <= start <= end else 416
                body = fixture.payload[start:end + 1] if status in (200, 206) else b"fixture unavailable\n"
                self.send_response(status)
                self.send_header("Content-Type", "application/x-tar")
                self.send_header("Content-Length", str(len(body)))
                self.send_header("Accept-Ranges", "bytes")
                self.send_header("ETag", '"' + hashlib.sha256(fixture.payload).hexdigest() + '"')
                if status == 206:
                    self.send_header("Content-Range", f"bytes {start}-{end}/{len(fixture.payload)}")
                if status == 416:
                    self.send_header("Content-Range", f"bytes */{len(fixture.payload)}")
                self.end_headers()
                written = 0
                disconnected = False
                try:
                    if send_body:
                        if fixture.partial_barrier and status == 206 and len(body) > 32768:
                            self.wfile.write(body[:32768])
                            self.wfile.flush()
                            written = 32768
                            with fixture.barrier_condition:
                                fixture.blocked += 1
                                fixture.barrier_condition.notify_all()
                            # Wait on actual client disconnect or explicit fixture
                            # release. Never advance based on an elapsed sleep.
                            while not fixture.release.is_set():
                                readable, _, _ = select.select([self.connection], [], [], .1)
                                if readable and self.connection.recv(1, socket.MSG_PEEK) == b"":
                                    disconnected = True
                                    break
                            if not disconnected:
                                self.wfile.write(body[written:])
                                self.wfile.flush()
                                written = len(body)
                        else:
                            self.wfile.write(body)
                            self.wfile.flush()
                            written = len(body)
                except (BrokenPipeError, ConnectionResetError):
                    disconnected = True
                finally:
                    with fixture.lock:
                        fixture.requests.append({"method": self.command, "path": self.path,
                            "range": range_header, "status": status, "bytes_sent": written,
                            "disconnected": disconnected})
                    if disconnected:
                        with fixture.barrier_condition:
                            fixture.disconnected += 1
                            fixture.barrier_condition.notify_all()

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.url = f"http://127.0.0.1:{self.server.server_port}/image.tar"

    def start(self):
        self.thread.start()

    def snapshot(self):
        with self.lock:
            return [dict(item) for item in self.requests]

    def hold_partial_downloads(self):
        self.partial_barrier = True
        self.release.clear()

    def release_downloads(self):
        self.partial_barrier = False
        self.release.set()

    def wait_barrier(self, *, disconnected=False, count=4, timeout=15):
        attribute = "disconnected" if disconnected else "blocked"
        with self.barrier_condition:
            if not self.barrier_condition.wait_for(lambda: getattr(self, attribute) >= count, timeout):
                raise RuntimeError(f"archive {attribute} connections did not reach {count}")
            return getattr(self, attribute)

    def stop(self):
        self.release_downloads()
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)


class ImageRuntimeFixture:
    def __init__(self, work: Path, run_id: str):
        if not re.fullmatch(r"[a-zA-Z0-9_-]+", run_id):
            raise ValueError("unsafe runId")
        self.work = work.resolve()
        self.run_id = run_id
        self.image_ref = f"gpuworks-e2e/{run_id.lower()}:runtime"
        self.marker = f"gpuworks-e2e-{run_id}-executed"
        self.node_name = f"e2e-{run_id}-cpu"
        self.node_uuid = ""
        self.daemon = self.agent = self.archive = None
        self.daemon_log = self.agent_log = None
        self.runtime = None
        self.base = self.admin_token = ""
        self.arch = {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine())
        if not self.arch:
            raise RuntimeError("native CPU image architecture is unsupported")

    def start(self):
        if os.getuid() == 0:
            raise RuntimeError("run fixture as ordinary user; root agent could stage into shared Docker data")
        # Agent's docker archive implementation first tries this fixed path.
        # Existing directories must also be unreachable, not merely unwritable.
        shared = Path("/var/lib/docker/gpuworks-image-archives")
        if os.access(shared, os.F_OK) or os.access(shared.parent, os.W_OK):
            raise RuntimeError("agent shared Docker archive path is accessible; isolated staging cannot be guaranteed")
        for command in ("sudo", "unshare", "mount", "sh", "dockerd", "docker", "busybox", "file"):
            if not shutil.which(command):
                raise RuntimeError(f"required fixture command is missing: {command}")
        subprocess.run(["sudo", "-n", "true"], check=True, capture_output=True)
        busybox = Path(shutil.which("busybox"))
        description = subprocess.run(["file", str(busybox)], check=True, capture_output=True, text=True).stdout
        if "statically linked" not in description:
            raise RuntimeError("fixture busybox must be a native static executable")
        payload, self.image_id = docker_archive(busybox.read_bytes(), self.image_ref, self.marker, self.arch)
        self.archive_sha256 = hashlib.sha256(payload).hexdigest()
        self.archive_size = len(payload)
        self.runtime = Path(tempfile.mkdtemp(prefix="gw-e2e-image-"))
        self.env = {key: value for key, value in os.environ.items()
            if not key.startswith(("DOCKER_", "GPUWORKS_"))}
        self.env["DOCKER_HOST"] = "unix://" + str(self.runtime / "docker.sock")
        config = self.runtime / "daemon.json"
        # Only this owned daemon trusts the loopback HTTP Registry fixture.
        config.write_text(json.dumps({"features": {"containerd-snapshotter": False},
            "insecure-registries": ["127.0.0.1/32"]}))
        # Dockerd otherwise auto-attaches a host containerd even with a private
        # Docker data/exec root. Hide only its conventional discovery directory
        # inside this already-private mount namespace. Never create a host path.
        masked_containerd = self.runtime / "masked-containerd"
        masked_containerd.mkdir()
        self.daemon_log = (self.work / "docker.log").open("w")
        self.daemon = subprocess.Popen(["sudo", "-n", "unshare", "--mount", "--propagation", "private",
            "sh", "-eu", "-c", 'if [ -d /run/containerd ]; then mount --bind "$1" /run/containerd; fi; shift; exec "$@"',
            "gpuworks-owned-daemon", str(masked_containerd), "dockerd", "--config-file=" + str(config), "--data-root=" + str(self.runtime / "data"),
            "--exec-root=" + str(self.runtime / "exec"), "--pidfile=" + str(self.runtime / "daemon.pid"),
            "--host=" + self.env["DOCKER_HOST"], "--bridge=none", "--iptables=false", "--ip6tables=false",
            "--ip-forward=false", "--ip-masq=false", "--userland-proxy=false", "--storage-driver=vfs"],
            stdout=self.daemon_log, stderr=subprocess.STDOUT, env=self.env)
        deadline = time.monotonic() + 40
        while time.monotonic() < deadline:
            if self.daemon.poll() is not None:
                raise RuntimeError("isolated dockerd exited; inspect private/docker.log")
            result = self.docker(["info", "--format", "{{json .}}"], check=False)
            if result.returncode == 0:
                info = json.loads(result.stdout)
                self.daemon_identity = self._validate_daemon_info(info)
                break
            threading.Event().wait(.1)
        else:
            raise RuntimeError("isolated dockerd readiness timed out")
        self.daemon_pid = int((self.runtime / "daemon.pid").read_text().strip())
        self.daemon_start = self._process_start(self.daemon_pid)
        containerd_pid = int(subprocess.run(["sudo", "-n", "cat",
            str(self.runtime / "exec/containerd/containerd.pid")], check=True, capture_output=True, text=True).stdout.strip())
        command = Path(f"/proc/{containerd_pid}/cmdline").read_bytes().split(b"\0")
        if str(self.runtime / "exec/containerd/containerd.toml").encode() not in command:
            raise RuntimeError("managed containerd command does not use the owned configuration")
        self.containerd_pid = containerd_pid
        self.containerd_start = self._process_start(containerd_pid)
        self.daemon_identity["ContainerdPID"] = containerd_pid
        self.archive = ArchiveServer(payload)
        self.archive.start()
        self.archive_url = self.archive.url
        return self.daemon_identity

    def _validate_daemon_info(self, info):
        if info["DockerRootDir"] != str(self.runtime / "data") or info["Images"] or info["Containers"]:
            raise RuntimeError("Docker is not the empty owned fixture daemon")
        address = (info.get("Containerd") or {}).get("Address")
        if address != str(self.runtime / "exec/containerd/containerd.sock"):
            raise RuntimeError("Docker must expose an owned containerd at the exact fixture exec path; "
                "shared or missing DockerInfo Containerd.Address is not supported")
        return {**{key: info[key] for key in ("ID", "DockerRootDir", "Driver", "ServerVersion")},
            "ContainerdAddress": address}

    @staticmethod
    def _process_start(pid):
        return Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()[19]

    def _wait_containerd_exit(self):
        if not hasattr(self, "containerd_pid"):
            return True
        deadline = time.monotonic() + 10
        while True:
            try:
                if self._process_start(self.containerd_pid) != self.containerd_start:
                    return True  # The original process exited; never signal a reused PID.
            except FileNotFoundError:
                return True
            if time.monotonic() >= deadline:
                return False
            threading.Event().wait(.1)

    def docker(self, args, *, check=True, timeout=30):
        if not self.runtime or self.env["DOCKER_HOST"] != "unix://" + str(self.runtime / "docker.sock"):
            raise RuntimeError("owned Docker endpoint is not established")
        return subprocess.run(["docker", "--host=" + self.env["DOCKER_HOST"], *args],
            env=self.env, capture_output=True, text=True, check=check, timeout=timeout)

    def start_agent(self, binary: Path, server_base: str, admin_token: str, remember):
        self.base, self.admin_token = server_base, admin_token
        status, response = http(server_base, "POST", "/api/v1/nodes", admin_token,
            {"name": self.node_name, "server_url": server_base})
        if status != 201 or not response.get("token") or not response.get("node", {}).get("uuid"):
            raise RuntimeError(f"isolated node enrollment failed: HTTP {status}")
        self.node_uuid = response["node"]["uuid"]
        remember(response["token"])
        self.agent_state = self.work / "agent-state"
        self.agent_log = (self.work / "agent.log").open("w")
        self.agent = subprocess.Popen([str(binary.resolve()), "run", "--server", server_base,
            "--driver", "docker", "--state", str(self.agent_state)],
            env={**self.env, "GPUWORKS_TOKEN": response["token"]},
            stdout=self.agent_log, stderr=subprocess.STDOUT)
        deadline = time.monotonic() + 45
        while time.monotonic() < deadline:
            if self.agent.poll() is not None:
                raise RuntimeError("owned agent exited; inspect private/agent.log")
            code, node = http(server_base, "GET", f"/api/v1/nodes/{self.node_uuid}", admin_token)
            if code == 200 and node.get("state") == "online" and node.get("runtime_driver") == "docker":
                return node
            threading.Event().wait(.1)
        raise RuntimeError("owned CPU agent did not become online with Docker")

    def stop(self, preserve_runtime=False):
        failures = []
        if self.agent and self.agent.poll() is None:
            self.agent.terminate()
            try:
                self.agent.wait(timeout=15)
            except subprocess.TimeoutExpired:
                self.agent.kill()
                self.agent.wait(timeout=5)
        if self.agent_log:
            self.agent_log.close()
        if self.node_uuid and not preserve_runtime:
            try:
                # The owned process is already stopped above; this fixture has no SSH host.
                status, _ = http(self.base, "DELETE", f"/api/v1/nodes/{self.node_uuid}?force=true", self.admin_token)
                code, _ = http(self.base, "GET", f"/api/v1/nodes/{self.node_uuid}", self.admin_token)
                if status not in (200, 204) or code != 404:
                    failures.append(f"owned node cleanup returned {status}/{code}")
            except Exception as error:
                # API unavailability must not leave an owned privileged daemon
                # running. Preserve the cleanup failure after process teardown.
                failures.append(f"owned node cleanup failed: {type(error).__name__}")
        if self.archive:
            self.archive.stop()
        if self.daemon and self.daemon.poll() is None:
            pidfile = self.runtime / "daemon.pid"
            if not pidfile.exists():
                failures.append("running daemon has no ownership pidfile")
            else:
                pid = int(pidfile.read_text().strip())
                command = Path(f"/proc/{pid}/cmdline").read_bytes().split(b"\0")
                expected_root = ("--data-root=" + str(self.runtime / "data")).encode()
                if expected_root not in command:
                    raise RuntimeError("daemon command ownership changed; refusing signal or cleanup")
                if hasattr(self, "daemon_pid") and (pid != self.daemon_pid or self._process_start(pid) != self.daemon_start):
                    raise RuntimeError("daemon ownership changed; refusing signal or cleanup")
                subprocess.run(["sudo", "-n", "kill", "-TERM", str(pid)], check=True, capture_output=True)
                try:
                    self.daemon.wait(timeout=30)
                except subprocess.TimeoutExpired:
                    failures.append("owned daemon did not terminate")
        containerd_stopped = self._wait_containerd_exit()
        if not containerd_stopped:
            failures.append("owned managed containerd did not terminate; runtime state retained")
        if self.daemon_log:
            self.daemon_log.close()
        if self.runtime and not preserve_runtime and not failures and (not self.daemon or self.daemon.poll() is not None):
            subprocess.run(["sudo", "-n", "rm", "-rf", "--", str(self.runtime)], check=True, capture_output=True)
        result = {"agent_stopped": self.agent is None or self.agent.poll() is not None,
            "daemon_stopped": self.daemon is None or self.daemon.poll() is not None,
            "containerd_stopped": containerd_stopped,
            "runtime_directory_removed": self.runtime is None or not self.runtime.exists(),
            "node_removed": not self.node_uuid or (not preserve_runtime and not failures),
            "node_retained": bool(self.node_uuid) and preserve_runtime, "errors": failures}
        if self.runtime and self.runtime.exists():
            result.update(runtime_directory_retained=True, runtime_directory=str(self.runtime),
                retention_days=7, cleanup_owner="test runner operator",
                cleanup_command="sudo -n rm -rf -- " + shlex.quote(str(self.runtime)))
        if failures:
            raise RuntimeError("; ".join(failures))
        return result
