"""MCP runner cleanup self-tests.

Run: PYTHONPATH=test/e2e python3 -m unittest test_mcp_cleanup -v
"""
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest import mock

from local_browser_fixture import Artifacts
import mcp_copied_config as copied
import mcp_settings_lifecycle as lifecycle


class CleanupTest(unittest.TestCase):
    def test_screenshot_error_preserves_original_failure(self):
        page = mock.Mock()
        page.content.return_value = "safe page"
        page.screenshot.side_effect = RuntimeError("closed-page")
        report = {"status": "FAIL_TEST", "failure": "original assertion"}
        with tempfile.TemporaryDirectory() as directory:
            lifecycle.capture_failure(page, Artifacts(Path(directory) / "run"), report)
        self.assertEqual(report["failure"], "original assertion")
        self.assertEqual(report["status"], "FAIL_TEST")
        self.assertIn("closed-page", report["screenshot_error"])

    def test_failed_resource_readback_retains_id_and_continues(self):
        page = mock.Mock()
        page.request.get.side_effect = [
            mock.Mock(status=200, json=lambda: {"enabled": False}),
            mock.Mock(status=200, json=lambda: {"enabled": False}),
            mock.Mock(status=503),
            mock.Mock(status=200, json=lambda: {"items": [], "roles": [], "types": [], "type_roles": {}}),
        ]
        page.request.delete.side_effect = [mock.Mock(status=204), mock.Mock(status=200)]
        report = {"status": "PASS", "owned_resources": {"platform_token_id": 7, "network_id": 8}}
        server = mock.Mock(base="http://localhost", process=mock.Mock(pid=42))
        server.stop.return_value = True
        lifecycle.cleanup_fixture(page, server, report)
        lifecycle.stop_server(report, server)
        self.assertEqual(report["status"], "FAIL_TEST")
        self.assertEqual(report["owned_resources"], {"platform_token_id": 7})
        self.assertEqual(report["remaining_resources"][0]["id"], 7)
        self.assertEqual(report["cleanup_stages"]["network_id"], "ok")
        self.assertTrue(report["server_stopped"])

    def test_actual_entries_preserve_close_failure(self):
        for module in (lifecycle, copied):
            for stage in (None, "trace", "context", "browser", "report"):
                with self.subTest(module=module.__name__, stage=stage), tempfile.TemporaryDirectory() as directory:
                    root = Path(directory)
                    server = mock.Mock(base="http://localhost", process=mock.Mock(pid=42))
                    server.start.return_value = {"ok": True}
                    server.stop.return_value = True
                    browser, context = mock.Mock(version="fixture"), mock.Mock()
                    browser.close.return_value = context.close.return_value = None
                    browser.new_context.return_value = context
                    page = context.new_page.return_value
                    page.content.return_value = ""
                    page.request.get.return_value.status = 200
                    page.request.get.return_value.json.return_value = {"enabled": False}
                    if stage in ("context", "browser"):
                        getattr(context if stage == "context" else browser, "close").side_effect = RuntimeError("injected-close")
                    play = mock.MagicMock()
                    play.__enter__.return_value.chromium.launch.return_value = browser
                    function = module.test_mcp_toggle_runtime if module is lifecycle else module.test_mcp_copied_config
                    def business(page, context, server, artifacts, report, *args):
                        report["preflight"] = "PASS"
                        report["evidence"] = [{"layer": layer} for layer in ("ui", "request", "persistence", "runtime", "cleanup")]
                    argv = ["case", "--server-bin", __file__, "--artifact-dir", str(root / "run")]
                    if module is copied:
                        argv += ["--case", "endpoint"]
                    original_write = Artifacts.write_json
                    writes = []
                    def write(artifacts, name, report):
                        writes.append(report["status"])
                        if stage == "report" and len(writes) == 1:
                            raise OSError("injected-report-write")
                        return original_write(artifacts, name, report)
                    with mock.patch.object(sys, "argv", argv), \
                            mock.patch.object(module, "LocalServer", return_value=server), \
                            mock.patch.object(module, "sync_playwright", return_value=play), \
                            mock.patch.object(module, function.__name__, side_effect=business), \
                            mock.patch.object(Artifacts, "write_json", write), \
                            mock.patch.object(Artifacts, "stop_trace", return_value=None, side_effect=RuntimeError("injected-close") if stage == "trace" else None):
                        # Manifest binding inspects the function name.
                        getattr(module, function.__name__).__name__ = function.__name__
                        code = module.main()
                    report = json.loads((root / "run/report.json").read_text())
                    self.assertEqual(code, 1 if stage else 0, report.get("failure"))
                    self.assertEqual(report["status"], "FAIL_TEST" if stage else "PASS", report.get("failure"))
                    self.assertTrue(report["server_stopped"])
                    context.close.assert_called_once()
                    browser.close.assert_called_once()
                    self.assertGreaterEqual(page.request.get.call_count, 2)
                    self.assertEqual(report["remaining_resources"], [])
                    if stage in (None, "report"):
                        self.assertEqual(writes[0], "NOT_RUN")

    def test_missing_one_time_token_reclaims_known_id(self):
        for module in (lifecycle, copied):
            for payload in ({"item": {"id": 7, "scope": "read"}},
                            {"token": True, "item": {"id": 7, "scope": "read"}}):
                with self.subTest(module=module.__name__, payload=payload):
                    self.check_prepare_failure(module, payload)

    def check_prepare_failure(self, module, payload):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            server = mock.Mock(base="http://localhost", process=mock.Mock(pid=42))
            server.start.return_value = {"ok": True}
            server.stop.return_value = True
            browser, context = mock.Mock(version="fixture"), mock.Mock()
            browser.close.return_value = context.close.return_value = None
            browser.new_context.return_value = context
            context.cookies.return_value = []
            context.new_page.return_value = mock.MagicMock()
            page = context.new_page.return_value
            page.url = server.base + "/settings#access"
            page.content.return_value = ""
            page.expect_response.return_value.__enter__.return_value.value.status = 200
            def get(url):
                if url.endswith(lifecycle.TOKENS):
                    body = {"items": [], "total": 0}
                elif url.endswith("/api/v1/networks"):
                    body = {"items": [], "roles": [], "types": [], "type_roles": {}}
                else:
                    body = {"enabled": False, "path": lifecycle.MCP, "protocol_version": "2025-03-26", "tools": ["call_api"]}
                return mock.Mock(status=200, json=lambda: body)
            def post(url, data):
                body = {**data, "id": 9} if url.endswith("/api/v1/networks") else payload
                return mock.Mock(status=201, json=lambda: body)
            def delete(url):
                return mock.Mock(status=204 if url.endswith("/7") else 200)
            page.request.get.side_effect = get
            page.request.post.side_effect = post
            page.request.delete.side_effect = delete
            play = mock.MagicMock()
            play.__enter__.return_value.chromium.launch.return_value = browser
            argv = ["case", "--server-bin", __file__, "--artifact-dir", str(root / "run")]
            if module is copied:
                argv += ["--case", "endpoint"]
            with mock.patch.object(sys, "argv", argv), \
                    mock.patch.object(module, "LocalServer", return_value=server), \
                    mock.patch.object(module, "sync_playwright", return_value=play), \
                    mock.patch.object(module, "expect"):
                code = module.main()
            report = json.loads((root / "run/report.json").read_text())
            self.assertEqual(code, 1)
            self.assertIn("incomplete credential", report["failure"])
            page.request.delete.assert_any_call(server.base + lifecycle.TOKENS + "/7")
            if module is copied:
                page.request.delete.assert_any_call(server.base + "/api/v1/networks/9")
            self.assertEqual(report["cleanup_stages"]["platform_token_id"], "ok")
            self.assertEqual(report["remaining_resources"], [])
            self.assertTrue(report["server_stopped"])


if __name__ == "__main__":
    unittest.main()
