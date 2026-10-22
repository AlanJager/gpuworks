"""Self-tests for the sleep/wake case's Docker classification."""
import unittest
from unittest import mock

import deployment_sleep_wake as case


class DockerRunningTest(unittest.TestCase):
    def inspect(self, returncode, stdout="", stderr=""):
        return mock.Mock(returncode=returncode, stdout=stdout, stderr=stderr)

    def docker_call(self, returncode, stdout="", stderr=""):
        with mock.patch.object(case.subprocess, "run",
                               return_value=self.inspect(returncode, stdout, stderr)):
            return case.docker_running("container-id")

    def test_running_container_reports_true(self):
        self.assertEqual(self.docker_call(0, "true\n"), "true")

    def test_missing_container_or_object_is_absent(self):
        for stderr in ("Error: No such container: container-id",
                       "Error response from daemon: No such object: container-id",
                       "error: no such container: container-id"):
            self.assertEqual(self.docker_call(1, "", stderr), "absent")

    def test_dns_no_such_host_is_not_absent(self):
        stderr = ('error during connect: Get "http://docker.invalid/v1.24/containers/'
                  'container-id/json": dial tcp: lookup docker.invalid: no such host')
        with self.assertRaises(AssertionError) as raised:
            self.docker_call(1, "", stderr)
        self.assertIn("no such host", str(raised.exception))

    def test_daemon_error_is_not_absent(self):
        stderr = "Cannot connect to the Docker daemon at unix:///var/run/docker.sock"
        with self.assertRaises(AssertionError) as raised:
            self.docker_call(1, "", stderr)
        self.assertIn(stderr, str(raised.exception))

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

    def test_missing_a_different_target_is_not_absent(self):
        with self.assertRaises(AssertionError):
            self.docker_call(1, "", "Error: No such object: unrelated-container")

    def test_success_without_a_running_boolean_is_an_error(self):
        for stdout in ("", "null\n", "not-a-boolean\n"):
            with self.subTest(stdout=stdout), self.assertRaises(AssertionError):
                self.docker_call(0, stdout)


class PreparationTest(unittest.TestCase):
    def test_failure_after_agent_start_keeps_handles_identity_and_redaction(self):
        import tempfile
        from pathlib import Path
        from types import SimpleNamespace
        report, holder, secrets = {}, {}, []
        agent = mock.Mock(pid=101)
        with tempfile.TemporaryDirectory() as tmp:
            server = SimpleNamespace(base="http://local", work=Path(tmp))
            with mock.patch.object(case, "http", return_value=(201, {
                    "node": {"uuid": "owned-node"}, "token": "synthetic-node-token"})), \
                    mock.patch.object(case.subprocess, "Popen", return_value=agent) as start, \
                    mock.patch.object(case, "wait_online", return_value=None):
                with self.assertRaisesRegex(RuntimeError, "model registration failure"):
                    case.create_fixtures(server, "token", "run", report, holder,
                                         Path("/requested/agent"), True, secrets.append)
            self.assertEqual(report["owned_node_uuid"], "owned-node")
            self.assertIs(holder["agent"], agent)
            self.assertEqual(start.call_args.args[0][0], "/requested/agent")
            self.assertEqual(secrets, ["synthetic-node-token"])
            holder["log"].close()


if __name__ == "__main__":
    unittest.main()
