"""Offline regression checks for terminal error admission and scoped cleanup."""
import unittest
from types import SimpleNamespace
from unittest import mock

import terminal_host as case


class Events:
    def __init__(self):
        self.handlers = {}

    def on(self, event, callback):
        self.handlers[event] = callback


class TerminalFixtureTest(unittest.TestCase):
    def test_popup_errors_are_never_expected_disabled_success(self):
        popup = Events()
        errors = {"page": [], "console": [], "http": []}
        case.monitor_terminal_popup(popup, errors)
        popup.handlers["pageerror"](RuntimeError("startup failed"))
        popup.handlers["console"](SimpleNamespace(type="error", text="unexpected error"))
        popup.handlers["response"](SimpleNamespace(
            url="http://local/api/v1/nodes/owned/terminal/settings", status=500,
            request=SimpleNamespace(method="GET")))
        self.assertEqual(errors, {
            "page": ["startup failed"], "console": ["unexpected error"],
            "http": [{"method": "GET", "path": "/api/v1/nodes/owned/terminal/settings", "status": 500}],
        })

    def test_delete_timeout_keeps_scoped_node_identity(self):
        report = {"owned_node_uuid": "owned-node"}
        with mock.patch.object(case, "http", side_effect=TimeoutError("delete timeout")):
            ok = case.cleanup(SimpleNamespace(base="http://local"), "token", report, None, None)
        self.assertFalse(ok)
        self.assertEqual(report["owned_node_uuid"], "owned-node")
        self.assertEqual(report["remaining_resources"][0]["id"], "owned-node")
        self.assertIn("DELETE /api/v1/nodes/owned-node?force=true",
                      report["remaining_resources"][0]["reclaim"])

    def test_agent_log_close_failure_is_not_clean(self):
        log = mock.Mock(closed=False)
        log.close.side_effect = OSError("close failed")
        report = {}
        self.assertFalse(case.cleanup(SimpleNamespace(base="http://local"), "token", report, None, log))
        self.assertTrue(report["cleanup_errors"])


if __name__ == "__main__":
    unittest.main()
