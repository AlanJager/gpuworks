"""Drive the actual replica main lifecycle with failing external boundaries.

Run: PYTHONPATH=test/e2e python3 -m unittest test_deployment_replica_runner -v
"""
import contextlib
import io
import json
import os
import subprocess
from pathlib import Path
import tempfile
import unittest
from unittest.mock import MagicMock, patch
from types import SimpleNamespace

import deployment_replicas as case
from local_browser_fixture import Artifacts as RealArtifacts


class MainLifecycle(unittest.TestCase):
    def run_main(self, *, fail_write=0, context_error=False, browser_error=False,
                 business_error=False, screenshot_error=False, context_setup_error=False, server_stop="ok", admin_token="fixture-admin-secret", stream_stop=True, prepare_error=False, discovery_error=False, cleanup_error=False, agent_stop_error=False, image_cleanup_timeout=False):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            binary = root / "binary"
            binary.write_bytes(b"fixture binary")
            output = root / "artifacts"
            server, agent, browser, context, page, play = [MagicMock() for _ in range(6)]
            agent.pid = 54322
            agent.poll.return_value = None
            def terminate():
                if agent_stop_error:
                    raise OSError("injected agent stop failure")
                agent.poll.return_value = 0
            agent.terminate.side_effect = terminate
            server.base = "http://127.0.0.1:8800"
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

            def prepare(server, token, run_id, report, holder, agent_bin, remember, replicas):
                holder["agent"] = agent
                holder["stream"] = MagicMock()
                holder["stream"].stop.return_value = stream_stop
                holder["stream"].status = 200
                holder["stream"].lines = ["data: synthetic-first"]
                holder["stream"].error = None
                (server.work / "server.log").write_text("fixture server log")
                report["owned_node_uuid"] = "run-owned-node"
                if image_cleanup_timeout:
                    report["owned_image_tag"] = "gpuworks/e2e-replicas:unit-owned"
                if prepare_error:
                    report["owned_deployment_name"] = "e2e-own-deployment"
                    raise TimeoutError("injected create response lost")
                return {}

            def cleanup(server, token, report, holder, **kwargs):
                if cleanup_error:
                    raise TimeoutError("injected cleanup timeout")
                if holder.get("agent") is not None:
                    self.assertIs(holder["agent"], agent)
                    agent.terminate()
                report["agent_stopped"] = True
                report["remaining_resources"] = []
                report["cleanup_steps"] = [{"stage": "owned resource cleanup", "ok": True}]
                return True

            def business(page, context, server, artifacts, report, locale, fixture, token, holder, selected):
                if business_error:
                    raise AssertionError("primary business failure")
                report["evidence"] = [{"layer": name} for name in
                    ("ui", "request", "persistence", "execution", "cleanup")]
            business.__name__ = "test_replicas"
            # Simulate the bound case definition while exercising real main.
            business.__code__ = business.__code__.replace(
                co_filename=str(case.ROOT / "test/e2e/deployment_replica_browser.py"))

            def command(args, **kwargs):
                if args[:2] == ["docker", "info"]:
                    return '{"id":"fixture","root":"fixture-root","name":"fixture","driver":"fixture"}'
                if args[:2] == ["git", "status"]:
                    return b""
                return "sha256:fixture\n" if args[0] == "docker" else "b40eddfc0\n"

            def run_command(args, **kwargs):
                if image_cleanup_timeout and kwargs.get("text") and args[:3] == ["docker", "image", "inspect"]:
                    raise subprocess.TimeoutExpired(args, kwargs.get("timeout"))
                return MagicMock(returncode=0)

            previous_umask = os.umask(0o077)
            try:
                def local_server(binary, work):
                    server.work = work
                    return server
                with patch.dict(case.os.environ, {"DOCKER_HOST": "unix:///tmp/replica-unit-docker.sock", "DOCKER_CONTEXT": ""}), \
                        patch.object(case, "Artifacts", Artifacts), patch.object(case, "LocalServer", side_effect=local_server), \
                        patch.object(case, "http", return_value=(200, {"token": admin_token})), \
                        patch.object(case.replica_fixture, "create", side_effect=prepare), \
                        patch.object(case.replica_fixture, "http", side_effect=(TimeoutError("injected discovery failure")
                            if discovery_error else lambda *a, **k: (200, {"items": [{"id": 73, "name": "e2e-own-deployment"}]}))), \
                        patch.object(case.container_fixture, "cleanup", side_effect=cleanup) as reclaimed, \
                        patch.object(case, "test_replicas", business), \
                        patch.object(case, "sync_playwright", return_value=playwright), \
                        patch.object(case.shutil, "which", return_value="/fixture/docker"), \
                        patch.object(case.subprocess, "run", side_effect=run_command), \
                        patch.object(case.replica_fixture, "docker", return_value=""), \
                        patch.object(case.subprocess, "check_output", side_effect=command), \
                        patch.object(case.sys, "argv", ["case", "--case", "scale", "--server-bin", str(binary),
                            "--agent-bin", str(binary), "--artifact-dir", str(output)]), \
                        contextlib.redirect_stdout(io.StringIO()):
                    result = case.main()
            finally:
                os.umask(previous_umask)
            report = json.loads((output / "report.json").read_text())
            reclaimed.assert_called_once()
            server.stop.assert_called_once()
            if isinstance(admin_token, str) and admin_token.strip():
                agent.terminate.assert_called_once()
                if prepare_error:
                    browser.close.assert_not_called()
                else:
                    browser.close.assert_called_once()
                if not context_setup_error and not prepare_error:
                    context.close.assert_called_once()
            else:
                agent.terminate.assert_not_called()
                browser.close.assert_not_called()
            if server_stop != "ok":
                self.assertEqual((output / "server.log").read_text(), "fixture server log")
            return result, report

    def test_dedicated_daemon_guard_rejects_empty_and_shared_default_addresses(self):
        for host in (None, "", " ", "/var/run/docker.sock", "/run/docker.sock",
                     "unix:///var/run/docker.sock", "unix:///run/docker.sock", "unix:/run/docker.sock"):
            with self.subTest(host=host), self.assertRaises(AssertionError):
                case.require_dedicated_daemon(host)
        case.require_dedicated_daemon("unix:///tmp/own-docker/docker.sock")
        for context in ("default", "foreign"):
            with self.subTest(context=context), self.assertRaisesRegex(AssertionError, "DOCKER_CONTEXT"):
                case.require_dedicated_daemon("unix:///tmp/own-docker/docker.sock", context)

    def test_cleanup_timeout_keeps_ownership_and_independently_stops_agent(self):
        result, report = self.run_main(cleanup_error=True)
        self.assertEqual(result, 1)
        self.assertEqual(report["status"], "FAIL_TEST")
        self.assertTrue(report["agent_stopped"])
        self.assertTrue(report["server_stopped"])
        self.assertEqual(report["remaining_resources"][0]["kind"], "cleanup")
        self.assertEqual(report["remaining_resources"][0]["owned"]["owned_node_uuid"], "run-owned-node")

    def test_cleanup_timeout_and_agent_stop_error_keeps_agent_pid_residual(self):
        result, report = self.run_main(cleanup_error=True, agent_stop_error=True)
        self.assertEqual(result, 1)
        self.assertFalse(report["agent_stopped"])
        self.assertTrue(report["server_stopped"])
        self.assertIn({"kind": "agent", "id": 54322, "state": "not confirmed stopped", "reclaim": "kill 54322"},
                      report["remaining_resources"])

    def test_image_absence_inspect_timeout_preserves_scoped_tag_residual(self):
        result, report = self.run_main(image_cleanup_timeout=True)
        self.assertEqual(result, 1)
        self.assertEqual(report["status"], "FAIL_TEST")
        self.assertTrue(report["agent_stopped"])
        self.assertTrue(report["server_stopped"])
        self.assertEqual(report["owned_image_tag"], "gpuworks/e2e-replicas:unit-owned")
        self.assertIn("TimeoutExpired", report["image_cleanup_error"])
        self.assertEqual(report["remaining_resources"][0]["kind"], "docker-image-tag")

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

    def test_lost_create_response_recovers_exact_name_or_preserves_scoped_residual(self):
        for broken in (False, True):
            with self.subTest(discovery_error=broken):
                result, report = self.run_main(prepare_error=True, discovery_error=broken)
                self.assertEqual(result, 1)
                self.assertEqual(report["status"], "FAIL_TEST")
                self.assertIn("injected create response lost", report["failure"])
                self.assertTrue(report["server_stopped"])
                if broken:
                    self.assertEqual(report["remaining_resources"][0]["kind"], "deployment-name")
                    self.assertEqual(report["remaining_resources"][0]["id"], "e2e-own-deployment")
                    self.assertFalse(report["environment_cleanup_ok"])
                else:
                    self.assertEqual(report["owned_deployment_id"], 73)
                    self.assertEqual(report["remaining_resources"], [])

    def test_held_stream_retains_residual_and_other_boundaries_close(self):
        result, report = self.run_main(stream_stop=False)
        self.assertEqual(result, 1)
        self.assertEqual(report["status"], "FAIL_TEST")
        self.assertFalse(report["stream_stopped"])
        self.assertFalse(report["environment_cleanup_ok"])
        self.assertTrue(report["server_stopped"])
        self.assertEqual(report["remaining_resources"][0]["kind"], "stream")
        self.assertIn("this run", report["remaining_resources"][0]["reclaim"])

    def test_json_contract_rejects_boolean_replaced_by_integer(self):
        with self.assertRaisesRegex(AssertionError, "wire types"):
            case.replica_fixture.strict({"quick_params": {"enable_cache_report": 1}},
                             {"quick_params": {"enable_cache_report": True}}, "wire types")
        case.replica_fixture.strict({"quick_params": {"enable_cache_report": True}},
                         {"quick_params": {"enable_cache_report": True}}, "wire types")

    def test_failed_context_creation_still_closes_the_launched_browser(self):
        result, report = self.run_main(context_setup_error=True)
        self.assertEqual(result, 1)
        self.assertEqual(report["status"], "FAIL_TEST")
        self.assertIn("injected context creation failure", report["failure"])


if __name__ == "__main__":
    unittest.main()
