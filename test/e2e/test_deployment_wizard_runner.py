"""Drive the actual wizard main lifecycle with failing external boundaries.

Run: PYTHONPATH=test/e2e python3 -m unittest test_deployment_wizard_runner -v
"""
import contextlib
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import MagicMock, patch
from types import SimpleNamespace

import deployment_wizard_create as case
from local_browser_fixture import Artifacts as RealArtifacts


class MainLifecycle(unittest.TestCase):
    def run_main(self, *, fail_write=0, context_error=False, browser_error=False,
                 business_error=False, screenshot_error=False, context_setup_error=False, server_stop="ok", admin_token="fixture-admin-secret", override=False, image_cleanup=None, cleanup_exception=None, agent_stop_error=False, lost_deployment_id=False):
        business_module, boundary, entry = case, case.wizard_fixture, case.main
        function_name = "test_deployment_wizard_create"
        if override:
            import deployment_override_create
            import deployment_override_fixture
            business_module, boundary, entry = deployment_override_create, deployment_override_fixture, deployment_override_create.main
            function_name = "test_deployment_container_overrides"
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            binary = root / "binary"
            binary.write_bytes(b"fixture binary")
            output = root / "artifacts"
            server, agent, browser, context, page, play = [MagicMock() for _ in range(6)]
            server.base = "http://127.0.0.1:8800"
            agent.pid = 12345
            agent.poll.return_value = None
            agent.terminate.side_effect = lambda: setattr(agent.poll, "return_value", 0)
            if agent_stop_error:
                agent.terminate.side_effect = RuntimeError("agent terminate failed")
            server.process = SimpleNamespace(pid=54321)
            server.start.return_value = {"ok": True}
            server.stop.return_value = server_stop != "false"
            if server_stop == "raise":
                server.stop.side_effect = RuntimeError("injected server stop failure")
            browser.version = "fixture-chrome"
            browser.new_context.return_value = context
            context.new_page.return_value = page
            play.chromium.launch.return_value = browser
            if context_error:
                context.close.side_effect = RuntimeError("injected context close failure")
            if browser_error:
                browser.close.side_effect = RuntimeError("injected browser close failure")
            if context_setup_error:
                browser.new_context.side_effect = RuntimeError("injected context creation failure")
            if screenshot_error:
                page.screenshot.side_effect = OSError("injected screenshot write failure")
            playwright = MagicMock()
            playwright.__enter__.return_value = play

            class Artifacts(RealArtifacts):
                writes = 0
                def write_json(self, name, value):
                    self.writes += 1
                    if self.writes == fail_write:
                        raise OSError("injected report write failure")
                    super().write_json(name, value)

            opened_logs = []
            def prepare(server, token, run_id, report, holder, agent_bin, remember):
                holder["agent"] = agent
                holder["log"] = (server.work / "agent.log").open("w")
                opened_logs.append(holder["log"])
                (server.work / "server.log").write_text("fixture server log")
                report["owned_node_uuid"] = "run-owned-node"
                if lost_deployment_id:
                    report["owned_deployment_name"] = "e2e-owned-deployment"
                if image_cleanup:
                    report["owned_override_image"] = "gpuworks/e2e-overrides:own"
                    report["owned_override_source_tags"] = ["gpuworks/e2e-overrides:own-source-0", "gpuworks/e2e-overrides:own-source-1"]
                return {}

            def cleanup(server, token, report, holder):
                if cleanup_exception:
                    raise cleanup_exception("fixture cleanup aborted")
                if holder.get("agent") is not None:
                    self.assertIs(holder["agent"], agent)
                    agent.terminate()
                if holder.get("log") is not None:
                    holder["log"].close()
                report["agent_stopped"] = True
                report["remaining_resources"] = []
                return True

            def business(page, context, server, artifacts, report, locale, fixture, token):
                if business_error:
                    raise AssertionError("primary business failure")
                report["evidence"] = [{"layer": name} for name in
                    ("ui", "request", "persistence", "execution", "cleanup")]
            business.__name__ = function_name
            business.__code__ = business.__code__.replace(co_filename=business_module.__file__)

            def command(args, **kwargs):
                if args[:2] == ["git", "status"]:
                    return b""
                return "sha256:fixture\n" if args[0] == "docker" else "b40eddfc0\n"

            attempted_images = []
            def run_command(args, **kwargs):
                if image_cleanup and args[:3] == ["docker", "image", "rm"]:
                    attempted_images.append(args[-1])
                    return SimpleNamespace(returncode=0, stdout="", stderr="")
                if image_cleanup and args[:3] == ["docker", "image", "inspect"] and args[-1].startswith("gpuworks/e2e-overrides:"):
                    if args[-1] == "gpuworks/e2e-overrides:own":
                        if image_cleanup == "timeout":
                            raise TimeoutError("image inspect timed out")
                        if image_cleanup == "present":
                            return SimpleNamespace(returncode=0, stdout="[{}]", stderr="")
                        return SimpleNamespace(returncode=1, stdout="", stderr="Cannot connect to Docker daemon")
                    return SimpleNamespace(returncode=1, stdout="", stderr="No such image: " + args[-1])
                return SimpleNamespace(returncode=0, stdout="", stderr="")

            previous_umask = os.umask(0o077)
            try:
                def local_server(binary, work):
                    server.work = work
                    return server
                actual_cleanup = boundary.cleanup
                resource_cleanup = patch.object(boundary.wizard, "cleanup", side_effect=cleanup) if image_cleanup else contextlib.nullcontext()
                with patch.dict(os.environ, {"DOCKER_HOST": "unix:///private-fixture/docker.sock", "DOCKER_CONTEXT": ""}), resource_cleanup, patch.object(case, "Artifacts", Artifacts), patch.object(case, "LocalServer", side_effect=local_server), \
                        patch.object(case, "http", return_value=(200, {"token": admin_token})), \
                        patch.object(boundary, "prepare", side_effect=prepare), \
                        patch.object(boundary, "cleanup", side_effect=actual_cleanup if image_cleanup else cleanup) as reclaimed, \
                        patch.object(business_module, function_name, business), \
                        patch.object(case, "sync_playwright", return_value=playwright), \
                        patch.object(case.shutil, "which", return_value="/fixture/docker"), \
                        patch.object(case.subprocess, "run", side_effect=run_command), \
                        patch.object(case.subprocess, "check_output", side_effect=command), \
                        patch.object(case.sys, "argv", ["case", "--server-bin", str(binary),
                            "--agent-bin", str(binary), "--artifact-dir", str(output)]), \
                        contextlib.redirect_stdout(io.StringIO()):
                    result = entry()
            finally:
                os.umask(previous_umask)
            report = json.loads((output / "report.json").read_text())
            reclaimed.assert_called_once()
            server.stop.assert_called_once()
            for log in opened_logs:
                self.assertTrue(log.closed, "agent log must close even when cleanup callback fails")
            if isinstance(admin_token, str) and admin_token.strip():
                agent.terminate.assert_called_once()
                browser.close.assert_called_once()
                if not context_setup_error:
                    context.close.assert_called_once()
            else:
                agent.terminate.assert_not_called()
                browser.close.assert_not_called()
            if server_stop != "ok":
                self.assertEqual((output / "server.log").read_text(), "fixture server log")
            if image_cleanup:
                self.assertEqual(attempted_images, ["gpuworks/e2e-overrides:own", "gpuworks/e2e-overrides:own-source-1", "gpuworks/e2e-overrides:own-source-0"])
            return result, report

    def test_baseline_runs_actual_main_and_closes_every_boundary(self):
        result, report = self.run_main()
        self.assertEqual(result, 0)
        self.assertEqual(report["status"], "PASS")
        self.assertTrue(report["browser_close_ok"])
        self.assertTrue(report["environment_cleanup_ok"])

    def test_close_errors_keep_nonpass_and_all_other_cleanup_runs(self):
        for context_error, browser_error in [(True, False), (False, True), (True, True)]:
            with self.subTest(context=context_error, browser=browser_error):
                result, report = self.run_main(context_error=context_error, browser_error=browser_error)
                self.assertEqual(result, 1)
                self.assertEqual(report["status"], "FAIL_TEST")
                self.assertFalse(report["browser_close_ok"])
                self.assertFalse(report["environment_cleanup_ok"])

    def test_report_write_failures_cannot_skip_context_browser_agent_or_server_close(self):
        # 1 = first report after preparation; 2 = first write inside browser finally.
        for write in (1, 2):
            with self.subTest(write=write):
                result, report = self.run_main(fail_write=write)
                self.assertEqual(result, 1)
                self.assertEqual(report["status"], "FAIL_TEST")
                self.assertIn("injected report write failure", report["failure"])
                self.assertTrue(report["browser_close_ok"])
                self.assertEqual(report["remaining_resources"], [])

    def test_secondary_report_and_screenshot_failures_preserve_first_business_failure(self):
        result, report = self.run_main(business_error=True, fail_write=2, screenshot_error=True)
        self.assertEqual(result, 1)
        self.assertIn("primary business failure", report["failure"])
        self.assertEqual([item["stage"] for item in report["artifact_errors"]], ["report.json", "failure.png"])

    def test_malformed_admin_token_cannot_poison_failure_artifacts(self):
        for token in (True, 1, {}, [], None, "", " "):
            with self.subTest(token=token):
                result, report = self.run_main(admin_token=token)
                self.assertEqual(result, 1)
                self.assertNotEqual(report["status"], "PASS")
                self.assertIn("no valid token", report["failure"])
                self.assertEqual(report["remaining_resources"], [])

    def test_failed_server_stop_retains_pid_and_scoped_reclaim(self):
        for result_kind in ("false", "raise"):
            with self.subTest(stop=result_kind):
                result, report = self.run_main(server_stop=result_kind)
                self.assertEqual(result, 1)
                self.assertEqual(report["status"], "FAIL_TEST")
                self.assertEqual(report["server_pid"], 54321)
                self.assertEqual(report["remaining_resources"], [{"kind": "server", "id": 54321,
                    "state": "not confirmed stopped", "reclaim": "kill 54321"}])

    def test_json_contract_rejects_boolean_replaced_by_integer(self):
        with self.assertRaisesRegex(AssertionError, "wire types"):
            case.assert_json({"quick_params": {"enable_cache_report": 1}},
                             {"quick_params": {"enable_cache_report": True}}, "wire types")
        case.assert_json({"quick_params": {"enable_cache_report": True}},
                         {"quick_params": {"enable_cache_report": True}}, "wire types")

    def test_failed_context_creation_still_closes_the_launched_browser(self):
        result, report = self.run_main(context_setup_error=True)
        self.assertEqual(result, 1)
        self.assertEqual(report["status"], "FAIL_TEST")
        self.assertIn("injected context creation failure", report["failure"])

    def test_cleanup_callback_exception_still_stops_agent_and_retains_owned_identity(self):
        for failure in (RuntimeError, TimeoutError):
            with self.subTest(failure=failure):
                result, report = self.run_main(cleanup_exception=failure)
                self.assertEqual(result, 1)
                self.assertEqual(report["status"], "FAIL_TEST")
                self.assertTrue(report["agent_stopped"])
                self.assertTrue(report["server_stopped"])
                self.assertTrue(report["browser_close_ok"])
                self.assertEqual(report["remaining_resources"], [{"kind": "node", "id": "run-owned-node",
                    "state": "unknown (cleanup callback failed)", "reclaim": "DELETE /api/v1/nodes/run-owned-node?force=true"}])

    def test_cleanup_exception_and_agent_stop_failure_never_claims_no_process_kept(self):
        result, report = self.run_main(cleanup_exception=RuntimeError, agent_stop_error=True)
        self.assertEqual(result, 1)
        self.assertFalse(report["agent_stopped"])
        self.assertEqual(report["remaining_resources"][-1], {"kind": "agent", "id": 12345,
            "state": "not confirmed stopped", "reclaim": "kill 12345"})
        self.assertNotIn("no process kept", report["private_state"])

    def test_unreadable_create_id_retains_run_owned_deployment_name_on_cleanup_exception(self):
        result, report = self.run_main(cleanup_exception=RuntimeError, lost_deployment_id=True)
        self.assertEqual(result, 1)
        item = report["remaining_resources"][0]
        self.assertEqual(item["kind"], "deployment-name")
        self.assertEqual(item["id"], "e2e-owned-deployment")
        self.assertEqual(item["reclaim"], "GET /api/v1/deployments?q=e2e-owned-deployment; DELETE only the id with this exact name")


if __name__ == "__main__":
    unittest.main()
