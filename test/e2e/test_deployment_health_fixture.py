"""Offline failure-path and independent-health-oracle tests; no real Docker."""
import json
import sys
from pathlib import Path
from tempfile import TemporaryDirectory
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import deployment_health as browser
import deployment_health_fixture as fixture


def health_probe(mode="ok", outcome="completed", http_status=200):
    return {"start": {"seq": 2, "at_ms": 1000, "phase": "started", "mode": mode,
        "method": "POST", "path": "/v1/chat/completions", "query": "",
        "body": {"model": "served", "messages": [{"role": "user", "content": "1"}],
                 "max_tokens": 1, "stream": False}},
        "terminal": {"seq": 3, "at_ms": 1001, "phase": outcome, "request": 2, "http_status": http_status}}


class AuditTest(unittest.TestCase):
    def setUp(self):
        self.temp = TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)

    def write(self, events, tail=b""):
        (self.directory / "audit.jsonl").write_bytes(b"".join(json.dumps(event).encode() + b"\n" for event in events) + tail)

    def test_run_owned_weight_hashes_do_not_claim_repository_source_paths(self):
        with patch.object(sys, "path", [str(fixture.ROOT / "scripts"), *sys.path]):
            import e2e_coverage_provenance as provenance
        weights = self.directory / "model"
        weights.mkdir()
        (weights / "config.json").write_text('{"model_type":"fixture"}')
        server = SimpleNamespace(storage_root=self.directory, work=self.directory, base="http://fixture.invalid")
        report, holder = {"fake_engine_image_id": "sha256:engine"}, {}
        def build(*args, **kwargs):
            (self.directory / "health-audit-proxy").write_bytes(b"test-proxy")
            return SimpleNamespace(returncode=0, stderr="")
        try:
            with patch.object(fixture.subprocess, "run", side_effect=build), \
                    patch.object(fixture.subprocess, "Popen"), \
                    patch.object(fixture.replicas, "docker", return_value="sha256:engine"), \
                    patch.object(fixture.lifecycle, "wait_online"), \
                    patch.object(fixture.lifecycle, "MODEL_DIR", weights), \
                    patch.object(fixture, "http", side_effect=[
                        (201, {"node": {"uuid": "owned-node"}, "token": "credential"}), (400, {})]):
                with self.assertRaisesRegex(AssertionError, "health model fixture"):
                    fixture.prepare(server, "token", "run-id", report, holder, Path("agent"), Mock())
        finally:
            if holder.get("log"):
                holder["log"].close()
        contract = {"manifest": "test/e2e/coverage/deployment-health.json",
                    "test_file": "test/e2e/deployment_health.py"}
        report["source_hashes"] = {relative: fixture.digest(fixture.ROOT / relative)
                                   for relative in contract.values()}
        self.assertEqual(provenance.fingerprints(report, contract, fixture.ROOT), [])
        self.assertEqual(report["health_proxy"]["weights_sha256"],
                         {"config.json": fixture.digest(weights / "config.json")})

    def test_atomic_controls_are_strict(self):
        fixture.write_control(self.directory, "slow")
        self.assertEqual(json.loads((self.directory / "control.json").read_text()), {"mode": "slow", "delay_ms": 2000})
        self.assertFalse((self.directory / "control.new").exists())
        with self.assertRaises(AssertionError):
            fixture.write_control(self.directory, "unknown")

    def test_ordered_audit_and_incomplete_tail(self):
        self.write([{"seq": 1, "phase": "child_started"}, health_probe()["start"], health_probe()["terminal"]], b'{"seq":4')
        probes = fixture.probes(self.directory)
        self.assertEqual(len(probes), 1)
        self.assertEqual(probes[0], health_probe())

    def test_audit_missing_duplicate_reordered_and_orphan_terminals_fail(self):
        malformed = [
            [{"seq": 2, "phase": "child_started"}],
            [{"seq": 1, "phase": "child_started"}, {"seq": 1, "phase": "child_started"}],
            [{"seq": 1, "phase": "completed", "request": 99}],
            [{"seq": 1, "phase": "started"}, {"seq": 2, "phase": "completed", "request": 1},
             {"seq": 3, "phase": "cancelled", "request": 1}],
        ]
        for events in malformed:
            with self.subTest(events=events), self.assertRaises(AssertionError):
                self.write(events)
                fixture.audit(self.directory)

    def test_fixture_control_and_transport_errors_cannot_be_normal_failures(self):
        for phase in ("rejected", "control_failed", "transport_failed"):
            with self.subTest(phase=phase), self.assertRaises(AssertionError):
                self.write([{"seq": 1, "phase": phase, **({"request": 1} if phase == "transport_failed" else {})}])
                fixture.audit(self.directory)

    def test_protocol_requires_exact_types_model_body_method_mode(self):
        fixture.verify_probe(health_probe(), "served", "ok", "completed")
        mutations = [("max_tokens", "1"), ("max_tokens", True), ("stream", "false"), ("model", "other")]
        for field, value in mutations:
            probe = health_probe()
            probe["start"]["body"][field] = value
            with self.subTest(field=field, value=value), self.assertRaises(AssertionError):
                fixture.verify_probe(probe, "served", "ok", "completed")
        for field, value in (("method", "GET"), ("query", "q=x"), ("mode", "fail")):
            probe = health_probe()
            probe["start"][field] = value
            with self.assertRaises(AssertionError):
                fixture.verify_probe(probe, "served", "ok", "completed")

    def test_pending_request_is_not_complete(self):
        probe = health_probe()
        probe["terminal"] = None
        with self.assertRaises(AssertionError):
            fixture.verify_probe(probe, "served", "ok", "completed")


class HealthOracleTest(unittest.TestCase):
    def document(self, state="timeout", elapsed=1000):
        return {"instances": [{"uuid": "instance", "container_id": "docker", "replica": 0, "state": "running"}],
                "health": {"status": state, "last": {"status": state, "at_ms": 1000,
                    "latency_ms": elapsed, "instance_uuid": "instance"}}}

    def test_timeout_requires_external_cancellation_and_exact_published_deadline(self):
        probe = health_probe("slow", "cancelled", 0)
        probe["terminal"]["at_ms"] = 2000
        identity = {"uuid": "instance", "container_id": "docker"}
        with patch.object(fixture, "probes", return_value=[probe]):
            self.assertTrue(browser.assert_health(self.document(), {"directory": "d", "served": "served"}, "slow", "timeout", 0, identity))
            with self.assertRaises(AssertionError):
                browser.assert_health(self.document(elapsed=2000), {"directory": "d", "served": "served"}, "slow", "timeout", 0, identity)
            probe["terminal"]["phase"] = "completed"
            with self.assertRaises(AssertionError):
                browser.assert_health(self.document(), {"directory": "d", "served": "served"}, "slow", "timeout", 0, identity)

    def test_changed_container_or_stale_sample_cannot_pass(self):
        identity = {"uuid": "instance", "container_id": "docker"}
        document = self.document()
        self.assertIsNone(browser.assert_health(document, {}, "slow", "timeout", 1000, identity))
        document["instances"][0]["container_id"] = "replacement"
        with self.assertRaises(AssertionError):
            browser.assert_health(document, {}, "slow", "timeout", 0, identity)

    def test_latency_display_contract_known_units(self):
        self.assertEqual([browser.format_ms(value) for value in (0, 999, 1000, 2050, 10000, 60000)],
                         ["0 ms", "999 ms", "1 s", "2.1 s", "10 s", "1 min"])


class CleanupTest(unittest.TestCase):
    def test_guard_failure_preserves_failure_and_still_cleans_target(self):
        for fail in (False, RuntimeError("guard cleanup failed")):
            report = {"health_deployments": [{"role": "guard", "id": 2, "name": "guard", "directory": "absent"}]}
            def domain(server, token, report, holder):
                report["remaining_resources"] = []
                report["cleanup_steps"] = [{"stage": "target", "ok": True}]
                return True
            with patch.object(fixture.containers, "cleanup", side_effect=fail if isinstance(fail, Exception) else None,
                              return_value=fail if not isinstance(fail, Exception) else False), \
                    patch.object(fixture.replicas, "cleanup", side_effect=domain) as target, \
                    patch.object(fixture, "audit", return_value=[]), patch.object(fixture, "probes", return_value=[]):
                self.assertFalse(fixture.cleanup(SimpleNamespace(), "token", report, {}))
                target.assert_called_once()

    def test_missing_proxy_exit_is_a_reclaimable_residual(self):
        report = {"health_deployments": [{"role": "target", "id": 1, "directory": "/owned/health", "expected_children": 1}]}
        with patch.object(fixture.replicas, "cleanup", return_value=True), \
                patch.object(fixture, "audit", return_value=[{"seq": 1, "phase": "child_started"}]), \
                patch.object(fixture, "probes", return_value=[]):
            self.assertFalse(fixture.cleanup(SimpleNamespace(), "token", report, {}))
        self.assertEqual(report["remaining_resources"][0]["kind"], "health-proxy")
        self.assertEqual(report["remaining_resources"][0]["id"], "/owned/health")

    def test_main_cleanup_exception_does_not_skip_guard_or_audit_cleanup(self):
        report = {"health_deployments": [{"role": "target", "directory": "d", "expected_children": 1}]}
        with patch.object(fixture.replicas, "cleanup", side_effect=TimeoutError("query failed")), \
                patch.object(fixture, "audit", return_value=[{"seq": 1, "phase": "child_started"}]) as audit:
            self.assertFalse(fixture.cleanup(SimpleNamespace(), "token", report, {}))
            audit.assert_called_once()
        self.assertEqual({item["kind"] for item in report["remaining_resources"]}, {"health-domain-cleanup", "health-proxy"})


if __name__ == "__main__":
    unittest.main()
