"""Fail-closed batch orchestration tests; no browser or product imports."""
import copy
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch
from argparse import Namespace

import first_batch as batch


class FirstBatchTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.addCleanup(patch.stopall)
        patch.object(batch, "ROOT", self.root).start()
        self.sha, self.target = "a" * 40, "b" * 40
        source = self.root / "test/e2e/example.py"
        source.parent.mkdir(parents=True)
        source.write_text("# fixture driver\n")
        manifests = self.root / "test/e2e/coverage"
        manifests.mkdir()
        self.manifest = {"contract_id": "gpuworks/test/example", "name": "Example",
                         "test_file": "test/e2e/example.py", "driver_file": "test/e2e/example.py",
                         "lane": "local-server", "required_evidence": ["ui", "request", "cleanup"],
                         "manifest": "test/e2e/coverage/example.json"}
        manifest = self.root / self.manifest["manifest"]
        manifest.write_text(json.dumps(self.manifest))
        self.server, self.agent = self.root / "server", self.root / "agent"
        self.server.write_bytes(b"server")
        self.agent.write_bytes(b"agent")
        self.binaries = {"binary_sha256": batch.digest(self.server), "agent_binary_sha256": batch.digest(self.agent)}
        self.report = {"contract": self.manifest["contract_id"], "locale": "en", "run_id": "20261021T000000Z-case",
                       "source_sha": self.sha, "target_sha": self.target, "working_tree": "clean", "status": "PASS",
                       "retries": 0, "case_completed": True, "artifact_scan": "PASS", **self.binaries,
                       "browser_close_ok": True, "environment_cleanup_ok": True, "remaining_resources": [],
                       "cleanup_steps": [{"stage": "observed absent", "ok": True}],
                       "evidence": [{"layer": layer} for layer in ("ui", "request", "cleanup")],
                       "source_hashes": {self.manifest["test_file"]: batch.digest(source),
                                         self.manifest["manifest"]: batch.digest(manifest)}}
        # Real docker_required=False runs do not start/hash an agent binary.
        self.report.pop("agent_binary_sha256")

    def validate(self, report=None, seen=None):
        return batch.validate(report or self.report, self.manifest, self.sha, self.target,
                              self.binaries, "en", set() if seen is None else seen)

    def test_exact_current_report_and_unique_run(self):
        seen = set()
        self.assertEqual(self.validate(seen=seen), self.report["run_id"])
        with self.assertRaisesRegex(AssertionError, "reused"):
            self.validate(seen=seen)

    def test_rejects_wrong_identity_and_mutation(self):
        for changes in ({"contract": "other"}, {"locale": "zh"}, {"source_sha": self.target},
                        {"target_sha": self.sha}, {"working_tree": "dirty"}, {"product_mutation": "actual mutant"},
                        {"binary_sha256": "c" * 64}, {"retries": 1}, {"run_id": ""}):
            with self.subTest(changes=changes), self.assertRaises(AssertionError):
                self.validate({**self.report, **changes})

    def test_agent_hash_is_required_only_for_agent_contracts(self):
        self.validate()
        self.manifest["lane"] = "real-server-agent-docker"
        with self.assertRaisesRegex(AssertionError, "not current"):
            self.validate()
        report = {**self.report, "agent_binary_sha256": self.binaries["agent_binary_sha256"]}
        self.validate(report)
        with self.assertRaisesRegex(AssertionError, "not current"):
            self.validate({**report, "agent_binary_sha256": "d" * 64})

    def test_rejects_incomplete_or_failed_evidence(self):
        for changes in ({"evidence": [{"layer": "cleanup"}]}, {"remaining_resources": [{"id": 1}]},
                        {"browser_close_ok": False}, {"environment_cleanup_ok": False},
                        {"case_completed": False}, {"artifact_scan": "FAIL"}, {"failure": "hidden failure"},
                        {"cleanup_steps": [{"ok": False}]}, {"status": "FAIL_TEST"}):
            with self.subTest(changes=changes), self.assertRaises(AssertionError):
                self.validate({**self.report, **changes})

    def test_stale_source_hash_is_rejected(self):
        (self.root / self.manifest["test_file"]).write_text("# changed oracle\n")
        with self.assertRaisesRegex(AssertionError, "not current"):
            self.validate()

    def test_stale_dirty_or_wrong_binary_metadata_is_rejected(self):
        with patch.object(batch.subprocess, "check_output") as version:
            version.return_value = f"gpuworks dev ({self.sha[:9]}, 2026-10-21T00:00:00Z)\n"
            self.assertIn(self.sha[:9], batch.binary_identity(self.server, "gpuworks", self.sha))
            for line in (f"gpuworks dev ({self.target[:9]}, now)",
                         f"gpuworks dev-dirty ({self.sha[:9]}, now)",
                         f"gpuworks-agent dev ({self.sha[:9]}, now)", "gpuworks dev (none, unknown)"):
                version.return_value = line
                with self.subTest(line=line), self.assertRaises(AssertionError):
                    batch.binary_identity(self.server, "gpuworks", self.sha)

    def configure_run(self, failure=None):
        patch.object(batch, "CASES", (("example", "example.py", "example.json", ()),)).start()
        patch.object(batch, "git", side_effect=lambda *args:
                     "" if args[0] == "status" else self.target if args[-1] == "wei/master" else self.sha).start()
        patch.object(batch, "binary_identity", return_value="verified fixture build").start()
        args = Namespace(artifact_dir=self.root / "output", server_bin=self.server, agent_bin=self.agent)
        self.calls = []

        def execute(command, **kwargs):
            if command[0] == "git":
                return subprocess.CompletedProcess(command, 0)
            self.calls.append(command)
            locale = command[command.index("--locale") + 1]
            directory = Path(command[command.index("--artifact-dir") + 1])
            directory.mkdir()
            report = copy.deepcopy(self.report)
            report.update(locale=locale, run_id=report["run_id"] + "-" + locale)
            if failure == "cleanup":
                report["cleanup_steps"] = [{"ok": False}]
            if failure != "missing":
                (directory / "report.json").write_text(json.dumps(report))
            return subprocess.CompletedProcess(command, 1 if failure == "exit" else 0)

        patch.object(batch.subprocess, "run", side_effect=execute).start()
        return args

    def test_runs_both_locales_in_order_with_distinct_artifacts(self):
        args = self.configure_run()
        self.assertEqual(batch.run(args), 0)
        index = json.loads((args.artifact_dir / "batch-report.json").read_text())
        self.assertEqual([row["status"] for row in index["cases"]], ["PASS", "PASS"])
        self.assertEqual([row["locale"] for row in index["cases"]], ["en", "zh"])
        self.assertEqual(len(self.calls), 2)

    def test_failed_child_never_retries_or_runs_next_locale(self):
        for failure in ("exit", "missing", "cleanup"):
            with self.subTest(failure=failure):
                args = self.configure_run(failure)
                args.artifact_dir = self.root / ("output-" + failure)
                self.assertEqual(batch.run(args), 1)
                index = json.loads((args.artifact_dir / "batch-report.json").read_text())
                self.assertEqual([row["status"] for row in index["cases"]], ["FAIL", "NOT_RUN"])
                self.assertEqual(len(self.calls), 1)

    def test_existing_artifact_directory_is_never_overwritten(self):
        args = self.configure_run()
        args.artifact_dir.mkdir()
        sentinel = args.artifact_dir / "report.json"
        sentinel.write_text("preserve")
        with self.assertRaises(FileExistsError):
            batch.run(args)
        self.assertEqual(sentinel.read_text(), "preserve")
        self.assertEqual(self.calls, [])

    def test_make_recipe_preserves_literal_artifact_path(self):
        original = Path(batch.__file__).with_name("first-batch.mk").read_text()
        recipe = next(line for line in original.splitlines() if "--artifact-dir" in line)
        makefile = self.root / "Makefile"
        makefile.write_text("export E2E_BATCH_DIR\nrun:\n" + recipe + "\n")
        shim = self.root / "python3"
        shim.write_text("#!" + os.sys.executable + "\nimport json,os,sys\n"
                        "open(os.environ['ARGS_FILE'],'w').write(json.dumps(sys.argv))\n")
        shim.chmod(0o700)
        output = self.root / "args.json"
        value = 'a space/"quote"/$(touch SHOULD_NOT_EXIST)/`touch ALSO_NOT`'
        environment = {**os.environ, "PATH": str(self.root) + os.pathsep + os.environ["PATH"],
                       "E2E_BATCH_DIR": value, "ARGS_FILE": str(output)}
        subprocess.run(["make", "--no-print-directory", "-f", str(makefile), "run"],
                       cwd=self.root, env=environment, check=True, capture_output=True)
        self.assertEqual(json.loads(output.read_text())[-1], value)
        self.assertFalse((self.root / "SHOULD_NOT_EXIST").exists())
        self.assertFalse((self.root / "ALSO_NOT").exists())


if __name__ == "__main__":
    unittest.main()
