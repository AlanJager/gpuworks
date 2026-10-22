"""Runner failure regressions; E2E standard: test/e2e/browser-e2e-testing-standard.md."""
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

import settings_ssh_key as case


class SSHRunnerTests(unittest.TestCase):
    # Mainline: isolated server startup fails -> stopping it also raises ->
    # retain a non-PASS report and still remove the owned private-key directory.
    def test_stop_exception_preserves_failure_report(self):
        with tempfile.TemporaryDirectory() as temporary:
            destination = Path(temporary) / "artifacts"
            server = Mock()
            server.start.side_effect = RuntimeError("injected startup failure")
            server.stop.side_effect = RuntimeError("injected stop failure")
            with patch.object(case, "LocalServer", return_value=server), \
                    patch.object(case, "generate_key", return_value=("synthetic-key", {"fingerprint": "test"})), \
                    patch.object(case, "digest", return_value="test-sha"), \
                    patch.object(case.subprocess, "check_output", return_value="test-head"), \
                    patch("sys.argv", ["settings_ssh_key.py", "--artifact-dir", str(destination)]):
                self.assertEqual(case.main(), 1)
            report = json.loads((destination / "report.json").read_text())
            self.assertEqual(report["status"], "BLOCKED_INFRA")
            self.assertFalse(report["server_stopped"])
            self.assertIn("injected stop failure", report["server_stop_error"])
            self.assertTrue(report["private_key_removed"])
            self.assertFalse(report["evidence"][-1]["cleanup_ok"])


if __name__ == "__main__":
    unittest.main()
