"""Archive wire fidelity and isolated runtime prerequisite tests.

E2E standard: test/e2e/browser-e2e-testing-standard.md
"""
import hashlib
import io
import json
import tarfile
import unittest
from unittest.mock import patch
from pathlib import Path
import tempfile
import urllib.error
import urllib.request

from image_runtime_fixture import ArchiveServer, ImageRuntimeFixture, docker_archive


class ImageRuntimeFixtureTests(unittest.TestCase):
    def test_daemon_preflight_rejects_shared_or_unverifiable_containerd(self):
        with tempfile.TemporaryDirectory(prefix="gw-e2e-image-unit-") as directory:
            fixture = ImageRuntimeFixture(Path(directory), "containerd-isolation")
            fixture.runtime = Path(directory)
            info = {"DockerRootDir": str(fixture.runtime / "data"), "Images": 0, "Containers": 0,
                "ID": "owned", "Driver": "vfs", "ServerVersion": "fixture"}
            for address in (None, "/run/containerd/containerd.sock", "/var/run/containerd/containerd.sock",
                    str(fixture.runtime / "exec-other/containerd/containerd.sock"),
                    str(fixture.runtime / "exec/containerd/../containerd.sock")):
                with self.subTest(address=address):
                    candidate = {**info, "Containerd": {"Address": address}}
                    with self.assertRaisesRegex(RuntimeError, "owned containerd"):
                        fixture._validate_daemon_info(candidate)
            address = str(fixture.runtime / "exec/containerd/containerd.sock")
            result = fixture._validate_daemon_info({**info, "Containerd": {"Address": address}})
            self.assertEqual(result["ContainerdAddress"], address)
            self.assertEqual(result["DockerRootDir"], info["DockerRootDir"])
            for bad in ({"DockerRootDir": "/var/lib/docker"}, {"Images": 1}, {"Containers": 1}):
                with self.subTest(bad=bad), self.assertRaisesRegex(RuntimeError, "empty owned fixture"):
                    fixture._validate_daemon_info({**info, "Containerd": {"Address": address}, **bad})

    def test_live_owned_containerd_prevents_runtime_deletion(self):
        with tempfile.TemporaryDirectory(prefix="gw-e2e-image-unit-") as directory:
            fixture = ImageRuntimeFixture(Path(directory), "containerd-retention")
            fixture.runtime = Path(directory)
            fixture.containerd_pid, fixture.containerd_start = 123, "owned-start"
            evidence = fixture.runtime / "runtime-evidence"
            evidence.write_text("retain while child is alive")
            with patch.object(fixture, "_process_start", return_value="owned-start"), \
                    patch("image_runtime_fixture.time.monotonic", side_effect=[0, 11]), \
                    patch("image_runtime_fixture.subprocess.run", side_effect=AssertionError("must not remove live runtime")):
                with self.assertRaisesRegex(RuntimeError, "containerd did not terminate"):
                    fixture.stop()
            self.assertTrue(evidence.exists())
            with patch.object(fixture, "_process_start", return_value="different-start"):
                self.assertTrue(fixture._wait_containerd_exit())
            with patch.object(fixture, "_process_start", side_effect=FileNotFoundError):
                self.assertTrue(fixture._wait_containerd_exit())

    def test_partial_download_barrier_observes_client_disconnect_and_recovers(self):
        payload = b"x" * 131072
        server = ArchiveServer(payload)
        server.start()
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        try:
            server.hold_partial_downloads()
            response = opener.open(urllib.request.Request(server.url,
                headers={"Range": "bytes=0-131071"}), timeout=5)
            self.assertEqual(response.read(32768), payload[:32768])
            server.wait_barrier(count=1)
            response.close()
            server.wait_barrier(disconnected=True, count=1)
            cancelled = server.snapshot()
            self.assertEqual(len(cancelled), 1)
            self.assertEqual(cancelled[0]["bytes_sent"], 32768)
            self.assertTrue(cancelled[0]["disconnected"])
            server.release_downloads()
            with opener.open(server.url, timeout=5) as recovered:
                self.assertEqual(recovered.read(), payload)
        finally:
            server.stop()

    def test_failed_run_preserves_owned_runtime_and_records_scoped_reclamation(self):
        with tempfile.TemporaryDirectory(prefix="gw-e2e-image-unit-") as directory:
            fixture = ImageRuntimeFixture(Path(directory), "preservation")
            fixture.runtime = Path(directory)
            fixture.node_uuid = "owned-node"
            evidence = fixture.runtime / "failure-evidence"
            evidence.write_text("retained")
            with patch("image_runtime_fixture.subprocess.run", side_effect=AssertionError("must not remove failed state")):
                result = fixture.stop(preserve_runtime=True)
            self.assertEqual(evidence.read_text(), "retained")
            self.assertTrue(result["agent_stopped"] and result["daemon_stopped"])
            self.assertTrue(result["runtime_directory_retained"])
            self.assertTrue(result["node_retained"])
            self.assertFalse(result["node_removed"])
            self.assertFalse(result["runtime_directory_removed"])
            self.assertEqual(result["runtime_directory"], directory)
            self.assertEqual(result["cleanup_command"], "sudo -n rm -rf -- " + directory)
            self.assertEqual(result["retention_days"], 7)

    def test_archive_declares_exact_runnable_payload_and_content_identity(self):
        executable = b"fixture-native-executable"
        payload, image_id = docker_archive(executable, "e2e/example:runtime", "e2e-run", "amd64")
        with tarfile.open(fileobj=io.BytesIO(payload)) as archive:
            manifest = json.load(archive.extractfile("manifest.json"))[0]
            self.assertEqual(manifest["RepoTags"], ["e2e/example:runtime"])
            config_bytes = archive.extractfile(manifest["Config"]).read()
            self.assertEqual(image_id, "sha256:" + hashlib.sha256(config_bytes).hexdigest())
            config = json.loads(config_bytes)
            self.assertEqual(config["config"]["Entrypoint"], ["/bin/busybox", "printf", "e2e-run\\n"])
            layer = archive.extractfile(manifest["Layers"][0]).read()
            self.assertEqual(config["rootfs"]["diff_ids"], ["sha256:" + hashlib.sha256(layer).hexdigest()])
            with tarfile.open(fileobj=io.BytesIO(layer)) as rootfs:
                self.assertEqual(rootfs.extractfile("bin/busybox").read(), executable)
                self.assertEqual(rootfs.getmember("bin/busybox").mode, 0o755)
        _, changed = docker_archive(executable, "e2e/example:runtime", "e2e-other", "amd64")
        self.assertNotEqual(changed, image_id)

    def test_http_head_ranges_and_failures_preserve_downloader_contract(self):
        server = ArchiveServer(b"0123456789")
        server.start()
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

        def request(method="GET", path="", range_header=None):
            req = urllib.request.Request(server.url + path, method=method,
                headers={"Range": range_header} if range_header else {})
            try:
                response = opener.open(req, timeout=5)
            except urllib.error.HTTPError as error:
                response = error
            with response:
                return response.status, dict(response.headers), response.read()

        try:
            status, headers, body = request("HEAD")
            self.assertEqual((status, headers["Content-Length"], body), (200, "10", b""))
            for byte_range, expected, content_range in (
                ("bytes=0-0", b"0", "bytes 0-0/10"),
                ("bytes=3-6", b"3456", "bytes 3-6/10"),
                ("bytes=8-", b"89", "bytes 8-9/10"),
                ("bytes=8-50", b"89", "bytes 8-9/10"),
            ):
                status, headers, body = request(range_header=byte_range)
                self.assertEqual((status, body, headers["Content-Range"]), (206, expected, content_range))
            for byte_range in ("bytes=10-", "bytes=8-2", "bytes=no", "bytes=0-1,3-4"):
                status, headers, _ = request(range_header=byte_range)
                self.assertEqual((status, headers["Content-Range"]), (416, "bytes */10"))
            self.assertEqual(request(path="/unknown")[0], 404)
            server.failure_status = 503
            self.assertEqual(request()[0], 503)
            self.assertEqual(request("HEAD")[0], 200)
            server.failure_status = None
            self.assertEqual(request()[2], b"0123456789")
        finally:
            server.stop()
        audit = server.snapshot()
        self.assertTrue(any(item["status"] == 503 for item in audit))
        self.assertTrue(any(item["range"] == "bytes=3-6" and item["bytes_sent"] == 4 for item in audit))
        self.assertTrue(all(item["bytes_sent"] == 0 for item in audit if item["method"] == "HEAD"))


if __name__ == "__main__":
    unittest.main()
