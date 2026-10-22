"""Container override fixture ownership and runtime-oracle regression tests.

Run: PYTHONPATH=test/e2e python3 -m unittest test_deployment_override_fixture -v
"""
import json
from pathlib import Path
import subprocess
import sqlite3
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import deployment_override_fixture as fixture
import deployment_override_create as case


class OverrideFixture(unittest.TestCase):
    def test_malformed_create_id_leaves_unique_name_recovery_instead_of_invalid_id_path(self):
        for ident in (True, None, "1", 0, -1, {}, []):
            with self.subTest(ident=ident):
                report = {"owned_deployment_name": "e2e-owned-deployment"}
                response = SimpleNamespace(json=lambda: {"id": ident})
                with self.assertRaisesRegex(AssertionError, "valid numeric ID"):
                    case.remember_created(response, report)
                self.assertNotIn("owned_deployment_id", report)
                self.assertEqual(report["owned_deployment_name"], "e2e-owned-deployment")
        report = {}
        self.assertEqual(case.remember_created(SimpleNamespace(json=lambda: {"id": 7}), report), 7)
        self.assertEqual(report["owned_deployment_id"], 7)

    def test_readonly_database_oracle_reads_only_owned_id_and_committed_wal(self):
        with tempfile.TemporaryDirectory() as directory:
            server = SimpleNamespace(work=Path(directory))
            data = server.work / "server-data"
            data.mkdir()
            with sqlite3.connect(data / "gpuworks.db") as writer:
                writer.execute("PRAGMA journal_mode=WAL")
                writer.execute("CREATE TABLE deployments (id INTEGER PRIMARY KEY, container_overrides TEXT)")
                writer.execute("INSERT INTO deployments VALUES (?, ?)", (1, '{"env":{"OWN":"first"}}'))
                writer.execute("INSERT INTO deployments VALUES (?, ?)", (2, '{"env":{"OTHER":"private"}}'))
                writer.commit()
                self.assertEqual(fixture.stored_overrides(server, 1), {"env": {"OWN": "first"}})
                writer.execute("UPDATE deployments SET container_overrides=? WHERE id=1", ('{"env":{"OWN":"updated"}}',))
                writer.commit()
                self.assertEqual(fixture.stored_overrides(server, 1), {"env": {"OWN": "updated"}})
                with self.assertRaisesRegex(AssertionError, "one run-owned"):
                    fixture.stored_overrides(server, 999)

    def test_build_failure_registers_image_before_any_resource_preparation(self):
        with tempfile.TemporaryDirectory() as directory:
            server = SimpleNamespace(work=Path(directory))
            report = {}
            with patch.object(fixture, "command", side_effect=[json.dumps([{"Id": "sha256:tools"}]), "", json.dumps([{"Id": "sha256:engine"}]), "", AssertionError("build broke")]), \
                    patch.object(fixture.wizard, "prepare") as prepare:
                with self.assertRaisesRegex(AssertionError, "build broke"):
                    fixture.prepare(server, "token", "20261021T010203Z-AbCd", report, {}, Path("agent"), lambda x: None)
            self.assertEqual(report["owned_override_image"], "gpuworks/e2e-overrides:20261021t010203z-abcd")
            prepare.assert_not_called()

    def test_bad_environment_boolean_or_mount_wire_type_cannot_compare_equal(self):
        with self.assertRaises(AssertionError):
            case.wizard.assert_json({"read_only": 1}, {"read_only": True}, "wire type")

    def test_late_extra_mutation_is_not_adopted_as_the_expected_cancel_baseline(self):
        values = {"model": {"id": 1}, "template": {"id": 2}, "node": {"uuid": "node"},
                  "run_id": "run", "served_name": "route", "name": "name", "image": "image",
                  "override_value": "override", "ro_dir": "/ro", "rw_dir": "/rw"}
        expected = case.expected_requests(values)
        case.wizard.assert_json(expected, case.expected_requests(values), "initial request sequence")
        for method in ("POST", "PUT", "PATCH", "DELETE"):
            with self.subTest(method=method):
                actual = expected + [{"method": method, "path": "/api/v1/deployments/1", "query": "", "body": {}}]
                with self.assertRaisesRegex(AssertionError, "late mutation"):
                    case.wizard.assert_json(actual, case.expected_requests(values), "late mutation")

    def test_image_cleanup_distinguishes_absent_from_daemon_and_permission_failure(self):
        image = "gpuworks/e2e-overrides:run"
        for error, expected in (("Error response from daemon: No such image: " + image, True),
                                ("Cannot connect to the Docker daemon", False),
                                ("permission denied", False)):
            with self.subTest(error=error):
                report = {"owned_override_image": image, "remaining_resources": []}
                with patch.object(fixture, "inventory"), patch.object(fixture.wizard, "cleanup", return_value=True), patch.object(
                        fixture.subprocess, "run", side_effect=[SimpleNamespace(returncode=1, stderr="rm failed"),
                        SimpleNamespace(returncode=1, stderr=error)]):
                    okay = fixture.cleanup(None, None, report, {})
                self.assertIs(okay, expected)
                self.assertEqual(report["owned_override_image"], None if expected else image)
                self.assertEqual(len(report["remaining_resources"]), 0 if expected else 1)

    def test_cleanup_still_attempts_owned_image_when_resource_cleanup_throws(self):
        image = "gpuworks/e2e-overrides:run"
        report = {"owned_override_image": image}
        with patch.object(fixture, "inventory"), patch.object(fixture.wizard, "cleanup", side_effect=TimeoutError("resource timeout")), patch.object(
                fixture.subprocess, "run", side_effect=[SimpleNamespace(returncode=0, stderr=""),
                SimpleNamespace(returncode=1, stderr="No such image: " + image)]) as remove:
            with self.assertRaisesRegex(TimeoutError, "resource timeout"):
                fixture.cleanup(None, None, report, {})
        self.assertEqual(remove.call_count, 2)
        self.assertIsNone(report["owned_override_image"])

    def test_inventory_never_claims_another_tag_of_the_same_cached_image(self):
        report = {"owned_override_image": "own:tag"}
        def command(args):
            if args[1] == "ps":
                return "owned-id\nforeign-id\n"
            return json.dumps([{"Image": "sha256:shared", "Config": {
                "Image": "own:tag" if args[-1] == "owned-id" else "foreign:tag"}}])
        with patch.object(fixture, "command", side_effect=command):
            fixture.inventory(report)
        self.assertEqual(report["containers"], ["owned-id"])

    def test_runtime_oracle_rejects_wrong_env_ro_bytes_and_false_write_protection(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            ro, rw = root / "ro", root / "rw"
            ro.mkdir(); rw.mkdir()
            values = {"image": "own:image", "override_value": "browser 中文", "read_bytes": b"known\n",
                      "write_bytes": b"written\n", "ro_dir": str(ro), "rw_dir": str(rw)}
            inspect = {"Config": {"Image": "own:image"}, "Image": "sha256:own",
                       "HostConfig": {"ShmSize": fixture.SHM_BYTES},
                       "Mounts": [{"Destination": "/e2e-" + name, "Type": "bind", "Source": str(path), "RW": writable}
                                  for name, path, writable in (("ro", ro, False), ("rw", rw, True))]}
            def execute(args):
                if args[:2] == ["docker", "inspect"]:
                    return json.dumps([inspect])
                if "stat" in args:
                    return "4096 65536\n"
                if "-i" in args:
                    (rw / "written.txt").write_bytes(values["write_bytes"])
                    return ""
                return values["override_value"]
            for mutation in (None, "env", "ro-bytes", "write-protection", "shm"):
                with self.subTest(mutation=mutation):
                    def mutated(args):
                        if mutation == "env" and args[-1].startswith("\nset"):
                            return "wrong env"
                        if mutation == "shm" and "stat" in args:
                            return "4096 1\n"
                        return execute(args)
                    result = SimpleNamespace(returncode=0 if mutation == "write-protection" else 2,
                                             stderr="" if mutation == "write-protection" else "Read-only file system")
                    with patch.object(fixture, "command", side_effect=mutated), patch.object(
                            fixture.subprocess, "check_output", return_value=b"wrong" if mutation == "ro-bytes" else values["read_bytes"]), \
                            patch.object(fixture.subprocess, "run", return_value=result):
                        if mutation is None:
                            observed = fixture.verify_runtime("owned", values, {"override_image_id": "sha256:own"})
                            self.assertEqual(observed["shm_bytes"], fixture.SHM_BYTES)
                        else:
                            with self.assertRaises(AssertionError):
                                fixture.verify_runtime("owned", values, {"override_image_id": "sha256:own"})


if __name__ == "__main__":
    unittest.main()
