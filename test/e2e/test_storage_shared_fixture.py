"""Failure-path tests for owned shared-storage fixtures (no processes/mounts)."""
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import storage_shared_dir as case


class SharedFixtureTests(unittest.TestCase):
    def test_failed_delete_retains_id_and_later_stages_run(self):
        report = {"owned_storage_id": 7, "owned_node_uuid": "owned-node"}
        def request(base, method, path, **kwargs):
            if method == "DELETE":
                raise TimeoutError("transport unavailable")
            return 200, {}
        with patch.object(case, "http", side_effect=request), patch.object(
                case, "unmount_shared", return_value=True) as unmount:
            self.assertFalse(case.cleanup(SimpleNamespace(base="unused"), "", report, {}, Path("/owned")))
        self.assertEqual(report["owned_storage_id"], 7)
        self.assertEqual(report["owned_node_uuid"], "owned-node")
        self.assertEqual({item["kind"] for item in report["remaining_resources"]}, {"storage", "node"})
        unmount.assert_called_once()

    def test_readback_404_resolves_unknown_delete_for_storage_and_node(self):
        report = {"owned_storage_id": 7, "owned_node_uuid": "owned-node"}
        def request(base, method, path, **kwargs):
            if method == "DELETE":
                raise TimeoutError("reply lost after delete")
            return 404, {}
        with patch.object(case, "http", side_effect=request), patch.object(case, "unmount_shared", return_value=True):
            self.assertFalse(case.cleanup(SimpleNamespace(base="unused"), "", report, {}, Path("/owned")))
        self.assertIsNone(report["owned_storage_id"])
        self.assertIsNone(report["owned_node_uuid"])
        self.assertEqual(report["remaining_resources"], [])

    def test_failed_process_start_keeps_log_owned_for_cleanup(self):
        with tempfile.TemporaryDirectory() as directory:
            server = SimpleNamespace(base="unused", work=Path(directory))
            report, holder = {}, {}
            with patch.object(case, "http", return_value=(201, {"node": {"uuid": "owned-node"}, "token": "synthetic"})), patch.object(case.subprocess, "Popen", side_effect=OSError("cannot start")):
                with self.assertRaises(OSError):
                    case.create_node(server, "", "run", report, holder, Path("/selected"))
            try:
                self.assertEqual(report["owned_node_uuid"], "owned-node")
                self.assertIsNotNone(holder.get("log"))
            finally:
                if holder.get("log"):
                    holder["log"].close()


if __name__ == "__main__":
    unittest.main()
