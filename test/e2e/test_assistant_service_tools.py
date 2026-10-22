"""Assistant fact-oracle regressions; test/e2e/browser-e2e-testing-standard.md."""
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch
import assistant_service_tools as case


class AssistantFactTests(unittest.TestCase):
    # Mainline: stale/duplicated/mistyped tool facts must not become the
    # rendered expected answer merely because they resemble inventory counts.
    def test_inventory_oracle_rejects_duplicate_and_boolean_tool_facts(self):
        inventory = {"total": 1, "items": [{"name": "owned-node"}]}
        for total, names in ((True, ["owned-node"]),
                             (1, ["owned-node", "owned-node"]),
                             (1, ["stale-node"])):
            with self.subTest(total=total, names=names), self.assertRaises(AssertionError):
                case.cluster_inventory_answer({
                    "summary": {"nodes": {"total": total}},
                    "nodes_needing_attention": {"items": [{"name": name} for name in names]}}, inventory)

    def test_inventory_is_the_independent_answer_source(self):
        inventory = {"total": 1, "items": [{"name": "owned-node"}]}
        fact = {"summary": {"nodes": {"total": 1}},
                "nodes_needing_attention": {"items": [{"name": "owned-node"}]}}
        self.assertEqual(case.cluster_inventory_answer(fact, inventory),
                         "The cluster overview reports 1 node(s); attention includes owned-node.")

    # Mainline: server startup/stop fail -> still stop the fixture, redact its
    # already-acquired key, and write both cleanup failures as non-PASS.
    def test_cleanup_exceptions_do_not_skip_the_fixture_or_report(self):
        with tempfile.TemporaryDirectory() as temporary:
            destination = Path(temporary) / "artifacts"
            server = Mock()
            server.start.side_effect = RuntimeError("startup failed")
            server.stop.side_effect = RuntimeError("server stop failed")
            fixture = Mock(base="http://127.0.0.1:1", model="fixture", api_key="synthetic-fixture-secret")
            fixture.start.return_value = fixture
            fixture.stop.side_effect = RuntimeError("fixture stop failed " + fixture.api_key)
            with patch.object(case, "LocalServer", return_value=server), \
                    patch.object(case, "AssistantEndpoint", return_value=fixture), \
                    patch.object(case, "digest", return_value="test-sha"), \
                    patch.object(case.subprocess, "check_output", return_value="test-head"), \
                    patch("sys.argv", ["assistant_service_tools.py", "--artifact-dir", str(destination)]):
                self.assertEqual(case.main(), 1)
            fixture.stop.assert_called_once()
            text = (destination / "report.json").read_text()
            self.assertNotIn(fixture.api_key, text)
            report = json.loads(text)
            self.assertEqual(report["status"], "FAIL_TEST")
            self.assertFalse(report["server_stopped"])
            self.assertFalse(report["fixture_stopped"])
            self.assertIn("[REDACTED]", report["fixture_stop_error"])

    # Mainline: an assertion finds an upstream credential on the page ->
    # retain the redacted failure without photographing the exposed value.
    def test_secret_bearing_case_failure_does_not_capture_a_screenshot(self):
        with tempfile.TemporaryDirectory() as temporary:
            destination = Path(temporary) / "artifacts"
            server = Mock(base="http://127.0.0.1:1")
            server.start.return_value = {}
            server.process.pid = 123
            server.stop.return_value = True
            fixture = Mock(base="http://127.0.0.1:2", model="fixture", api_key="synthetic-upstream-secret")
            fixture.start.return_value = fixture
            play = Mock()
            browser = play.chromium.launch.return_value
            browser.version = "test-browser"
            page = browser.new_context.return_value.new_page.return_value
            manager = Mock()
            manager.__enter__ = Mock(return_value=play)
            manager.__exit__ = Mock(return_value=False)

            def test_assistant_service_tools(page, context, server, artifacts, report, locale, endpoint):
                report["preflight"] = "PASS"
                raise AssertionError("page echoed " + endpoint.api_key)

            with patch.object(case, "LocalServer", return_value=server), \
                    patch.object(case, "AssistantEndpoint", return_value=fixture), \
                    patch.object(case, "sync_playwright", return_value=manager), \
                    patch.object(case, "test_assistant_service_tools", test_assistant_service_tools), \
                    patch.object(case, "digest", return_value="test-sha"), \
                    patch.object(case.subprocess, "check_output", return_value="test-head"), \
                    patch("sys.argv", ["assistant_service_tools.py", "--artifact-dir", str(destination)]):
                self.assertEqual(case.main(), 1)
            page.screenshot.assert_not_called()
            self.assertNotIn(fixture.api_key, (destination / "report.json").read_text())


if __name__ == "__main__":
    unittest.main()
