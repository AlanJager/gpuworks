"""Self-tests for the Playground chat gateway case's Docker classification."""
import unittest
import tempfile
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

import playground_chat_gateway as case


class DockerRunningTest(unittest.TestCase):
    def inspect(self, returncode, stdout="", stderr=""):
        return mock.Mock(returncode=returncode, stdout=stdout, stderr=stderr)

    def docker_call(self, returncode, stdout="", stderr=""):
        with mock.patch.object(case.subprocess, "run",
                               return_value=self.inspect(returncode, stdout, stderr)):
            return case.docker_running("container-id")

    def test_running_container_reports_true(self):
        self.assertEqual(self.docker_call(0, "true\n"), "true")

    def test_missing_container_is_absent(self):
        self.assertEqual(
            self.docker_call(1, "", "Error: No such object: container-id"), "absent")
        self.assertEqual(
            self.docker_call(1, "", "Error: no such container: container-id"), "absent")

    def test_daemon_error_is_not_absent(self):
        stderr = "Cannot connect to the Docker daemon at unix:///var/run/docker.sock"
        with self.assertRaises(AssertionError) as raised:
            self.docker_call(1, "", stderr)
        self.assertIn(stderr, str(raised.exception))

    def test_missing_socket_is_not_a_missing_container(self):
        with self.assertRaises(AssertionError):
            self.docker_call(1, "", "error connecting to socket: no such file or directory")

    def test_unexpected_success_output_is_not_a_container_state(self):
        with self.assertRaises(AssertionError):
            self.docker_call(0, "[]")

    def test_injected_error_is_not_absent(self):
        case.INJECT["docker_error"] = True
        try:
            with self.assertRaises(AssertionError):
                case.docker_running("container-id")
        finally:
            case.INJECT["docker_error"] = False

    def test_no_container_id_is_absent_without_calling_docker(self):
        with mock.patch.object(case.subprocess, "run") as run:
            self.assertEqual(case.docker_running(""), "absent")
        run.assert_not_called()


class ChatIsolationTest(unittest.TestCase):
    def test_new_chat_sends_exactly_one_user_prompt(self):
        valid = {"model": "served", "stream": True,
                 "messages": [{"role": "user", "content": "second"}]}
        case.check_new_chat_request(valid, "served", "second")
        for messages in ([{"role": "assistant", "content": "second"}],
                         valid["messages"] * 2,
                         [{"role": "system", "content": "unrelated history"}, *valid["messages"]]):
            with self.subTest(messages=messages), self.assertRaises(AssertionError):
                case.check_new_chat_request({**valid, "messages": messages}, "served", "second")


class CleanupOwnershipTest(unittest.TestCase):
    def test_failed_delete_retains_ids_and_still_stops_agent(self):
        report = {"owned_deployment_id": 3, "owned_model_id": 4,
                  "owned_node_uuid": "owned-node", "containers": []}
        agent = mock.Mock(pid=123)
        agent.poll.side_effect = [None, 0]
        def request(base, method, path, **kwargs):
            if method == "DELETE":
                raise TimeoutError("unreachable")
            return 200, {"instances": []}
        with mock.patch.object(case, "http", side_effect=request):
            self.assertFalse(case.cleanup(mock.Mock(base="unused"), "", report, {"agent": agent}))
        agent.terminate.assert_called_once()
        self.assertEqual(report["owned_deployment_id"], 3)
        self.assertEqual(report["owned_model_id"], 4)
        self.assertEqual(report["owned_node_uuid"], "owned-node")
        self.assertEqual({item["kind"] for item in report["remaining_resources"]},
                         {"deployment", "model", "node"})


class PreparationOwnershipTest(unittest.TestCase):
    def test_each_preparation_failure_preserves_allocated_ownership(self):
        for failure, expected in (("online", {"node"}), ("model", {"node"}),
                                  ("config", {"node", "model"}),
                                  ("deployment", {"node", "model", "deployment"})):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as directory:
                report, holder = {}, {}
                server = SimpleNamespace(base="unused", work=Path(directory))
                agent, artifacts = mock.Mock(), mock.Mock()
                def request(base, method, path, **kwargs):
                    if (method, path) == ("POST", case.NODES):
                        return 201, {"node": {"uuid": "owned-node"}, "token": "synthetic"}
                    if (method, path) == ("POST", case.MODELS + "/local"):
                        return 201, {"id": 4}
                    if (method, path) == ("PATCH", case.MODELS + "/4/inference-config"):
                        return 200, {}
                    if (method, path) == ("POST", case.DEPLOYMENTS):
                        return 201, {"id": 3}
                    raise AssertionError("unexpected fixture request")
                try:
                    with mock.patch.object(case, "http", side_effect=request), mock.patch.object(case.subprocess, "Popen", return_value=agent) as popen, mock.patch.object(case, "wait_online"):
                        with self.assertRaisesRegex(RuntimeError, "injected"):
                            case.create_fixtures(server, "", "run", report, holder, Path("/selected-agent"), artifacts,
                                                 **{f"inject_{failure}_failure": True})
                    self.assertIs(holder["agent"], agent)
                    self.assertEqual(popen.call_args.args[0][0], "/selected-agent")
                    observed = {kind for kind, key in (("node", "owned_node_uuid"), ("model", "owned_model_id"), ("deployment", "owned_deployment_id")) if report.get(key)}
                    self.assertEqual(observed, expected)
                    artifacts.remember.assert_called_with("synthetic")
                finally:
                    if holder.get("log"):
                        holder["log"].close()

    def test_failed_deployment_delete_still_reclaims_its_known_container(self):
        report = {"owned_deployment_id": 3}
        order = []
        agent = mock.Mock(pid=123)
        agent.poll.side_effect = [None, 0]
        agent.terminate.side_effect = lambda: order.append("agent-stop")
        def request(base, method, path, **kwargs):
            if method == "DELETE":
                raise TimeoutError("before sending")
            return 200, {"instances": [{"container_id": "owned-container"}]}
        def remove(argv, **kwargs):
            order.append("docker-remove")
            self.assertEqual(argv, ["docker", "rm", "-f", "owned-container"])
            return SimpleNamespace(returncode=0)
        with mock.patch.object(case, "http", side_effect=request), mock.patch.object(case, "docker_running", side_effect=["true", "absent"]), mock.patch.object(case.subprocess, "run", side_effect=remove):
            self.assertFalse(case.cleanup(SimpleNamespace(base="unused"), "", report, {"agent": agent}))
        self.assertEqual(order, ["agent-stop", "docker-remove"])
        self.assertEqual(report["cleanup_containers"], {"owned-container": "absent"})
        self.assertEqual(report["owned_deployment_id"], 3)
        self.assertNotIn("container", {item["kind"] for item in report["remaining_resources"]})


if __name__ == "__main__":
    unittest.main()
