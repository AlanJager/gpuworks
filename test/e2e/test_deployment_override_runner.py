"""Run the actual override entry and shared lifecycle failure matrix.

Run: PYTHONPATH=test/e2e python3 -m unittest test_deployment_override_runner -v
"""
import json
import os
from pathlib import Path
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

from local_browser_fixture import ROOT, digest
import test_deployment_wizard_runner as wizard_runner
import deployment_override_create as case


class OverrideLifecycle(wizard_runner.MainLifecycle):
    def run_main(self, **kwargs):
        return super().run_main(override=True, **kwargs)

    def test_override_metadata_points_to_actual_business_runner_and_fixture(self):
        result, report = self.run_main()
        self.assertEqual(result, 0)
        self.assertEqual(report["contract"], "gpuworks/deployment/container-overrides")
        self.assertEqual(report["test_file"], "test/e2e/deployment_override_create.py")
        self.assertEqual(report["test_sha256"], digest(ROOT / "test/e2e/deployment_override_create.py"))
        self.assertEqual(report["runner_sha256"], digest(ROOT / "test/e2e/deployment_wizard_create.py"))
        self.assertEqual(report["case_fixture_file"], "test/e2e/deployment_override_fixture.py")
        self.assertEqual(report["case_fixture_sha256"], digest(ROOT / report["case_fixture_file"]))
        self.assertEqual(report["command"][1], report["test_file"])

    def test_actual_image_cleanup_failure_is_nonpass_and_remaining_source_tags_still_close(self):
        for failure in ("daemon", "timeout", "present"):
            with self.subTest(failure=failure):
                result, report = self.run_main(image_cleanup=failure)
                self.assertEqual(result, 1)
                self.assertEqual(report["status"], "FAIL_TEST")
                self.assertTrue(report["agent_stopped"])
                self.assertTrue(report["server_stopped"])
                self.assertTrue(report["browser_close_ok"])
                self.assertFalse(report["environment_cleanup_ok"])
                self.assertEqual(report["owned_override_image"], "gpuworks/e2e-overrides:own")
                self.assertEqual(report["owned_override_source_tags"], [])
                self.assertEqual(len(report["remaining_resources"]), 1)
                self.assertEqual(report["remaining_resources"][0]["kind"], "image")
                self.assertEqual(report["remaining_resources"][0]["id"], "gpuworks/e2e-overrides:own")
                self.assertEqual(report["remaining_resources"][0]["reclaim"], "docker image rm gpuworks/e2e-overrides:own")

    def test_manifest_cannot_attribute_an_exercise_to_another_source_file(self):
        manifest = json.loads((ROOT / "test/e2e/coverage/deployment-container-overrides.json").read_text())
        manifest["test_file"] = "test/e2e/deployment_wizard_create.py"
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "manifest.json"
            path.write_text(json.dumps(manifest))
            with patch.object(case.wizard.sys, "argv", ["case", "--artifact-dir", str(Path(directory) / "artifacts")]), \
                    patch.object(case.wizard.shutil, "which", return_value="docker"), \
                    patch.object(case.wizard.subprocess, "run", return_value=SimpleNamespace(returncode=0)):
                with self.assertRaisesRegex(AssertionError, "exercise source file"):
                    case.wizard.main(manifest_path=path, exercise=case.test_deployment_container_overrides,
                                     fixture_module=case.fixture_tools)

    def test_default_or_unset_docker_socket_is_rejected_before_server_or_image_start(self):
        for host in ("", " ", "/var/run/docker.sock", "/run/docker.sock", "unix:///var/run/docker.sock", "unix:///run/docker.sock", "unix:/var/run/docker.sock", "unix:///var/run/../run/docker.sock"):
            with self.subTest(host=host), patch.dict(os.environ, {"DOCKER_HOST": host}), patch.object(case.wizard, "main") as run:
                with self.assertRaisesRegex(AssertionError, "dedicated DOCKER_HOST"):
                    case.main()
                run.assert_not_called()

    def test_docker_context_cannot_override_the_dedicated_socket(self):
        for context in ("default", " "):
            with self.subTest(context=context), patch.dict(os.environ, {"DOCKER_HOST": "unix:///private/docker.sock", "DOCKER_CONTEXT": context}), \
                    patch.object(case.wizard, "main") as run:
                with self.assertRaisesRegex(AssertionError, "empty DOCKER_CONTEXT"):
                    case.main()
                run.assert_not_called()

    def test_default_socket_symlink_alias_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            alias = Path(directory) / "docker.sock"
            alias.symlink_to("/var/run/docker.sock")
            with patch.dict(os.environ, {"DOCKER_HOST": alias.as_uri().replace("file:", "unix:"), "DOCKER_CONTEXT": ""}), \
                    patch.object(case.wizard, "main") as run:
                with self.assertRaisesRegex(AssertionError, "dedicated DOCKER_HOST"):
                    case.main()
                run.assert_not_called()


if __name__ == "__main__":
    unittest.main()
