#!/usr/bin/env python3
"""Offline regression tests for derived E2E contract inventory."""
import hashlib
import json
import tempfile
import unittest
from pathlib import Path

from e2e_coverage_inventory import inventory


class InventoryTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.write("ui/src/App.tsx", '''const navGroups = [
          { items: [
            { key: "/models", icon: <X />, labelKey: "navigation.models" },
            { key: "/monitor", icon: <X />, labelKey: "navigation.monitor" },
            { key: "/gpus", icon: <X />, labelKey: "navigation.gpus" },
            { key: "/zstack", icon: <X />, labelKey: "navigation.privateCloud" },
            { key: "/apikeys", icon: <X />, labelKey: "navigation.apiKeys" },
          ] },
        ];''')
        self.write("ui/src/i18n/locales/zh/common.json", {"navigation": {
            "models": "模型", "monitor": "监控中心", "gpus": "GPU 设备",
            "privateCloud": "专有云", "apiKeys": "API 令牌"}})
        self.write("test/e2e/example.py", "raise RuntimeError('must not execute')\n")

    def write(self, name, value):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(value, ensure_ascii=False) if not isinstance(value, str) else value)
        return path

    def manifest(self, name, **extra):
        value = {"contract_id": "gpuworks/model/register", "test_file": "test/e2e/example.py",
                 "test_function": "register", "command": "python3 example.py", **extra}
        self.write("test/e2e/coverage/" + name + ".json", value)

    def test_nav_modules_without_contracts_remain_visible(self):
        self.manifest("single", page_id="models")
        result = inventory(self.root)
        modules = {item["id"]: item for item in result["modules"]}
        self.assertEqual(modules["monitor"]["name"], "监控中心")
        self.assertEqual(modules["zstack"]["paths"], ["/zstack"])
        self.assertIn("gpus", modules)
        self.assertFalse(any(row["module"] == "monitor" for row in result["contracts"]))
        self.assertFalse(any("percent" in key or key == "status" for row in result["contracts"] for key in row))
        self.assertEqual(result["warnings"], [])

    def test_nav_modules_come_from_the_navigation_module_when_it_exists(self):
        # The menu now lives in ui/src/navigation.tsx; App.tsx no longer has it.
        (self.root / "ui/src/App.tsx").write_text("export default function App() { return null; }\n")
        self.write("ui/src/navigation.tsx", '''export const navGroups = [
          { key: "overview", labelKey: "navigation.groupOverview", items: [
            { key: "/monitor", icon: <X />, labelKey: "navigation.monitor" },
          ] },
          { key: "resources", labelKey: "navigation.groupResources", items: [
            { key: "/gpus", icon: <X />, labelKey: "navigation.gpus" },
          ] },
        ];''')
        self.manifest("single", page_id="gpus")
        result = inventory(self.root)
        self.assertEqual(result["warnings"], [])
        ids = [module["id"] for module in result["modules"]]
        self.assertIn("monitor", ids)
        self.assertIn("gpus", ids)

    def test_cases_inherit_execution_and_preserve_parent_missing(self):
        self.manifest("nested", contract_id="gpuworks/model/umbrella", lane="offline-test",
                      required_evidence=["request", "persistence"], missing=["parent gap"],
                      cases={"register": {"id": "gpuworks/model/register", "missing": ["leaf gap"]},
                             "recovery": {"id": "gpuworks/model/recovery"}})
        rows = inventory(self.root)["contracts"]
        self.assertEqual(len(rows), 2)
        self.assertEqual(rows[0]["selection"], {"case": "register"})
        self.assertEqual(rows[0]["lane"], "offline-test")
        self.assertEqual(rows[0]["test_function"], "register")
        self.assertEqual(rows[0]["required_evidence"], ["request", "persistence"])
        self.assertEqual({item["text"] for item in rows[0]["missing"]}, {"parent gap", "leaf gap"})
        self.assertNotIn("gpuworks/model/umbrella", {row["id"] for row in rows})

    def test_platform_umbrella_is_not_counted_and_parameters_retained(self):
        self.manifest("token", contract_id="gpuworks/platform-token/options", page_id="api-tokens",
                      cases=[{"contract_id": "gpuworks/platform-token/options/read-0", "case": "read-0",
                              "parameters": {"scope": "read", "days": 0}, "command": "make token CASE=read-0"}])
        row, = inventory(self.root)["contracts"]
        self.assertEqual(row["module"], "apikeys")
        self.assertEqual(row["selection"], {"case": "read-0", "parameters": {"scope": "read", "days": 0}})
        self.assertEqual(row["command"], "make token CASE=read-0")

    def test_template_ids_and_selections_pair_by_index(self):
        self.manifest("template", contract_id=None, contract_ids=["gpuworks/model/clone", "gpuworks/model/syntax"],
                      parameters=["clone", "syntax"])
        rows = inventory(self.root)["contracts"]
        self.assertEqual([row["selection"] for row in rows], [{"parameter": "clone"}, {"parameter": "syntax"}])
        self.assertEqual([row["id"] for row in rows], ["gpuworks/model/clone", "gpuworks/model/syntax"])

    def test_selection_and_cross_references_are_not_additional_contracts(self):
        self.manifest("references", cases=[{"selection": "endpoint", "contract_id": "gpuworks/model/endpoint"}],
                      additional_cases=[{"contract_id": "gpuworks/model/not-an-executable-declaration"}],
                      fields=[{"related_contract": "gpuworks/model/referenced", "covered": ["exact request"]}])
        rows = inventory(self.root)["contracts"]
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]["selection"], {"case": "endpoint"})
        self.assertEqual([item["text"] for item in rows[0]["covered"]], ["exact request"])

    def test_contextual_explicit_claims_do_not_infer_evidence(self):
        self.manifest("claims", required_evidence=["execution"],
                      fields=[{"field_id": "model.name", "covered": ["trim"], "missing": ["Unicode"]}],
                      component_checklist=[{"item": "1.14", "status": "covered", "reason": "exact independent GET"},
                                           {"item": "7.4", "status": "MISSING", "reason": "expiry"}],
                      crud={"update": {"status": "N/A", "reason": "no editing"}},
                      checklist_items={"1.2": "PARTIAL: defaults; MISSING: async load"})
        row, = inventory(self.root)["contracts"]
        self.assertTrue(any("model.name" in item["path"] and item["text"] == "trim" for item in row["covered"]))
        self.assertTrue(any("1.14" in item["path"] and item["text"] == "exact independent GET" for item in row["covered"]))
        self.assertTrue(any(item["text"] == "expiry" for item in row["missing"]))
        self.assertFalse(any(item["text"] in {"execution", "no editing"} for item in row["covered"]))
        self.assertTrue(any("PARTIAL" in item["text"] for item in row["missing"]))

    def test_legacy_and_runner_are_not_business(self):
        self.write("test/e2e/coverage/legacy.json", {"entries": [
            {"entry_id": "legacy/old", "test_file": "test/e2e/example.py", "missing": ["source audit only"]}]})
        self.manifest("runner", lane="offline-runner-contract", cases=[
            {"contract_id": "gpuworks/model/runner", "case_id": "preflight"}])
        self.assertEqual({row["kind"] for row in inventory(self.root)["contracts"]}, {"legacy", "runner"})

    def test_duplicates_keep_both_gap_sets_and_are_marked_ambiguous(self):
        self.manifest("a", missing=["gap A"])
        self.manifest("b", missing=["gap B"])
        result = inventory(self.root)
        row, = result["contracts"]
        self.assertTrue(row["conflict"])
        self.assertEqual(len(row["duplicate_sources"]), 1)
        self.assertEqual({item["text"] for item in row["missing"]}, {"gap A", "gap B"})
        self.assertTrue(any("Duplicate contract ID" in warning for warning in result["warnings"]))

    def test_malformed_and_unknown_manifests_are_visible(self):
        self.write("test/e2e/coverage/broken.json", "{bad JSON")
        self.write("test/e2e/coverage/unknown.json", {"unrecognized": ["retained"]})
        result = inventory(self.root)
        self.assertEqual(len(result["manifest_files"]), 2)
        self.assertEqual(len(result["contracts"]), 2)
        self.assertTrue(all(row["module"] == "unclassified" and row["missing"] for row in result["contracts"]))
        self.assertEqual(len(result["warnings"]), 2)

    def test_hashes_are_file_bytes_and_unsafe_test_path_not_read(self):
        self.manifest("digest")
        row, = inventory(self.root)["contracts"]
        self.assertEqual(row["test_sha256"], hashlib.sha256((self.root / row["test_file"]).read_bytes()).hexdigest())
        self.assertEqual(row["manifest_sha256"], hashlib.sha256((self.root / row["manifest"]).read_bytes()).hexdigest())
        self.manifest("digest", test_file="/etc/passwd")
        result = inventory(self.root)
        self.assertEqual(result["contracts"][0]["test_sha256"], "")
        self.assertTrue(any("unsafe test file" in warning for warning in result["warnings"]))

    def test_repository_inventory_parses_every_manifest_without_warnings(self):
        root = Path(__file__).resolve().parents[1]
        result = inventory(root)
        actual = sorted(path.relative_to(root).as_posix() for path in (root / "test/e2e/coverage").glob("*.json"))
        self.assertEqual(result["manifest_files"], actual)
        self.assertEqual(result["warnings"], [])
        self.assertTrue(all(any(row["manifest"] == path for row in result["contracts"]) for path in actual))
        self.assertEqual(len({row["id"] for row in result["contracts"]}), len(result["contracts"]))
        self.assertEqual(len([row for row in result["contracts"] if row["manifest"].endswith("platform-token-options.json")]), 8)
        self.assertEqual(len([row for row in result["contracts"] if row["manifest"].endswith("template-edit.json")]), 3)
        self.assertEqual(len([row for row in result["contracts"] if row["kind"] == "runner"]), 2)
        self.assertFalse(any(row["id"] == "gpuworks/platform-token/options" for row in result["contracts"]))


if __name__ == "__main__":
    unittest.main()
