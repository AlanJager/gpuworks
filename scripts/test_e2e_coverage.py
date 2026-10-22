"""Offline report identity and data-export regressions.

Run: python3 -m unittest discover -s scripts -p 'test_e2e_coverage*.py'
"""
import copy
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

import e2e_coverage as viewer


class ReportIdentity(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.manifest = self.root / "test/e2e/coverage/example.json"
        self.manifest.parent.mkdir(parents=True)
        self.manifest.write_text('{"contract_id":"gpuworks/example"}')
        self.script = self.root / "test/e2e/example.py"
        self.script.write_text("def test_example(): pass\n")
        self.head = "a" * 40
        self.contract = {"id": "gpuworks/example", "manifest": "test/e2e/coverage/example.json",
                         "manifest_sha256": viewer.digest(self.manifest), "test_file": "test/e2e/example.py",
                         "test_sha256": viewer.digest(self.script), "required_evidence": ["ui", "request", "cleanup"]}
        self.report = {"contract": self.contract["id"], "status": "PASS", "source_sha": self.head,
                       "run_id": "20261021T010203Z-test", "working_tree": "clean", "locale": "en",
                       "test_sha256": viewer.digest(self.script), "coverage_manifest_sha256": viewer.digest(self.manifest),
                       "binary_sha256": "b" * 64, "retries": 0,
                       "evidence": [{"layer": layer} for layer in self.contract["required_evidence"]],
                       "remaining_resources": [], "browser_close_ok": True, "environment_cleanup_ok": True}

    def summarize(self, **values):
        report = {**copy.deepcopy(self.report), **values}
        return viewer.summarize_report(report, self.contract, self.root, self.head, False, 1, {})

    def write(self, name, report):
        p = self.root / "reports" / name / "report.json"
        p.parent.mkdir(parents=True)
        p.write_text(json.dumps(report))
        return p

    def test_current_clean_identity_and_http_hash_map_schema(self):
        self.assertEqual(self.summarize()["state"], "current")
        report = copy.deepcopy(self.report)
        report.pop("test_sha256")
        report.pop("coverage_manifest_sha256")
        report["source_hashes"] = {self.contract["manifest"]: self.contract["manifest_sha256"],
                                   self.contract["test_file"]: self.contract["test_sha256"]}
        result = viewer.summarize_report(report, self.contract, self.root, self.head, False, 1, {})
        self.assertEqual(result["state"], "current")

    def test_old_source_dirty_mutation_and_missing_evidence_never_current(self):
        self.assertEqual(self.summarize(source_sha="c" * 40)["state"], "historical")
        self.assertEqual(self.summarize(working_tree="dirty")["state"], "incomplete")
        self.assertEqual(self.summarize(product_mutation="broken-routing")["state"], "mutation")
        for changes in [{"evidence": []}, {"remaining_resources": ["container-id"]},
                        {"remaining_resources": {}}, {"browser_close_ok": False},
                        {"test_sha256": "x"}, {"retries": 1}, {"retries": False}]:
            with self.subTest(changes=changes):
                self.assertEqual(self.summarize(**changes)["state"], "incomplete")

    def test_dirty_viewed_worktree_and_conflicting_registry_not_passed(self):
        dirty = viewer.summarize_report(self.report, self.contract, self.root, self.head, True, 1, {})
        self.assertEqual(dirty["state"], "uncommitted")
        self.contract["conflict"] = True
        self.assertEqual(self.summarize()["state"], "incomplete")

    def test_all_declared_hashes_and_local_binary_are_checked(self):
        self.report["source_sha256"] = {"test/e2e/example.py": "e" * 64}
        self.assertEqual(self.summarize()["state"], "incomplete")
        self.report.pop("source_sha256")
        result = viewer.summarize_report(self.report, self.contract, self.root, self.head, False, 1,
                                         {"binary_sha256": "c" * 64})
        self.assertEqual(result["state"], "incomplete")
        self.assertIs(result["binary_checked"], False)

    def test_source_fingerprint_paths_cannot_escape_or_read_private_data(self):
        self.report["source_hashes"] = {"../../outside": "e" * 64, "secrets/key": "e" * 64}
        self.assertEqual(self.summarize()["state"], "incomplete")
        self.assertIsNone(viewer.source_file(self.root, "/etc/passwd"))

    def test_real_report_helper_product_maps_and_scalar_digest_bindings(self):
        fields = {"fixture_sha256": "fixture.py", "browser_case_sha256": "browser.py",
                  "replica_fixture_sha256": "replica.py"}
        lines = ["report = {"]
        for key, name in fields.items():
            path = self.root / "test/e2e" / name
            path.write_text("# source bytes\n")
            self.report[key] = viewer.digest(path)
            lines.append(f'"{key}": digest(ROOT / "test/e2e/{name}"),')
        lines.append("}")
        self.script.write_text("\n".join(lines))
        self.contract["test_sha256"] = self.report["test_sha256"] = viewer.digest(self.script)
        self.report["helper_sha256"] = {"test/e2e/fixture.py": self.report["fixture_sha256"]}
        self.report["product_sha256"] = {"test/e2e/browser.py": self.report["browser_case_sha256"]}
        self.assertEqual(self.summarize()["state"], "current")
        for key in [*fields, "helper_sha256", "product_sha256"]:
            before = self.report[key]
            self.report[key] = ({name: "e" * 64 for name in before} if isinstance(before, dict) else "e" * 64)
            with self.subTest(key=key):
                self.assertEqual(self.summarize()["state"], "incomplete")
            self.report[key] = before

    def test_unknown_source_digest_is_unverified_and_binary_map_is_supported(self):
        self.assertEqual(self.summarize(unknown_source_sha256="f" * 64)["state"], "incomplete")
        self.contract["lane"] = "local-real-agent-docker"
        self.assertEqual(self.summarize()["state"], "incomplete")
        self.assertEqual(self.summarize(binary_sha256={"server": "b" * 64, "agent": "c" * 64})["state"], "current")

    def test_replica_model_asset_map_uses_declared_fixture_directory(self):
        declaration = self.root / "test/e2e/deployment_lifecycle.py"
        declaration.write_text('MODEL_DIR = ROOT / "test/e2e/testdata/fake-model"\n')
        model = self.root / "test/e2e/testdata/fake-model/config.json"
        model.parent.mkdir(parents=True)
        model.write_text('{"model_type":"fixture"}')
        self.report["fixture_model_file_sha256"] = {"config.json": viewer.digest(model)}
        self.assertEqual(self.summarize()["state"], "current")
        model.write_text('{"model_type":"changed"}')
        self.assertEqual(self.summarize()["state"], "incomplete")

    def test_scan_failure_and_missing_cleanup_proof_never_current_pass(self):
        self.assertEqual(self.summarize(artifact_scan="FAIL: synthetic-secret")["state"], "incomplete")
        self.assertEqual(self.summarize(cleanup_steps=None)["state"], "incomplete")
        for key in ["browser_close_ok", "environment_cleanup_ok", "remaining_resources"]:
            report = copy.deepcopy(self.report)
            report.pop(key)
            with self.subTest(missing=key):
                result = viewer.summarize_report(report, self.contract, self.root, self.head, False, 1, {})
                self.assertEqual(result["state"], "incomplete")

    def test_new_unidentified_failure_remains_newest(self):
        self.write("old-pass", self.report)
        failure = {**self.report, "status": "FAIL_TEST", "run_id": "20261022T010203Z-test"}
        failure.pop("source_sha")
        self.write("new-fail", failure)
        data = {"contracts": [self.contract], "warnings": []}
        viewer.attach_reports(data, [self.root / "reports"], self.root, self.head)
        self.assertEqual(self.contract["runs"][0]["status"], "FAIL_TEST")
        self.assertEqual(self.contract["runs"][0]["state"], "incomplete")

    def test_selection_does_not_assign_one_case_pass_to_another(self):
        self.report.pop("contract")
        self.report["selection"] = [self.contract["id"], "gpuworks/other"]
        self.write("selection-only", self.report)
        data = {"contracts": [self.contract], "warnings": []}
        totals = viewer.attach_reports(data, [self.root / "reports"], self.root, self.head)
        self.assertEqual(totals["matched"], 0)
        self.assertEqual(self.contract["runs"], [])

    def test_newer_failure_stays_visible_and_private_and_symlinks_are_skipped(self):
        self.write("old", self.report)
        failure = {**self.report, "status": "FAIL_TEST", "run_id": "20261022T010203Z-test"}
        path = self.write("new", failure)
        self.write("private/hidden", {**self.report, "run_id": "20261023T010203Z-hidden"})
        (self.root / "reports/link.json").symlink_to(path)
        data = {"contracts": [self.contract], "warnings": []}
        totals = viewer.attach_reports(data, [self.root / "reports", path], self.root, self.head)
        self.assertEqual(totals["matched"], 2)
        self.assertEqual(self.contract["runs"][0]["status"], "FAIL_TEST")
        self.assertEqual(len(self.contract["runs"]), 2)

    def test_malformed_reports_visible_as_warning_without_raw_error_text(self):
        path = self.write("bad", [])
        secret = "SYNTHETIC_SECRET_never_export"
        path.write_text('{"api_key":"' + secret + '"')
        data = {"contracts": [self.contract], "warnings": []}
        viewer.attach_reports(data, [self.root / "reports"], self.root, self.head)
        self.assertEqual(len(data["warnings"]), 1)
        self.assertNotIn(secret, json.dumps(data))

    def test_export_allowlist_drops_requests_secrets_and_raw_evidence(self):
        secret = "SYNTHETIC_SECRET_never_export"
        self.report.update(api_key=secret, failure=secret, command=[secret], base_url=secret)
        self.report["evidence"][0]["body"] = {"Authorization": secret}
        self.assertNotIn(secret, json.dumps(self.summarize()))

    def test_embedded_json_cannot_escape_script(self):
        payload = {"warnings": ["</script><script>window.injected=true</script>"]}
        html = viewer.render(payload)
        self.assertNotIn("<script>window.injected", html)
        start = html.index('<script type="application/json" id="coverage-data">')
        end = html.index("</script>", start)
        self.assertIn("\\u003c/script>", html[start:end])

    def test_cli_is_read_only_supports_literal_env_paths_and_rejects_source_overwrite(self):
        env = {k: v for k, v in os.environ.items() if not k.startswith(("GIT_", "E2E_"))}
        env.update(GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull)
        def git(*args):
            return subprocess.check_output(["git", "-C", str(self.root), *args], env=env, text=True).strip()
        self.manifest.write_text(json.dumps({"contract_id": self.contract["id"], "test_file": self.contract["test_file"],
                                            "test_function": "test_example", "required_evidence": ["ui", "request", "cleanup"]}))
        (self.root / ".gitignore").write_text("secrets/\n")
        tracked = self.root / "source.html"
        tracked.write_text("never overwrite source")
        git("init", "--quiet")
        git("add", ".")
        git("-c", "user.name=Coverage test", "-c", "user.email=coverage@example.invalid",
            "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "fixture")
        folder = self.root / 'secrets/e2e/reports "$(not-a-command)'
        folder.mkdir(parents=True)
        report = {**self.report, "source_sha": git("rev-parse", "HEAD"),
                  "coverage_manifest_sha256": viewer.digest(self.manifest)}
        (folder / "report.json").write_text(json.dumps(report))
        output = self.root / 'secrets/e2e/page "$(not-a-command).html'
        env.update(E2E_REPORT_DIR=str(folder), E2E_COVERAGE_OUTPUT=str(output))
        command = [sys.executable, str(Path(viewer.__file__).resolve()), "--root", str(self.root)]
        result = subprocess.run(command, env=env, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(output.exists())
        self.assertEqual(git("status", "--porcelain"), "")
        result = subprocess.run([*command, "--output", str(tracked)], env=env, capture_output=True)
        self.assertEqual(result.returncode, 2)
        self.assertEqual(tracked.read_text(), "never overwrite source")


if __name__ == "__main__":
    unittest.main()
