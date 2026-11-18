"""Run the architecture gate with isolated budgets and command failures."""

import os
import pathlib
import shutil
import subprocess
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]


class ArchitectureMeasurementTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="gpuworks-arch-gate-")
        self.addCleanup(temporary.cleanup)
        self.root = pathlib.Path(temporary.name)
        # Keep the real package/import checks, but isolate the budget input.
        for entry in ROOT.iterdir():
            if entry.name not in (".git", "scripts", "artifacts", "bin"):
                (self.root / entry.name).symlink_to(entry, target_is_directory=entry.is_dir())
        scripts = self.root / "scripts"
        scripts.mkdir()
        for entry in (ROOT / "scripts").iterdir():
            if entry.name not in ("check_architecture.sh", "arch-constraints.txt"):
                (scripts / entry.name).symlink_to(entry, target_is_directory=entry.is_dir())
        shutil.copy2(ROOT / "scripts/check_architecture.sh", scripts)
        (scripts / "arch-constraints.txt").write_text(
            "ui_copy_long_strings\t0\tmax\nui_copy_caveat_strings\t0\tmax\n")
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.env = {**os.environ, "PATH": str(self.bin) + os.pathsep + os.environ["PATH"]}
        self.env.pop("GPUWORKS_ARCH_BASELINE", None)

    def node_result(self, output, status=0):
        real_node = shutil.which("node")
        self.assertIsNotNone(real_node, "Node must be installed to run the gate tests")
        node = self.bin / "node"
        node.write_text("#!/bin/sh\n"
                        "case \"$1\" in\n"
                        "  scripts/ui_copy_audit.mjs)\n"
                        "    printf '%s\\n' \"$GPUWORKS_TEST_MEASUREMENT\"\n"
                        "    exit \"$GPUWORKS_TEST_MEASUREMENT_STATUS\" ;;\n"
                        "esac\n"
                        "exec \"$GPUWORKS_TEST_REAL_NODE\" \"$@\"\n")
        node.chmod(0o755)
        self.env.update(GPUWORKS_TEST_MEASUREMENT=output,
                        GPUWORKS_TEST_MEASUREMENT_STATUS=str(status),
                        GPUWORKS_TEST_REAL_NODE=real_node)

    def run_gate(self):
        return subprocess.run([str(self.root / "scripts/check_architecture.sh")],
                              cwd=self.root, env=self.env, capture_output=True,
                              text=True, check=False, timeout=120)

    def test_failed_measurement_cannot_report_zero_violations(self):
        self.node_result("", 42)
        result = self.run_gate()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("ui_copy_long_strings", result.stderr)
        self.assertNotIn("ui_copy_long_strings 0/0", result.stdout)
        self.assertNotIn("architecture check passed", result.stdout)

    def test_failed_measurement_cannot_return_a_plausible_count(self):
        self.node_result("0", 42)
        result = self.run_gate()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("ui_copy_long_strings measurement failed", result.stderr)
        self.assertNotIn("ui_copy_long_strings 0/0", result.stdout)

    def test_missing_node_fails_instead_of_skipping_copy_checks(self):
        for directory in os.get_exec_path():
            if not pathlib.Path(directory).is_dir():
                continue
            for entry in pathlib.Path(directory).iterdir():
                target = self.bin / entry.name
                if (entry.name not in ("node", "nodejs") and entry.is_file()
                        and os.access(entry, os.X_OK) and not target.exists()):
                    target.symlink_to(entry.resolve())
        self.env["PATH"] = str(self.bin)
        self.assertIsNone(shutil.which("node", path=self.env["PATH"]))
        result = self.run_gate()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("ui_copy_long_strings measurement failed", result.stderr)
        self.assertNotIn("ui_copy_long_strings 0/0", result.stdout)

    def test_empty_or_invalid_measurements_are_not_zero(self):
        for output in ("", " \n", "unavailable", "-1", "1.5"):
            with self.subTest(output=output):
                self.node_result(output)
                result = self.run_gate()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("ui_copy_long_strings measurement must be", result.stderr)
                self.assertNotIn("architecture check passed", result.stdout)

    def test_zero_count_passes(self):
        self.node_result("0")
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("ui_copy_long_strings 0/0", result.stdout)
        self.assertIn("architecture check passed", result.stdout)

    def test_positive_count_still_exceeds_zero_budget(self):
        self.node_result("1")
        result = self.run_gate()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("ui_copy_long_strings exceeds its budget (1 > 0)", result.stderr)


if __name__ == "__main__":
    unittest.main()
