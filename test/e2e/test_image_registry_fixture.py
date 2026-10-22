"""Registry HTTP fidelity and content identity tests.

E2E standard: test/e2e/browser-e2e-testing-standard.md
"""
import gzip
import hashlib
import io
import json
import socket
import tarfile
import unittest
import urllib.error
import urllib.request

from image_registry_fixture import CONFIG_TYPE, LAYER_TYPE, MANIFEST_TYPE, RegistryServer
from image_runtime_fixture import docker_archive


class RegistryFixtureTests(unittest.TestCase):
    def setUp(self):
        self.executable = b"fixture-native-executable"
        self.payload, self.image_id = docker_archive(self.executable, "old/image:tag", "e2e-run", "amd64")
        self.registry = RegistryServer(self.payload, "e2e/run")
        self.addCleanup(self.registry.stop)
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def request(self, path, method="GET", headers=None):
        request = urllib.request.Request(self.registry.url + path, method=method, headers=headers or {})
        try:
            response = self.opener.open(request, timeout=5)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            return response.status, dict(response.headers), response.read()

    # Mainline: docker-save bytes -> Registry manifest/config/layer GET -> verify runnable rootfs and all content identities.
    def test_manifest_config_and_layer_preserve_runnable_image_identity(self):
        status, headers, body = self.request("/v2/")
        self.assertEqual((status, json.loads(body)), (200, {}))
        self.assertEqual(headers["Docker-Distribution-Api-Version"], "registry/2.0")
        status, headers, body = self.request("/v2/e2e/run/manifests/runtime")
        self.assertEqual((status, headers["Content-Type"]), (200, MANIFEST_TYPE))
        self.assertEqual(headers["Docker-Content-Digest"], "sha256:" + hashlib.sha256(body).hexdigest())
        self.assertEqual(self.request("/v2/e2e/run/manifests/" + self.registry.manifest_digest)[2], body)
        manifest = json.loads(body)
        self.assertEqual(manifest["schemaVersion"], 2)
        self.assertEqual(manifest["config"]["digest"], self.image_id)
        self.assertEqual(manifest["config"]["mediaType"], CONFIG_TYPE)
        self.assertEqual(manifest["layers"][0]["mediaType"], LAYER_TYPE)
        downloaded = []
        for descriptor in [manifest["config"], *manifest["layers"]]:
            status, headers, body = self.request("/v2/e2e/run/blobs/" + descriptor["digest"])
            self.assertEqual(status, 200)
            self.assertEqual(int(headers["Content-Length"]), descriptor["size"])
            self.assertEqual(len(body), descriptor["size"])
            self.assertEqual("sha256:" + hashlib.sha256(body).hexdigest(), descriptor["digest"])
            downloaded.append(body)
        config, layer = json.loads(downloaded[0]), gzip.decompress(downloaded[1])
        self.assertEqual(config["rootfs"]["diff_ids"], ["sha256:" + hashlib.sha256(layer).hexdigest()])
        self.assertEqual(config["config"]["Entrypoint"], ["/bin/busybox", "printf", "e2e-run\\n"])
        with tarfile.open(fileobj=io.BytesIO(layer)) as rootfs:
            self.assertEqual(rootfs.extractfile("bin/busybox").read(), self.executable)
            self.assertEqual(rootfs.getmember("bin/busybox").mode, 0o755)
        duplicate = RegistryServer(self.payload, "another/image", "tag")
        try:
            self.assertEqual(duplicate.manifest_digest, self.registry.manifest_digest)
            self.assertEqual(duplicate.image_id, self.image_id)
        finally:
            duplicate.stop()

    # Mainline: known config blob -> HEAD and range reads -> exact bytes/headers; invalid ranges fail without full payload.
    def test_head_ranges_and_audit_match_bytes_served(self):
        path = "/v2/e2e/run/blobs/" + self.image_id
        blob = self.registry.blobs[self.image_id]
        status, headers, body = self.request(path, method="HEAD")
        self.assertEqual((status, int(headers["Content-Length"]), body), (200, len(blob), b""))
        for value, start, end in (("bytes=0-0", 0, 0), ("bytes=3-6", 3, 6),
                ("bytes=8-", 8, len(blob) - 1), ("bytes=-5", len(blob) - 5, len(blob) - 1),
                ("bytes=8-999999", 8, len(blob) - 1)):
            status, headers, body = self.request(path, headers={"Range": value})
            self.assertEqual((status, body), (206, blob[start:end + 1]))
            self.assertEqual(headers["Content-Range"], f"bytes {start}-{end}/{len(blob)}")
        for value in (f"bytes={len(blob)}-", "bytes=8-2", "bytes=no", "bytes=-0", "bytes=0-1,3-4"):
            status, headers, body = self.request(path, headers={"Range": value})
            self.assertEqual((status, body, headers["Content-Range"]), (416, b"", f"bytes */{len(blob)}"))
        self.doCleanups()
        audit = self.registry.snapshot()
        self.assertEqual(len(audit), 11)
        self.assertEqual(audit[0]["bytes_sent"], 0)
        self.assertTrue(all(item["bytes_sent"] == item["content_length"] for item in audit[1:]))
        self.assertTrue(all(not item["disconnected"] for item in audit))

    # Mainline: unknown repository/tag/digest -> real HTTP refusal -> Registry error envelope, never supplied image bytes.
    def test_unknown_resources_fail_closed(self):
        for path, code in (
            ("/v2/other/run/manifests/runtime", "NAME_UNKNOWN"),
            ("/v2/e2e/run/manifests/missing", "MANIFEST_UNKNOWN"),
            ("/v2/e2e/run/manifests/sha256:" + "0" * 64, "MANIFEST_UNKNOWN"),
            ("/v2/e2e/run/blobs/sha256:" + "0" * 64, "BLOB_UNKNOWN"),
            ("/v2/e2e/run/blobs/invalid", "BLOB_UNKNOWN"),
        ):
            status, headers, body = self.request(path)
            self.assertEqual(status, 404)
            self.assertEqual(json.loads(body)["errors"][0]["code"], code)
            self.assertNotIn("Docker-Content-Digest", headers)
            self.assertEqual(self.request(path, method="HEAD")[0], 404)

    # Mainline: invalid protocol and unsupported method -> refused before GET handler -> accepted connections remain observable.
    def test_connection_audit_includes_requests_outside_get_and_head(self):
        self.assertEqual(self.registry.connection_count, 0)
        address = ("127.0.0.1", self.registry.server.server_port)
        with socket.create_connection(address, timeout=5) as client:
            client.sendall(b"\x16\x03\x01\x00\x00\r\n")
            self.assertIn(b"400", client.recv(8192))
        self.assertEqual(self.registry.connection_count, 1)
        self.assertEqual(self.registry.snapshot(), [])
        self.assertEqual(self.request("/v2/", method="POST")[0], 501)
        self.assertEqual(self.registry.connection_count, 2)
        self.assertEqual(self.registry.snapshot(), [])
        self.doCleanups()
        self.assertFalse(self.registry.thread.is_alive())
        with self.assertRaises(ConnectionRefusedError):
            socket.create_connection(address, timeout=5)

    # Mainline: malformed fixture identity/config -> constructor refuses before opening any server socket.
    def test_rejects_unsafe_names_and_corrupt_rootfs(self):
        for repository in ("../image", "Upper/image", "image:tag", "image//child"):
            with self.assertRaises(ValueError):
                RegistryServer(self.payload, repository)
        with self.assertRaises(ValueError):
            RegistryServer(self.payload, "image", "../tag")
        corrupted = io.BytesIO()
        with tarfile.open(fileobj=io.BytesIO(self.payload)) as archive, tarfile.open(fileobj=corrupted, mode="w") as out:
            for entry in archive:
                data = archive.extractfile(entry).read()
                if entry.name.endswith("layer.tar"):
                    data += b"modified"
                    entry.size = len(data)
                out.addfile(entry, io.BytesIO(data))
        with self.assertRaisesRegex(ValueError, "diff_ids"):
            RegistryServer(corrupted.getvalue(), "image")


if __name__ == "__main__":
    unittest.main()
