"""Terminal identity/release negative tests without an agent or Docker daemon."""
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import container_terminal_fixture as subject


def stat(pid=31, started=555):
    return f"{pid} (owned shell) " + " ".join(["S", *(["0"] * 18), str(started), *(["0"] * 10)])


class TerminalOracles(unittest.TestCase):
    def test_identity_uses_start_time_and_rejects_invalid_pid_or_stat(self):
        self.assertEqual(subject.process_identity(stat()), (31, 555))
        self.assertNotEqual(subject.process_identity(stat(started=556)), (31, 555))
        for value in ("", "absent", "no such host", "31 sh", stat(pid=1), "31 (sh) S malformed"):
            with self.subTest(value=value), self.assertRaises(AssertionError):
                subject.process_identity(value)
        with self.assertRaises(AssertionError):
            subject.process_identity(stat(pid=32), 31)

    def test_process_lookup_requires_owned_identity_and_only_exact_enoent_is_absent(self):
        absent = SimpleNamespace(returncode=1, stdout=b"", stderr=b"cat: can't open '/proc/31/stat': No such file or directory\n")
        with patch.object(subject.subprocess, "run", return_value=absent):
            self.assertIsNone(subject.observed_process("owned-container", (31, 555)))
        present = SimpleNamespace(returncode=0, stdout=stat().encode(), stderr=b"")
        with patch.object(subject.subprocess, "run", return_value=present):
            self.assertEqual(subject.observed_process("owned-container", (31, 555)), (31, 555))
        for message in (b"cat: can't open '/proc/31/stat': Permission denied",
                        b"no such host", b"Error: No such container: owned-container", b"No such file or directory"):
            failed = SimpleNamespace(returncode=1, stdout=b"", stderr=message)
            with self.subTest(message=message), patch.object(subject.subprocess, "run", return_value=failed), self.assertRaises(AssertionError):
                subject.observed_process("owned-container", (31, 555))
        for output in (b"absent", b"", b"no such host"):
            malformed_success = SimpleNamespace(returncode=0, stdout=output, stderr=b"")
            with self.subTest(output=output), patch.object(subject.subprocess, "run", return_value=malformed_success), self.assertRaises(AssertionError):
                subject.observed_process("owned-container", (31, 555))
        for error in (OSError("daemon unavailable"), subject.subprocess.TimeoutExpired("docker", 20)):
            with patch.object(subject.subprocess, "run", side_effect=error), self.assertRaises(type(error)):
                subject.observed_process("owned-container", (31, 555))
        denied = SimpleNamespace(returncode=1, stdout=b"", stderr=b"cat: Permission denied")
        ended = {"id": 7, "active": False, "ended_at": "2026-10-21T08:00:00Z"}
        with patch.object(subject, "audits", return_value=[ended]), \
             patch.object(subject, "admission", return_value={"active_node": 0, "active_total": 0}), \
             patch.object(subject.subprocess, "run", return_value=denied), self.assertRaises(AssertionError):
            subject.wait_released(Mock(), Mock(), "token", "node", "owned-container", (31, 555), 7)
        for identity in ((1, 555), ("31", 555), None):
            with self.subTest(identity=identity), self.assertRaises(AssertionError):
                subject.observed_process("owned-container", identity)

    def test_exit_code_requires_one_strict_integer_control(self):
        self.assertEqual(subject.exit_code([{"type": "info"}, {"type": "exit", "code": 0}]), 0)
        for value in (False, True, "0", 0.0, None):
            with self.subTest(value=value), self.assertRaises(AssertionError):
                subject.exit_code([{"type": "exit", "code": value}])
        for controls in ([], [{"type": "info", "code": 0}], [{"type": "exit"}], [{"type": "exit", "code": 0}] * 2):
            with self.subTest(controls=controls), self.assertRaises(AssertionError):
                subject.exit_code(controls)

    def test_preflight_rejects_missing_unknown_and_coercive_json_types(self):
        value = {"enabled": True, "can_open": True, "container_supported": True,
                 "idle_timeout_min": 15, "max_per_node": 4, "max_global": 32,
                 "active_total": 0, "active_node": 0, "reason": "", "container_reason": "",
                 "container_runtime": "docker", "container_capability": "webterm.container"}
        self.assertEqual(subject.assert_preflight(value), value)
        variants = [{**value, "active_total": False}, {**value, "enabled": 1},
                    {**value, "active_node": "0"}, {**value, "reason": None},
                    {**value, "extra": 0}, {key: item for key, item in value.items() if key != "can_open"}]
        for document in variants:
            with self.subTest(document=document), self.assertRaises(AssertionError):
                subject.assert_preflight(document)

    def test_closed_audit_alone_does_not_prove_shell_or_slot_release(self):
        ended = {"id": 7, "active": False, "ended_at": "2026-10-21T08:00:00Z"}
        page = Mock()
        for identity, active in (((31, 555), 0), (None, 1)):
            with self.subTest(identity=identity, active=active), \
                 patch.object(subject, "audits", return_value=[ended]), \
                 patch.object(subject, "admission", return_value={"active_node": active, "active_total": active}), \
                 patch.object(subject, "observed_process", return_value=identity), \
                 patch.object(subject.time, "monotonic", side_effect=[0, 0, 2]), \
                 self.assertRaises(AssertionError):
                subject.wait_released(page, Mock(), "token", "node", "container", (31, 555), 7, timeout=1)
        with patch.object(subject, "audits", return_value=[ended]), \
             patch.object(subject, "admission", return_value={"active_node": 0, "active_total": 0}), \
             patch.object(subject, "observed_process", return_value=None):
            self.assertEqual(subject.wait_released(page, Mock(), "token", "node", "container", (31, 555), 7), ended)

    def test_wrong_audit_target_and_duplicate_admission_fail(self):
        row = {"id": 8, "username": "admin", "node_uuid": "node", "target": "container", "ref": "owned", "active": True}
        for values in ([{**row, "target": "host"}], [{**row, "ref": "foreign"}], [row, {**row, "id": 9}]):
            with self.subTest(values=values), patch.object(subject, "audits", return_value=values), self.assertRaises(AssertionError):
                subject.wait_audit(Mock(), Mock(), "token", "node", "owned", set(), True)

    def test_failed_user_preparation_retains_scoped_ownership(self):
        report = {}
        server = SimpleNamespace(work=Path("/private/owned"))
        with patch.object(subject.diagnostics, "prepare", return_value={}), \
             patch.object(subject.sqlite3, "connect", side_effect=OSError("read-only fixture DB")), \
             self.assertRaises(OSError):
            subject.prepare(server, "token", "run", report, {}, Path("agent"), Mock())
        self.assertEqual(report["owned_terminal_username"], "e2e-run-reader")

    def test_context_and_session_cleanup_failure_does_not_skip_base_reclaim(self):
        report = {"run_id": "run", "owned_terminal_username": "owned-reader", "owned_node_uuid": "node"}
        context = Mock()
        context.close.side_effect = RuntimeError("context close failed")
        def base_cleanup(server, token, report, holder):
            report["remaining_resources"] = []
            report["cleanup_steps"] = [{"stage": "base observed absent", "ok": True}]
            report["owned_node_uuid"] = None
            return True
        holder = {"terminal_contexts": [{"context": context, "closed": False}]}
        server = SimpleNamespace(work=Path("/private/owned"))
        with patch.object(subject, "admission", side_effect=RuntimeError("query unavailable")), \
             patch.object(subject.sqlite3, "connect", side_effect=OSError("DB failed")), \
             patch.object(subject.diagnostics, "cleanup", side_effect=base_cleanup):
            self.assertFalse(subject.cleanup(server, "token", report, holder))
        self.assertIsNone(report["owned_node_uuid"])
        self.assertEqual(report["owned_terminal_username"], "owned-reader")
        self.assertEqual(len(report["remaining_resources"]), 3)
        self.assertEqual([step["ok"] for step in report["cleanup_steps"]], [True, False, False, False])


if __name__ == "__main__":
    unittest.main()
