"""Offline failure-path regressions for the node/doctor acceptance runner."""
import contextlib
import io
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest import mock

import node_reconnect_doctor as case


class NodeDoctorFixtureTest(unittest.TestCase):
    def test_agent_spawn_failure_closes_the_opened_log(self):
        path = mock.Mock()
        log = path.open.return_value
        with mock.patch.object(case.subprocess, "Popen", side_effect=FileNotFoundError("agent")):
            with self.assertRaises(FileNotFoundError):
                case.start_agent(Path("/missing/agent"), "http://local", "token", Path("state"), path)
        log.close.assert_called_once()

    def test_failed_node_delete_keeps_owner_and_checks_readback(self):
        report = {"owned_node_uuid": "owned-node"}
        with mock.patch.object(case, "http", side_effect=[(500, {}), (200, {})]) as http:
            ok = case.cleanup(SimpleNamespace(base="http://local"), "token", report,
                              {"agent": None, "log": None})
        self.assertFalse(ok)
        self.assertEqual(report["owned_node_uuid"], "owned-node")
        self.assertEqual(http.call_args_list[-1].args[1:3], ("GET", "/api/v1/nodes/owned-node"))
        self.assertEqual(report["remaining_resources"][0]["id"], "owned-node")

    def test_success_status_without_disappearance_is_not_cleanup(self):
        report = {"owned_node_uuid": "owned-node"}
        with mock.patch.object(case, "http", side_effect=[(200, {}), (200, {})]):
            self.assertFalse(case.cleanup(SimpleNamespace(base="http://local"), "token",
                                          report, {"agent": None, "log": None}))
        self.assertEqual(report["owned_node_uuid"], "owned-node")

    def test_preparation_failure_redacts_local_admin_token_and_stops_server(self):
        token = "synthetic-secret-admin-token-ABCDEFGHIJKLM"
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp) / "run"
            server = mock.Mock(base="http://local", process=SimpleNamespace(pid=123))
            server.start.return_value = {}
            server.stop.return_value = True
            with mock.patch.object(case, "LocalServer", return_value=server), \
                    mock.patch.object(case, "digest", return_value="test-digest"), \
                    mock.patch.object(case, "http", side_effect=[(200, {"token": token}),
                                                               RuntimeError(token)]), \
                    mock.patch.object(case.sys, "argv", ["case", "--artifact-dir", str(directory)]), \
                    contextlib.redirect_stdout(io.StringIO()):
                self.assertNotEqual(case.main(), 0)
            report = json.loads((directory / "report.json").read_text())
            self.assertNotIn("admin_token", report)
            self.assertNotIn(token, (directory / "report.json").read_text())
            self.assertTrue(report["server_stopped"])
            server.stop.assert_called_once()


if __name__ == "__main__":
    unittest.main()
