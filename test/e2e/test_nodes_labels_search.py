"""Runner teardown regression; test/e2e/browser-e2e-testing-standard.md."""
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

import nodes_labels_search as case


class RunnerCleanupTests(unittest.TestCase):
    # Mainline: readiness fails and teardown raises -> retain the original
    # failure and cleanup error in a final non-PASS report.
    def test_stop_exception_still_writes_the_failure_report(self):
        with tempfile.TemporaryDirectory() as temporary:
            destination = Path(temporary) / "artifacts"
            server = Mock()
            server.start.side_effect = RuntimeError("injected readiness failure")
            server.stop.side_effect = RuntimeError("injected teardown failure")
            with patch.object(case, "LocalServer", return_value=server), \
                    patch.object(case, "digest", return_value="test-sha"), \
                    patch.object(case.subprocess, "check_output", return_value="test-head"), \
                    patch("sys.argv", ["nodes_labels_search.py", "--artifact-dir", str(destination)]):
                self.assertEqual(case.main(), 1)
            report = json.loads((destination / "report.json").read_text())
            self.assertEqual(report["status"], "FAIL_TEST")
            self.assertFalse(report["server_stopped"])
            self.assertIn("injected readiness failure", report["failure"])
            self.assertIn("injected teardown failure", report["server_stop_error"])


if __name__ == "__main__":
    unittest.main()
