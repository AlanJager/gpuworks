"""Offline negative oracles; no server, Docker daemon or browser is started."""
import copy
import json
from pathlib import Path
import subprocess
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import instance_diagnostics_fixture as subject


class DiagnosticsOracles(unittest.TestCase):
    def test_missing_and_altered_metric_samples_fail(self):
        valid = {"items": [{"labels": {"instance": "owned"}, "points": [[123, 0.375], [234, 0.375]]}], "total": 1}
        self.assertEqual(subject.assert_series(valid, "owned", 0.375), valid["items"][0]["points"])
        variants = [None, {}, {"items": []}, {"items": valid["items"] * 2}]
        for points in ([], [[123, 999]], [[123, "0.375"]], [[True, 0.375]], [[123, True]]):
            variant = copy.deepcopy(valid)
            variant["items"][0]["points"] = points
            variants.append(variant)
        other = copy.deepcopy(valid)
        other["items"][0]["labels"]["instance"] = "foreign"
        variants.append(other)
        variants.extend({**valid, "total": wrong} for wrong in (0, 2, True, "1"))
        for value in variants:
            with self.subTest(value=value), self.assertRaises(AssertionError):
                subject.assert_series(value, "owned", 0.375)

    def test_strict_json_rejects_boolean_numeric_type_drift(self):
        for value in (True, False):
            subject.strict_json({"nested": [{"read_only": value}]}, {"nested": [{"read_only": value}]}, "UI JSON")
            with self.subTest(value=value), self.assertRaises(AssertionError):
                subject.strict_json({"nested": [{"read_only": int(value)}]}, {"nested": [{"read_only": value}]}, "UI JSON")

    def test_only_requests_started_in_action_can_supply_gauge_responses(self):
        metrics = list(subject.GAUGES)
        def request(metric):
            return SimpleNamespace(url="http://local/api/v1/metrics/query?instance=owned&start=0&end=900000&step=10s&metric=" + metric)
        for stale_response in (False, True):
            listeners = {}
            page = Mock()
            page.on.side_effect = lambda name, callback: listeners.update({name: callback})
            page.remove_listener.side_effect = lambda name, callback: listeners.pop(name)
            current = [request(metric) for metric in metrics]
            # Same URL/value as the final new request, but a different actual
            # request object whose dispatch happened before listener install.
            stale = SimpleNamespace(request=request(metrics[-1]))
            expected = [SimpleNamespace(request=sent) for sent in current]
            def action():
                for sent in current:
                    listeners["request"](sent)
                for response in expected[:2]:
                    listeners["response"](response)
                listeners["response"](stale if stale_response else expected[-1])
            with patch.object(subject.time, "monotonic", side_effect=[0, 0, 20]):
                if stale_response:
                    with self.assertRaisesRegex(AssertionError, "three new gauge requests"):
                        subject.capture_gauges(page, "owned", (900000, "10s"), action, timeout=1)
                else:
                    self.assertEqual(subject.capture_gauges(page, "owned", (900000, "10s"), action), expected)
            self.assertEqual(listeners, {})

    def test_docker_error_timeout_and_empty_response_are_not_success(self):
        for result in [SimpleNamespace(returncode=1, stdout=b"", stderr=b"no such host"),
                       SimpleNamespace(returncode=1, stdout=b"", stderr=b"permission denied")]:
            with patch.object(subject.subprocess, "run", return_value=result), self.assertRaises(AssertionError):
                subject.docker("inspect", "owned")
        with patch.object(subject.subprocess, "run", side_effect=subprocess.TimeoutExpired("docker", 20)), self.assertRaises(subprocess.TimeoutExpired):
            subject.docker("inspect", "owned")
        with patch.object(subject, "docker", return_value=b"[]"), self.assertRaises(AssertionError):
            subject.inspect("owned")

    def test_metric_samples_must_be_distinct_ordered_buckets_inside_query(self):
        for points in ([[100, 2], [100, 2]], [[200, 2], [100, 2]], [[900, 2]], [[105, 2]]):
            document = {"items": [{"labels": {"instance": "owned"}, "points": points}], "total": 1}
            with self.subTest(points=points), self.assertRaises(AssertionError):
                subject.assert_series(document, "owned", 2, (100, 500, 100))

    def test_prepare_keeps_deployment_ownership_when_status_is_wrong(self):
        fixture = {"model": {"id": 1}, "node": {"uuid": "node"}, "name": "e2e-owned", "served_name": "route", "image": "owned-image"}
        report = {"owned_deployment_name": "e2e-owned"}
        with patch.object(subject.tools, "prepare", return_value=fixture), patch.object(subject, "http", return_value=(500, {"id": 7})), self.assertRaises(AssertionError):
            subject.prepare(SimpleNamespace(base="local"), "secret", "run", report, {}, Path("agent"), SimpleNamespace(remember=lambda value: None))
        self.assertEqual(report["owned_deployment_id"], 7)
        self.assertEqual(report["owned_deployment_name"], "e2e-owned")

    def test_marker_requires_actual_single_engine_stdout_line(self):
        outputs = [b"", "INFO marker 中文诊断\nINFO marker 中文诊断\n".encode()]
        for output in outputs:
            with patch.object(subject, "docker", side_effect=[b"", output]), self.assertRaises(AssertionError):
                subject.write_marker("owned", "marker")
        with patch.object(subject, "docker", side_effect=[b"", "INFO marker 中文诊断\n".encode()]):
            self.assertEqual(subject.write_marker("owned", "marker"), "INFO marker 中文诊断")

    def test_runtime_launch_identity_and_uncollected_process_must_match(self):
        actual = {"Id": "owned", "Image": "sha256:engine", "Path": "/engine", "Args": ["--model", "tiny"],
                  "Config": {"Entrypoint": ["/engine"], "Cmd": ["--model", "tiny"], "Env": ["A=1"]},
                  "Mounts": [{"Type": "bind", "Source": "/weights", "Destination": "/model", "RW": False}],
                  "HostConfig": {"Runtime": "runc", "NetworkMode": "host"}}
        image = {"Id": "sha256:engine", "Config": {"Entrypoint": ["/engine"], "Env": ["A=1"]}}
        container = {"status": "observed", "observed_at_unix_ms": 123, "container_id": "owned", "image_id": "sha256:engine",
                     "entrypoint": ["/engine"], "cmd": ["--model", "tiny"], "path": "/engine", "args": ["--model", "tiny"],
                     "env": {"A": "1"}, "mounts": [{"type": "bind", "source": "/weights", "destination": "/model", "read_only": True}],
                     "devices": [], "runtime_name": "runc", "network_mode": "host"}
        observed_image = {"status": "observed", "observed_at_unix_ms": 123, "image_id": "sha256:engine",
                          "entrypoint": ["/engine"], "cmd": [], "env": {"A": "1"}, "volumes": []}
        facts = {"runtime_observation_relation": {"status": "current"}, "runtime_observation": {
            "runtime": "docker", "container": container, "image": observed_image, "process": {"status": "unknown", "reason": "not_collected"}}}
        subject.assert_launch(facts, actual, image)
        for field, value in {"container_id": "foreign", "image_id": "sha256:foreign", "env": {"A": "wrong"}, "mounts": []}.items():
            changed = copy.deepcopy(facts)
            changed["runtime_observation"]["container"][field] = value
            with self.subTest(field=field), self.assertRaises(AssertionError):
                subject.assert_launch(changed, actual, image)
        for read_only in (True, False):
            actual["Mounts"][0]["RW"] = not read_only
            facts["runtime_observation"]["container"]["mounts"][0]["read_only"] = read_only
            subject.assert_launch(facts, actual, image)
            changed = copy.deepcopy(facts)
            changed["runtime_observation"]["container"]["mounts"][0]["read_only"] = int(read_only)
            with self.subTest(read_only=read_only), self.assertRaises(AssertionError):
                subject.assert_launch(changed, actual, image)
        facts["runtime_observation"]["process"] = {"status": "observed", "pid": 0}
        with self.assertRaises(AssertionError):
            subject.assert_launch(facts, actual, image)


if __name__ == "__main__":
    unittest.main()
