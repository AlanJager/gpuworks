"""Artifact safety tests; these are infrastructure tests, not browser coverage.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: python3 -m unittest discover -s test/e2e -p test_local_browser_fixture.py
"""

import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock
import zipfile

import yaml

import local_browser_fixture
from local_browser_fixture import Artifacts, LocalServer, write_server_config


class ArtifactSafetyTest(unittest.TestCase):
    # Mainline: synthetic secret and cookie → trace export → redacted contents + raw trace removed.
    def test_trace_and_report_remove_credentials(self):
        with tempfile.TemporaryDirectory() as directory:
            artifacts = Artifacts(Path(directory) / "run")
            secret = "gwp_" + "fixture-only-not-a-real-token" * 2
            cookie = "fixture-cookie-never-authorized"
            artifacts.remember(secret)

            class Context:
                tracing = None

                def stop(self, path):
                    with zipfile.ZipFile(path, "w") as archive:
                        archive.writestr("trace.network", json.dumps({"cookie": cookie, "token": secret}))

                def cookies(self):
                    return [{"value": cookie}]

            context = Context()
            context.tracing = context
            artifacts.tracing = True
            artifacts.stop_trace(context)
            artifacts.write_json("report.json", {"error": f"{secret} / {cookie}"})
            artifacts.scan()
            self.assertFalse((artifacts.private / "trace.zip").exists())
            with zipfile.ZipFile(artifacts.directory / "trace-1.zip") as archive:
                result = json.loads(archive.read("trace.network"))
            self.assertEqual(result, {"cookie": "[REDACTED]", "token": "[REDACTED]"})
            self.assertNotIn(secret, (artifacts.directory / "report.json").read_text())

    # Mainline: accidental credential in a compressed resource → artifact scan → reject instead of publish.
    def test_scan_rejects_secret_inside_zip(self):
        with tempfile.TemporaryDirectory() as directory:
            artifacts = Artifacts(Path(directory) / "run")
            secret = "synthetic-credential-for-negative-test"
            artifacts.remember(secret)
            with zipfile.ZipFile(artifacts.directory / "leak.zip", "w", zipfile.ZIP_DEFLATED) as archive:
                archive.writestr("binary", b"\x00\xff" + secret.encode())
            with self.assertRaisesRegex(RuntimeError, "credential scan rejected leak.zip"):
                artifacts.scan()


class ServerConfigTest(unittest.TestCase):
    # Mainline: run directory with YAML-significant and non-BMP characters → server.yaml → only that root,
    # only the loopback fixture host, and the locked listen address; never overwrites an existing file.
    def test_isolated_installation_config(self):
        with tempfile.TemporaryDirectory() as directory:
            work = Path(directory) / 'run: #1 "quoted" \\ é 😀'
            root = work / "storage"
            root.mkdir(parents=True)
            config = write_server_config(work, root, "127.0.0.1:8800")
            self.assertEqual(config, work / "server.yaml")
            self.assertEqual(config.stat().st_mode & 0o777, 0o600)
            # Go's YAML decoder rejects the surrogate pairs json.dumps emits by default.
            self.assertNotIn("\\ud83d", config.read_text())
            self.assertEqual(yaml.safe_load(config.read_text()), {
                "listen": {"console": ["127.0.0.1:8800"]},
                "storage_probe_roots": [str(root)],
                "outbound_allow_private_hosts": ["127.0.0.1"]})
            with self.assertRaises(FileExistsError):
                write_server_config(work, root, "127.0.0.1:8802")
            # A control character would make the server refuse its config (or, for NEL, misread the root).
            other = Path(directory) / "other"
            other.mkdir()
            with self.assertRaisesRegex(ValueError, "printable"):
                write_server_config(other, other / "bad\x85root", "127.0.0.1:8800")
            self.assertFalse((other / "server.yaml").exists())

    # Mainline: LocalServer.start → serve gets --config with that file and no --addr; root excludes server data.
    def test_server_runs_from_its_config_without_addr_override(self):
        with tempfile.TemporaryDirectory() as directory:
            work = Path(directory) / "private"
            work.mkdir()
            server = LocalServer(Path("/bin/true"), work)
            launched = []

            def refuse(argv, **kwargs):
                launched.append(argv)
                raise RuntimeError("stop after launch arguments")

            with mock.patch.object(local_browser_fixture.subprocess, "Popen", side_effect=refuse):
                with self.assertRaisesRegex(RuntimeError, "stop after launch"):
                    server.start()
            server.stop()
            argv = launched[0]
            self.assertEqual(argv[1:4], ["serve", "--config", str(work.resolve() / "server.yaml")])
            self.assertNotIn("--addr", argv)
            config = yaml.safe_load((work / "server.yaml").read_text())
            self.assertEqual(config["listen"]["console"], [server.base.removeprefix("http://")])
            self.assertEqual(config["storage_probe_roots"], [str(work.resolve() / "storage")])
            self.assertEqual(server.environment["storage_probe_roots"], config["storage_probe_roots"])
            self.assertFalse((work.resolve() / "server-data").is_relative_to(work.resolve() / "storage"))

if __name__ == "__main__":
    unittest.main()
