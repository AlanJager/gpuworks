"""External fixture protocol checks; not browser business coverage.

E2E standard: test/e2e/browser-e2e-testing-standard.md
"""
import io
import json
import re
from pathlib import Path
import ssl
import tarfile
import tempfile
import unittest
import urllib.error
import urllib.request

from image_selection_fixture import ImageSelectionFixture


class ImageSelectionFixtureTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.fixture = ImageSelectionFixture(Path(self.directory.name), "unit")
        self.client = urllib.request.build_opener(urllib.request.ProxyHandler({}),
            urllib.request.HTTPSHandler(context=ssl.create_default_context(cafile=str(self.fixture.cert))))

    def tearDown(self):
        self.assertTrue(self.fixture.close())
        self.directory.cleanup()

    def request(self, path, method="GET", headers=None):
        request = urllib.request.Request(self.fixture.origin + path, method=method, headers=headers or {})
        try:
            return self.client.open(request, timeout=5)
        except urllib.error.HTTPError as error:
            return error

    # Mainline: a real runId (uppercase T/Z) → repository names that satisfy the OCI path grammar
    # the server enforces (internal/server/imagecatalog/community/browse.go repositoryPath).
    def test_repositories_follow_the_oci_name_grammar(self):
        grammar = re.compile(r"^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$")
        self.fixture.configure("20261021T010203Z-abcdef12")
        for repo in (self.fixture.repo_a, self.fixture.repo_b):
            path = repo.split("/", 1)[1]
            self.assertRegex(path, grammar)
            self.assertIn("20261021t010203z-abcdef12", path)

    def test_tag_lists_are_distinct_and_unknown_routes_fail_closed(self):
        with self.request("/tags/a") as response:
            self.assertEqual(json.load(response)["tags"], ["nightly", "v1.2.3"])
        with self.request("/tags/b") as response:
            self.assertEqual(json.load(response)["tags"], ["v4.5.6", "latest"])
        with self.request("/api/v1/images") as response:
            self.assertEqual(response.status, 404)

    def test_archive_head_range_and_manifest_share_same_bytes(self):
        content = self.fixture.archives["/a.tar"]
        with self.request("/a.tar", "HEAD") as response:
            self.assertEqual(int(response.headers["Content-Length"]), len(content))
            self.assertEqual(response.read(), b"")
        with self.request("/a.tar?download=1", headers={"Range": "bytes=512-1023"}) as response:
            self.assertEqual(response.status, 206)
            self.assertEqual(response.headers["Content-Range"], f"bytes 512-1023/{len(content)}")
            self.assertEqual(response.read(), content[512:1024])
        with tarfile.open(fileobj=io.BytesIO(content)) as archive:
            self.assertEqual(json.load(archive.extractfile("manifest.json"))[0]["RepoTags"], [self.fixture.refs[0]])
        with self.request("/a.tar", headers={"Range": "bytes=999999-9999999"}) as response:
            self.assertEqual(response.status, 416)

    def test_controlled_failure_recovers_and_audit_records_both(self):
        self.fixture.failed = True
        with self.request("/b.tar", "HEAD") as response:
            self.assertEqual(response.status, 503)
        self.fixture.failed = False
        with self.request("/b.tar", "HEAD") as response:
            self.assertEqual(response.status, 200)
        self.assertEqual([row["status"] for row in self.fixture.records], [503, 200])


if __name__ == "__main__":
    unittest.main()
