"""Failure ownership for wizard prerequisites; external runtime remains real in E2E.

Run: PYTHONPATH=test/e2e python3 -m unittest test_deployment_wizard_fixture -v
"""
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import MagicMock, patch
from local_browser_fixture import Artifacts

import deployment_wizard_fixture as fixture


class FixtureOwnership(unittest.TestCase):
    def test_model_registration_failure_keeps_node_and_agent_for_cleanup(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            storage = root / "storage"
            storage.mkdir()
            server = SimpleNamespace(base="http://127.0.0.1:8800", work=root, storage_root=storage)
            calls = []
            def request(base, method, path, **kwargs):
                calls.append((method, path))
                if path == "/api/v1/nodes":
                    return 201, {"node": {"uuid": "owned-node"}, "token": "node-secret"}
                if path == "/api/v1/models/local":
                    return 400, {"code": "MODEL_LOCAL_PATH_NOT_FOUND"}
                self.fail(f"unexpected fixture request: {method} {path}")
            report, holder, secrets = {}, {}, []
            process = MagicMock()
            with patch.object(fixture, "http", side_effect=request), patch.object(
                    fixture, "wait_online"), patch.object(fixture.subprocess, "Popen", return_value=process):
                with self.assertRaisesRegex(AssertionError, "model fixture: 400"):
                    fixture.prepare(server, "admin-secret", "case", report, holder, Path("agent"), secrets.append)
            self.assertEqual(report["owned_node_uuid"], "owned-node")
            self.assertIs(holder["agent"], process)
            self.assertEqual(secrets, ["node-secret"])
            self.assertEqual(calls, [("POST", "/api/v1/nodes"), ("POST", "/api/v1/models/local")])
            holder["log"].close()

    def test_invalid_node_credentials_keep_identity_without_poisoning_artifacts(self):
        for credential in (True, 1, {}, [], None, "", " "):
            with self.subTest(credential=credential), tempfile.TemporaryDirectory() as directory:
                artifacts = Artifacts(Path(directory) / "artifacts")
                server = SimpleNamespace(base="http://local", work=artifacts.private)
                report, holder = {}, {}
                with patch.object(fixture, "http", return_value=(201, {
                        "node": {"uuid": "known-node"}, "token": credential})), patch.object(
                        fixture.subprocess, "Popen") as start:
                    with self.assertRaisesRegex(AssertionError, "no valid token"):
                        fixture.prepare(server, "admin", "case", report, holder, Path("agent"), artifacts.remember)
                self.assertEqual(report["owned_node_uuid"], "known-node")
                start.assert_not_called()
                self.assertEqual(artifacts.secrets, ["gpuworks-test-password-2026"])
                artifacts.write_json("report.json", {"status": "FAIL_TEST", **report})
                artifacts.scan()

    def test_invalid_inference_credentials_keep_key_name_and_report_writable(self):
        for credential in (True, 1, {}, [], None, "", " "):
            with self.subTest(credential=credential), tempfile.TemporaryDirectory() as directory:
                artifacts = Artifacts(Path(directory) / "artifacts")
                storage = artifacts.private / "storage"
                storage.mkdir()
                server = SimpleNamespace(base="http://local", work=artifacts.private, storage_root=storage)
                report, holder = {}, {}
                replies = [(201, {"node": {"uuid": "known-node"}, "token": "valid-node-secret"}),
                           (201, {"id": 3}), (200, {}), (200, {"id": 7}),
                           (201, {"name": "e2e-case-key", "key": credential})]
                with patch.object(fixture, "http", side_effect=replies), patch.object(
                        fixture, "wait_online"), patch.object(fixture.subprocess, "Popen"):
                    with self.assertRaisesRegex(AssertionError, "no valid key"):
                        fixture.prepare(server, "admin", "case", report, holder, Path("agent"), artifacts.remember)
                self.assertEqual(report["owned_node_uuid"], "known-node")
                self.assertEqual(report["owned_key_name"], "e2e-case-key")
                self.assertEqual(artifacts.secrets, ["gpuworks-test-password-2026", "valid-node-secret"])
                artifacts.write_json("report.json", {"status": "FAIL_TEST", **report})
                artifacts.scan()
                holder["log"].close()

    def test_import_response_failure_retains_spec_identity_for_cleanup(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            storage = root / "storage"
            storage.mkdir()
            server = SimpleNamespace(base="http://127.0.0.1:8800", work=root, storage_root=storage)
            report, holder = {}, {}
            replies = [(201, {"node": {"uuid": "node"}, "token": "node-secret"}),
                       (201, {"id": 3}), TimeoutError("import response lost")]
            with patch.object(fixture, "http", side_effect=replies), patch.object(
                    fixture, "wait_online"), patch.object(fixture.subprocess, "Popen"):
                with self.assertRaisesRegex(TimeoutError, "import response lost"):
                    fixture.prepare(server, "admin", "case", report, holder, Path("agent"), lambda value: None)
            self.assertEqual(report["owned_template_model_id"], "e2e/case")
            self.assertEqual(report["owned_model_id"], 3)
            holder["log"].close()

    def test_unreadable_create_response_recovers_owned_deployment_by_exact_name(self):
        report = {"owned_deployment_name": "e2e-run-deployment"}
        server = SimpleNamespace(base="http://127.0.0.1:8800")
        with patch.object(fixture.containers, "cleanup", return_value=True) as cleanup, patch.object(
                fixture, "http", return_value=(200, {"items": [
                    {"id": 7, "name": "other-e2e-run-deployment"},
                    {"id": 8, "name": "e2e-run-deployment"}]})):
            self.assertTrue(fixture.cleanup(server, "admin", report, {}))
        self.assertEqual(cleanup.call_args.args[2]["owned_deployment_id"], 8)

    def test_recovery_lookup_error_retains_scoped_name_and_continues_other_cleanup(self):
        report = {"owned_deployment_name": "e2e-run-deployment"}
        server = SimpleNamespace(base="http://127.0.0.1:8800")
        with patch.object(fixture.containers, "cleanup", return_value=True) as cleanup, patch.object(
                fixture, "http", side_effect=TimeoutError("lookup timeout")):
            self.assertFalse(fixture.cleanup(server, "admin", report, {}))
        cleanup.assert_called_once()
        self.assertEqual(report["remaining_resources"][0]["id"], "e2e-run-deployment")
        self.assertIn("lookup timeout", report["remaining_resources"][0]["state"])


    def test_template_cleanup_network_error_is_not_disappearance(self):
        report = {"owned_template_model_id": "e2e/run"}
        server = SimpleNamespace(base="http://127.0.0.1:8800")
        with patch.object(fixture.containers, "cleanup", return_value=True), patch.object(
                fixture, "http", side_effect=ConnectionError("fixture server unavailable")):
            self.assertFalse(fixture.cleanup(server, "admin", report, {}))
        self.assertEqual(report["owned_template_model_id"], "e2e/run")
        self.assertEqual(report["remaining_resources"][0]["id"], "e2e/run")
        self.assertIn("DELETE /api/v1/inference-templates/spec?model_id=e2e%2Frun", report["remaining_resources"][0]["reclaim"])

    def test_template_cleanup_requires_independent_404(self):
        for code, expected in [(404, True), (200, False), (503, False)]:
            with self.subTest(code=code):
                report = {"owned_template_model_id": "e2e/run"}
                server = SimpleNamespace(base="http://127.0.0.1:8800")
                with patch.object(fixture.containers, "cleanup", return_value=True), patch.object(
                        fixture, "http", side_effect=[(200, {"deleted": True}), (code, {})]):
                    self.assertIs(fixture.cleanup(server, "admin", report, {}), expected)
                self.assertEqual(report["owned_template_model_id"], None if expected else "e2e/run")


if __name__ == "__main__":
    unittest.main()
