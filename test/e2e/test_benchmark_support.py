"""Failure paths must not prevent another owned resource from being stopped."""
import unittest
from unittest.mock import Mock
from benchmark_support import finish_environment, close_browser


class CleanupTests(unittest.TestCase):
    def test_context_close_failure_still_closes_browser_and_invalidates_pass(self):
        context, browser, artifacts = Mock(), Mock(), Mock()
        context.close.side_effect = RuntimeError("injected close failure")
        artifacts.redact.side_effect = lambda value: value
        report = {"status": "PASS"}
        close_browser(context, browser, report, artifacts)
        browser.close.assert_called_once()
        self.assertEqual(report["status"], "FAIL_TEST")
        self.assertFalse(report["browser_close_ok"])

    def test_failed_server_stop_does_not_skip_fixture_or_result(self):
        server, fixture = Mock(), Mock()
        server.stop.side_effect = TimeoutError("injected")
        fixture.stop.return_value = True
        report = {"status": "PASS"}
        finish_environment(server, fixture, report)
        fixture.stop.assert_called_once()
        self.assertFalse(report["environment_cleanup_ok"])
        self.assertEqual(report["status"], "FAIL_TEST")
        self.assertTrue(report["cleanup_errors"])

    def test_unfinished_fixture_does_not_claim_cleanup_success(self):
        server, fixture = Mock(), Mock()
        server.stop.return_value = True
        fixture.stop.return_value = False
        report = {"status": "PASS"}
        finish_environment(server, fixture, report)
        self.assertFalse(report["fixture_stopped"])
        self.assertFalse(report["environment_cleanup_ok"])
        self.assertEqual(report["status"], "FAIL_TEST")
