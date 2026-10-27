"""Offline resource and failure regressions for deployment container acceptance."""
from types import SimpleNamespace
import subprocess
import unittest
from unittest import mock

import deployment_container_fixture as fixture


class DeploymentCleanupTest(unittest.TestCase):
    def test_revoked_key_can_remain_in_the_audit_list(self):
        report = {"owned_key_name": "owned-key"}
        revoked = [False]

        def request(base, method, path, **kwargs):
            if method == "GET" and path == fixture.APIKEYS:
                item = {"name": "owned-key", "prefix": "owned-prefix"}
                if revoked[0]:
                    item["revoked_at"] = "2026-10-26T06:00:00Z"
                return 200, {"items": [item]}
            if method == "DELETE" and path == fixture.APIKEYS + "/owned-prefix":
                revoked[0] = True
                return 200, {"status": "revoked"}
            raise AssertionError(f"unexpected request {method} {path}")

        self.assertTrue(fixture.cleanup(SimpleNamespace(base="http://local"), "token", report,
                                        {}, request=request, inspect=mock.Mock()))
        self.assertIsNone(report["owned_key_name"])
        self.assertEqual(report["remaining_resources"], [])
        self.assertTrue(report["cleanup_key_revoked"])
        self.assertFalse(report["cleanup_key_gone"])

    def test_invalid_revocation_marker_retains_key_ownership(self):
        for marker in (None, "", True, "yesterday"):
            with self.subTest(marker=marker):
                report = {"owned_key_name": "owned-key"}

                def request(base, method, path, **kwargs):
                    if method == "GET" and path == fixture.APIKEYS:
                        return 200, {"items": [{"name": "owned-key", "prefix": "owned-prefix",
                                                "revoked_at": marker}]}
                    if method == "DELETE" and path == fixture.APIKEYS + "/owned-prefix":
                        return 200, {"status": "revoked"}
                    raise AssertionError(f"unexpected request {method} {path}")

                self.assertFalse(fixture.cleanup(SimpleNamespace(base="http://local"), "token", report,
                                                 {}, request=request, inspect=mock.Mock()))
                self.assertEqual(report["owned_key_name"], "owned-key")
                self.assertEqual(report["remaining_resources"][0]["kind"], "inference-key")

    def test_preparing_container_is_observed_before_a_delete_failure(self):
        report = {"owned_deployment_id": 7, "owned_node_uuid": "owned-node"}
        agent = mock.Mock(pid=101)
        running = [True]
        agent.poll.side_effect = lambda: None if running[0] else 0
        agent.terminate.side_effect = lambda: running.__setitem__(0, False)

        def request(base, method, path, **kwargs):
            if path == fixture.DEPLOYMENTS + "/7":
                if method == "DELETE":
                    raise TimeoutError("deployment DELETE was not sent")
                return 200, {"instances": [{"state": "starting", "container_id": "owned-container"}]}
            if path.startswith(fixture.NODES + "/owned-node"):
                return (200, {}) if method == "DELETE" else (404, {})
            raise AssertionError(f"unexpected request {method} {path}")

        inspector = mock.Mock(return_value="absent")
        ok = fixture.cleanup(SimpleNamespace(base="http://local"), "token", report,
                             {"agent": agent, "log": None}, request=request, inspect=inspector)
        self.assertFalse(ok)
        self.assertEqual(report["owned_deployment_id"], 7)
        self.assertEqual(report["containers"], ["owned-container"])
        self.assertEqual(report["cleanup_containers"], {"owned-container": "absent"})
        self.assertTrue(report["agent_stopped"])
        self.assertTrue(report["cleanup_node_gone"])
        self.assertEqual(report["remaining_resources"][0]["id"], 7)
        self.assertIn("deployment delete", report["cleanup_failed_stages"])

    def test_delete_200_does_not_prove_model_or_key_disappeared(self):
        report = {"owned_model_id": 9, "owned_key_name": "owned-key"}

        def request(base, method, path, **kwargs):
            if path == fixture.APIKEYS:
                return 200, {"items": [{"name": "owned-key", "prefix": "owned-prefix"}]}
            if path == fixture.APIKEYS + "/owned-prefix":
                return 200, {}
            if path == fixture.MODELS + "/9":
                return 200, {"id": 9}
            raise AssertionError(f"unexpected request {method} {path}")

        ok = fixture.cleanup(SimpleNamespace(base="http://local"), "token", report,
                             {}, request=request, inspect=mock.Mock())
        self.assertFalse(ok)
        self.assertEqual(report["owned_model_id"], 9)
        self.assertEqual(report["owned_key_name"], "owned-key")
        self.assertEqual({entry["kind"] for entry in report["remaining_resources"]},
                         {"model", "inference-key"})
        self.assertEqual(report["owned_key_prefix"], "owned-prefix")

    def test_node_timeout_retains_its_scoped_reclaim_entry(self):
        report = {"owned_node_uuid": "owned-node"}
        request = mock.Mock(side_effect=TimeoutError("node read failed"))
        self.assertFalse(fixture.cleanup(SimpleNamespace(base="http://local"), "token", report,
                                         {}, request=request, inspect=mock.Mock()))
        self.assertEqual(report["owned_node_uuid"], "owned-node")
        self.assertEqual(report["remaining_resources"], [{
            "kind": "node", "id": "owned-node", "state": "unknown (query failed)",
            "reclaim": "DELETE /api/v1/nodes/owned-node?force=true"}])

    def test_inspection_failure_keeps_container_residual(self):
        report = {"containers": ["owned-container"]}
        inspector = mock.Mock(side_effect=AssertionError("daemon unavailable"))
        self.assertFalse(fixture.cleanup(SimpleNamespace(base="http://local"), "token", report,
                                         {}, request=mock.Mock(), inspect=inspector))
        self.assertEqual(report["remaining_resources"][0]["id"], "owned-container")
        self.assertEqual(report["remaining_resources"][0]["kind"], "container")


class DockerDeadlineTest(unittest.TestCase):
    def test_probe_has_a_deadline_and_timeout_is_not_absence(self):
        with mock.patch.object(fixture.subprocess, "run",
                side_effect=subprocess.TimeoutExpired(["docker", "inspect"], 20)) as run:
            with self.assertRaises(subprocess.TimeoutExpired):
                fixture.docker_running("owned-container")
        run.assert_called_once_with(
            ["docker", "inspect", "--format", "{{.State.Running}}", "owned-container"],
            capture_output=True, text=True, timeout=20)

    def test_real_probe_timeout_keeps_residual_and_still_stops_agent(self):
        report = {"containers": ["owned-container"]}
        agent = mock.Mock(pid=101)
        running = [True]
        agent.poll.side_effect = lambda: None if running[0] else 0
        agent.terminate.side_effect = lambda: running.__setitem__(0, False)
        with mock.patch.object(fixture.subprocess, "run",
                side_effect=subprocess.TimeoutExpired(["docker", "inspect"], 20)):
            okay = fixture.cleanup(SimpleNamespace(base="http://local"), "token", report,
                {"agent": agent}, request=mock.Mock(), inspect=fixture.docker_running)
        self.assertFalse(okay)
        self.assertTrue(report["agent_stopped"])
        agent.terminate.assert_called_once()
        self.assertEqual(report["remaining_resources"], [{"kind": "container", "id": "owned-container",
            "state": "unknown (query failed)", "reclaim": "docker rm -f owned-container"}])


class GatewayOutputTest(unittest.TestCase):
    def test_a_200_with_only_the_word_choices_is_not_inference(self):
        response = mock.Mock(status=200)
        response.json.return_value = {"error": "choices unavailable"}
        with self.assertRaises(AssertionError):
            fixture.assert_chat_response(response)

    def test_fake_engine_content_must_be_recovered(self):
        response = mock.Mock(status=200)
        response.json.return_value = {
            "object": "chat.completion", "choices": [{"index": 0,
            "message": {"role": "assistant", "content": "garbage"}, "finish_reason": "stop"}]}
        with self.assertRaises(AssertionError):
            fixture.assert_chat_response(response)
        response.json.return_value["choices"][0]["message"]["content"] = "hello from fake-engine"
        fixture.assert_chat_response(response)


if __name__ == "__main__":
    unittest.main()
