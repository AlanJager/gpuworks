"""Provider endpoint runner close/error isolation self-tests.

Run: PYTHONPATH=test/e2e python3 -m unittest test_provider_endpoint_cleanup -v
E2E standard: test/e2e/browser-e2e-testing-standard.md
"""
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest import mock

from local_browser_fixture import Artifacts
import provider_endpoint_edit as case


class ProviderEntryCleanupTest(unittest.TestCase):
    # Mainline: business success is followed by each actual entry close failure;
    # every later close is still attempted, the server stops, and PASS is withdrawn.
    def test_actual_entry_preserves_stage_failures(self):
        for stage in (None, "trace", "context", "browser", "server", "server-log",
                      "report-before-scan", "artifact-scan", "business"):
            with self.subTest(stage=stage), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                server = mock.Mock(base="http://localhost", process=mock.Mock(pid=42))
                server.start.return_value = {"ok": True}
                server.stop.return_value = True
                if stage == "server":
                    server.stop.side_effect = RuntimeError("injected-server-stop")
                browser, context = mock.Mock(version="test-browser"), mock.Mock()
                browser.new_context.return_value = context
                browser.close.return_value = context.close.return_value = None
                if stage in ("context", "browser"):
                    getattr(context if stage == "context" else browser, "close").side_effect = RuntimeError("injected-close")
                play = mock.MagicMock()
                play.__enter__.return_value.chromium.launch.return_value = browser
                function = case.test_provider_endpoint_edit

                def business(page, context, server, artifacts, report, *args):
                    report["preflight"] = "PASS"
                    report["evidence"] = [{"layer": layer} for layer in ("ui", "request", "persistence", "runtime", "cleanup")]
                    report["remaining_resources"] = []
                    if stage == "business":
                        report["remaining_resources"] = [{"kind": "provider", "id": 7, "scope": "isolated DB"}]
                        raise AssertionError("injected-business-failure")
                business.__name__ = function.__name__
                original_write, original_log, original_scan = Artifacts.write_json, case.copy_server_log, Artifacts.scan
                written_statuses = []
                def write(artifacts, name, report):
                    written_statuses.append(report["status"])
                    if stage == "report-before-scan" and len(written_statuses) == 1:
                        raise OSError("injected-report-write")
                    return original_write(artifacts, name, report)
                def log(artifacts):
                    if stage == "server-log":
                        raise OSError("injected-log-write")
                    return original_log(artifacts)
                def scan(artifacts):
                    if stage == "artifact-scan":
                        raise RuntimeError("injected-secret-scan")
                    return original_scan(artifacts)
                with mock.patch.object(sys, "argv", ["case", "--server-bin", __file__, "--artifact-dir", str(root / "run")]), \
                        mock.patch("local_browser_fixture.LocalServer", return_value=server), \
                        mock.patch("playwright.sync_api.sync_playwright", return_value=play), \
                        mock.patch.object(case, function.__name__, business), \
                        mock.patch.object(case, "copy_server_log", side_effect=log) as copy_log, \
                        mock.patch.object(Artifacts, "write_json", write), \
                        mock.patch.object(Artifacts, "scan", scan), \
                        mock.patch.object(Artifacts, "stop_trace", return_value=None,
                                          side_effect=RuntimeError("injected-close") if stage == "trace" else None):
                    code = case.main()
                report = json.loads((root / "run/report.json").read_text())
                self.assertEqual(code, 1 if stage else 0)
                self.assertEqual(report["status"], "FAIL_TEST" if stage else "PASS")
                self.assertEqual(report["server_stopped"], stage != "server")
                context.close.assert_called_once()
                browser.close.assert_called_once()
                server.stop.assert_called_once()
                copy_log.assert_called_once()
                self.assertNotEqual(written_statuses[0], "PASS")
                for name in ("trace", "context", "browser", "server", "server-log", "report-before-scan", "artifact-scan"):
                    if name == stage:
                        self.assertNotEqual(report["cleanup_stages"][name], "ok")
                    else:
                        self.assertEqual(report["cleanup_stages"][name], "ok")
                if stage == "server":
                    self.assertEqual(report["remaining_resources"], [{"kind": "process", "pid": 42, "reclaim": "kill 42"}])
                elif stage == "business":
                    self.assertIn("injected-business-failure", report["failure"])
                    self.assertEqual(report["remaining_resources"][0]["id"], 7)
                else:
                    self.assertEqual(report["remaining_resources"], [])


if __name__ == "__main__":
    unittest.main()
