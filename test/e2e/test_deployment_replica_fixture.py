"""Protocol negatives for the replica browser's independent observations.

Run: PYTHONPATH=test/e2e python3 -m unittest test_deployment_replica_fixture -v
"""
import json
from pathlib import Path
import tempfile
import unittest
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest.mock import patch, MagicMock
from types import SimpleNamespace
import deployment_replica_fixture as fixture
from deployment_replica_browser import mutation_wire


class Protocol(unittest.TestCase):
    def complete(self):
        delta = {"id": "fake-engine-completion", "choices": [{"delta": {"content": fixture.CONTENT}}]}
        return ["data: " + json.dumps(delta)] * fixture.CHUNKS + [
            'data: {"choices":[],"usage":{"prompt_tokens":16,"completion_tokens":8,"total_tokens":24}}',
            "data: [DONE]"]

    def test_create_lost_response_keeps_name_registered_before_post(self):
        report = {"fake_engine_image_id": "sha256:fixture"}
        holder = {}
        def wire(base, method, path, token, body=None):
            if path == fixture.containers.NODES:
                return 201, {"node": {"uuid": "own-node"}, "token": "synthetic-node-secret"}
            if path == fixture.containers.MODELS + "/local":
                return 201, {"id": 8}
            if path == fixture.containers.APIKEYS:
                return 201, {"key": "synthetic-key-secret"}
            self.assertEqual(path, fixture.containers.DEPLOYMENTS)
            self.assertEqual(body["routing_policy"], "failover")
            self.assertEqual(report["owned_deployment_name"], body["name"])
            self.assertEqual(report["fixture_deployment_body"], body)
            raise TimeoutError("created but response lost")
        with tempfile.TemporaryDirectory() as directory, patch.object(fixture, "http", side_effect=wire), \
                patch.object(fixture, "docker", return_value="sha256:fixture"), \
                patch.object(fixture.subprocess, "Popen", return_value=MagicMock()), \
                patch.object(fixture.lifecycle, "wait_online"):
            try:
                with self.assertRaisesRegex(TimeoutError, "response lost"):
                    fixture.create(SimpleNamespace(base="http://fixture", work=Path(directory)), "admin", "own-run",
                                   report, holder, Path("agent"), lambda _: None, 1)
                self.assertEqual(report["owned_deployment_name"], "e2e-own-run-deployment")
                self.assertEqual(report["owned_node_uuid"], "own-node")
                self.assertEqual(report["owned_model_id"], 8)
                self.assertEqual(report["owned_image_tag"], "gpuworks/e2e-replicas:own-run")
            finally:
                holder["log"].close()

    def test_agent_log_registers_short_lived_and_overlapping_names(self):
        with tempfile.TemporaryDirectory() as directory:
            work = Path(directory)
            (work / "agent.log").write_text(
                'INFO args="[run -d --name gpuworks-123456789abc --restart no]"\n'
                'INFO args="[run -d --name gpuworks-abcdef123456 --restart no]"\n'
                'DEBUG args="[inspect foreign-container]"\n')
            report = {}
            fixture.agent_inventory(SimpleNamespace(work=work), report)
            self.assertEqual(report["containers"], ["gpuworks-123456789abc", "gpuworks-abcdef123456"])
            self.assertEqual(report["agent_log_container_names"], sorted(report["containers"]))

    def test_inventory_excludes_other_tags_of_same_image_id(self):
        calls = []
        def observed(*args):
            calls.append(args)
            if args[0] == "ps":
                return "own-container\nforeign-container\n"
            return json.dumps([{"Config": {"Image": "gpuworks/e2e-replicas:own" if args[1] == "own-container" else "gpuworks/fake-engine:test"}}])
        report = {"owned_image_tag": "gpuworks/e2e-replicas:own"}
        with patch.object(fixture, "docker", side_effect=observed):
            fixture.inventory(report)
        self.assertEqual(report["containers"], ["own-container"])
        self.assertIn(("inspect", "foreign-container"), calls)

    def test_actual_listener_extractor_covers_collection_and_descendant_mutations(self):
        for method in ("POST", "PUT", "PATCH", "DELETE"):
            for path in ("/api/v1/deployments", "/api/v1/deployments/1/replicas", "/api/v1/instances", "/api/v1/instances/own/restart"):
                with self.subTest(method=method, path=path):
                    request = SimpleNamespace(method=method, url="http://fixture" + path + "?drain=false",
                        post_data='{"replicas":2}', post_data_json={"replicas": 2})
                    self.assertEqual(mutation_wire(request), {"method": method, "path": path,
                        "query": "drain=false", "body": {"replicas": 2}})
        self.assertIsNone(mutation_wire(SimpleNamespace(method="GET", url="http://fixture/api/v1/deployments")))
        self.assertIsNone(mutation_wire(SimpleNamespace(method="POST", url="http://fixture/api/v1/deployments-other")))

    def test_final_wire_ledger_rejects_delayed_extra_mutation(self):
        expected = [{"method": "PUT", "body": {"replicas": 2}}]
        with self.assertRaisesRegex(AssertionError, "final ledger"):
            fixture.strict(expected + [{"method": "PUT", "body": {"replicas": 1}}], expected, "final ledger")

    def test_running_inventory_filters_same_image_foreign_tags(self):
        def observed(*args):
            if args[0] == "ps":
                return "own\nforeign\n"
            return json.dumps([{"Id": args[1], "Config": {"Image": "own-tag" if args[1] == "own" else "foreign-tag"}}])
        report = {"owned_image_tag": "own-tag"}
        with patch.object(fixture, "docker", side_effect=observed):
            self.assertEqual(fixture.running_containers(report), ["own"])
        self.assertEqual(report["containers"], ["own"])

    def test_failover_counterfactual_rejects_natural_idle_peer_and_admitted_draining_target(self):
        before = {"target": 1, "peer": 0}
        fixture.assert_counter_delta(before, {"target": 2, "peer": 0}, "target", "peer", 1, 0, "pre-drain")
        fixture.assert_counter_delta(before, {"target": 1, "peer": 1}, "target", "peer", 0, 1, "post-drain")
        with self.assertRaisesRegex(AssertionError, "pre-drain"):
            fixture.assert_counter_delta(before, {"target": 1, "peer": 1}, "target", "peer", 1, 0, "pre-drain")
        with self.assertRaisesRegex(AssertionError, "draining exclusion"):
            fixture.assert_counter_delta(before, {"target": 2, "peer": 0}, "target", "peer", 0, 1, "draining exclusion")

    def test_no_surge_guard_rejects_ready_replacement_and_third_real_container(self):
        target = {"uuid": "target", "container_id": "target-container"}
        peer = {"uuid": "peer", "container_id": "peer-container"}
        document = {"routing_policy": "failover", "replicas": 2, "ready_replicas": 1,
                    "instances": [{"uuid": "target", "state": "draining"}, {"uuid": "peer", "state": "running"}]}
        fixture.assert_no_surge(document, target, peer, ["target-container", "peer-container"])
        with self.assertRaisesRegex(AssertionError, "Docker running container set"):
            fixture.assert_no_surge(document, target, peer, ["target-container", "peer-container", "replacement-container"])
        replaced = {**document, "instances": [*document["instances"], {"uuid": "replacement", "state": "running"}]}
        with self.assertRaisesRegex(AssertionError, "replacement must not become running"):
            fixture.assert_no_surge(replaced, target, peer, ["target-container", "peer-container"])
        with self.assertRaisesRegex(AssertionError, "failover routing"):
            fixture.assert_no_surge({**document, "routing_policy": "least_request"}, target, peer, ["target-container", "peer-container"])

    def test_exact_stream_happy_path(self):
        fixture.assert_complete(self.complete())

    def test_missing_partial_duplicate_and_wrong_usage_fail(self):
        lines = self.complete()
        for broken in (lines[:-1], lines[1:], lines + [lines[-1]],
                       [*lines[:-2], lines[-2].replace('"total_tokens":24', '"total_tokens":999'), lines[-1]]):
            with self.subTest(events=len(broken)), self.assertRaises(AssertionError):
                fixture.assert_complete(broken)

    def test_stream_stop_interrupts_real_held_socket_read(self):
        release = threading.Event()
        class Held(BaseHTTPRequestHandler):
            def do_POST(self):
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                self.wfile.write(b'data: {"first":true}\n\n')
                self.wfile.flush()
                release.wait(10)
            def log_message(self, *_):
                pass
        server = ThreadingHTTPServer(("127.0.0.1", 0), Held)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        stream = fixture.Stream(f"http://127.0.0.1:{server.server_port}", "synthetic-key", "synthetic-model")
        try:
            self.assertTrue(stream.first.wait(3))
            self.assertFalse(stream.done.is_set())
            self.assertTrue(stream.stop())
            self.assertTrue(stream.done.is_set())
        finally:
            release.set()
            server.shutdown()
            server.server_close()
            thread.join(3)

    def test_strict_boolean_integer_wire_types(self):
        with self.assertRaises(AssertionError):
            fixture.strict({"replicas": True}, {"replicas": 1}, "wire")

    def test_docker_daemon_error_is_never_absence_or_empty_counter(self):
        with patch.object(fixture.subprocess, "run", return_value=SimpleNamespace(
                returncode=1, stdout="", stderr="Cannot connect to Docker daemon")):
            with self.assertRaisesRegex(AssertionError, "Cannot connect"):
                fixture.counters([{"container_id": "own-container"}])

    def test_counter_uses_full_request_ids_and_rejects_duplicates(self):
        logs = "Received request req-00000001: prompt=16 tok, completion=8 tok\n"
        with patch.object(fixture, "docker", return_value=logs):
            self.assertEqual(fixture.counters([{"container_id": "own"}]), {"own": 1})
        with patch.object(fixture, "docker", return_value=logs + logs):
            with self.assertRaisesRegex(AssertionError, "duplicate"):
                fixture.counters([{"container_id": "own"}])


if __name__ == "__main__":
    unittest.main()
