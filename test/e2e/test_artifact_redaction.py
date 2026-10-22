"""Escaped-form artifact redaction and scan failure-path tests.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: PYTHONPATH=test/e2e python3 -m unittest test_artifact_redaction -v
"""
import json
import tempfile
import unittest
import urllib.parse
import zipfile
from pathlib import Path

from local_browser_fixture import Artifacts

PEM = (
    "-----BEGIN OPENSSH PRIVATE KEY-----\n"
    "b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtz\n"
    "c2gtZWQyNTUxOQAAACDexampleprivatekeymaterial\n"
    "-----END OPENSSH PRIVATE KEY-----\n"
)


class ArtifactRedactionTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.artifacts = Artifacts(Path(self.temporary.name) / "artifacts")
        self.artifacts.remember(PEM)

    # Mainline: a multiline private key inside a traceback -> written report
    # -> no raw or JSON-escaped key survives.
    def test_write_json_redacts_a_multiline_secret_before_serialization(self):
        self.artifacts.write_json("report.json", {
            "failure": f"TimeoutError: fill(-----BEGIN OPENSSH PRIVATE KEY-----...)\n{PEM}",
            "nested": [{"detail": PEM}],
        })
        text = (self.artifacts.directory / "report.json").read_text()
        self.assertNotIn(PEM, text)
        self.assertNotIn(PEM.strip(), text)
        self.assertNotIn("exampleprivatekeymaterial", text)
        self.assertNotIn(json.dumps(PEM)[1:-1], text)
        self.assertIn("[REDACTED]", text)

    # Mainline: a hand-written artifact contains only the JSON-escaped form;
    # the scan decodes JSON and rejects it.
    def test_scan_rejects_a_json_escaped_secret(self):
        (self.artifacts.directory / "leak.json").write_text(
            json.dumps({"failure": "fill failed: " + PEM}))
        with self.assertRaisesRegex(RuntimeError, "credential scan rejected"):
            self.artifacts.scan()

    # Mainline: a URL-encoded secret must not bypass the scan.
    def test_scan_rejects_an_encoded_secret_form(self):
        (self.artifacts.directory / "url.txt").write_text(
            urllib.parse.quote(PEM, safe=""))
        with self.assertRaisesRegex(RuntimeError, "credential scan rejected"):
            self.artifacts.scan()

    # Mainline: a traceback string with escaped newlines written as raw text
    # is rejected even when the file is not valid JSON.
    def test_scan_rejects_a_secret_in_a_traceback_text(self):
        (self.artifacts.directory / "traceback.txt").write_text(
            "Traceback (most recent call last):\n  fill('" +
            PEM.replace("\n", "\\n") + "')\n")
        with self.assertRaisesRegex(RuntimeError, "credential scan rejected"):
            self.artifacts.scan()

    # Mainline: clean artifacts stay uploadable and the scanner does not flag
    # unrelated content.
    def test_scan_accepts_clean_artifacts(self):
        self.artifacts.write_json("report.json", {"status": "PASS", "detail": "ssh-ed25519"})
        (self.artifacts.directory / "server.log").write_text("server listening\n")
        self.artifacts.scan()

    # Mainline: a trace with a text resource and a real PNG resource -> the
    # text is redacted and the binary resource keeps every original byte.
    def test_trace_rewrite_keeps_binary_bytes_and_redacts_text(self):
        png = b"\x89PNG\r\n\x1a\n" + bytes(range(256)) + b"\xff\xfe"
        raw = self.artifacts.private / "trace.zip"
        with zipfile.ZipFile(raw, "w") as archive:
            archive.writestr("trace.trace", json.dumps({"body": "fill " + PEM}))
            archive.writestr("resources/pixel.png", png)
        destination = self.artifacts.directory / "trace-1.zip"
        self.artifacts._rewrite_trace(raw, destination)
        with zipfile.ZipFile(destination) as rewritten:
            text = rewritten.read("trace.trace").decode()
            self.assertNotIn("exampleprivatekeymaterial", text)
            self.assertIn("[REDACTED]", text)
            self.assertEqual(rewritten.read("resources/pixel.png"), png)

    # Mainline: a secret embedded in a binary resource is still rejected even
    # though the resource must be copied byte-for-byte.
    def test_trace_rewrite_rejects_a_secret_in_a_binary_resource(self):
        raw = self.artifacts.private / "trace.zip"
        with zipfile.ZipFile(raw, "w") as archive:
            archive.writestr("resources/blob.bin",
                             PEM.encode() + b"\xff\xfe\x00binary")
        with self.assertRaisesRegex(RuntimeError, "credential scan rejected"):
            self.artifacts._rewrite_trace(raw, self.artifacts.directory / "trace-1.zip")

    # Mainline: an arbitrary binary body contains a UTF-8 credential followed
    # by invalid UTF-8 -> reject before any public trace can be published.
    def test_trace_rejects_utf8_secret_in_binary_before_publishing(self):
        secret = "synthetic-clé-🔑-credential"
        self.artifacts.remember(secret)
        raw = self.artifacts.private / "trace.zip"
        destination = self.artifacts.directory / "trace-1.zip"
        with zipfile.ZipFile(raw, "w") as archive:
            archive.writestr("trace.trace", '{"type":"before"}')
            archive.writestr("resources/blob", b"\xff" + secret.encode() + b"\x00")
        with self.assertRaisesRegex(RuntimeError, "credential scan rejected"):
            self.artifacts._rewrite_trace(raw, destination)
        self.assertFalse(destination.exists(), "a rejected trace was published")

    # Mainline: a timeout truncates its fill argument to one PEM payload line
    # -> the diagnostic must not expose that fragment of the private key.
    def test_redacts_private_key_payload_fragments(self):
        material = PEM.splitlines()[1]
        self.artifacts.write_json("report.json", {"failure": f"fill('{material}...')"})
        text = (self.artifacts.directory / "report.json").read_text()
        self.assertNotIn(material, text)
        self.assertIn("[REDACTED]", text)

    # Mainline: an ASCII binary resource is valid UTF-8 but contains NULs;
    # rejecting an embedded credential must not rewrite or publish its bytes.
    def test_trace_rejects_secret_in_utf8_decodable_binary(self):
        raw = self.artifacts.private / "trace.zip"
        destination = self.artifacts.directory / "trace-1.zip"
        with zipfile.ZipFile(raw, "w") as archive:
            archive.writestr("resources/font", b"font\x00" + PEM.encode())
        with self.assertRaisesRegex(RuntimeError, "credential scan rejected"):
            self.artifacts._rewrite_trace(raw, destination)
        self.assertFalse(destination.exists())

    # Mainline: a secret-bearing case forces a failure after the key is
    # visible -> the failure artifact policy refuses to capture a screenshot.
    def test_capture_failure_omits_screenshots_for_secret_phases(self):
        class FakePage:
            def __init__(self):
                self.calls = []

            def screenshot(self, **kwargs):
                self.calls.append(kwargs)
                (self.artifacts_dir / "failure.png").write_bytes(PEM.encode())

        page = FakePage()
        page.artifacts_dir = self.artifacts.directory
        outcome = self.artifacts.capture_failure(page, sensitive=True)
        self.assertIn("omitted", outcome)
        self.assertEqual(page.calls, [])
        self.assertFalse((self.artifacts.directory / "failure.png").exists())
        self.artifacts.write_json("report.json", {"failure": "echoed: " + PEM})
        self.artifacts.scan()


if __name__ == "__main__":
    unittest.main()
