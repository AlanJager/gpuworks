"""The new selection must retain the existing batch's failure and identity gates."""
import json
from pathlib import Path
import unittest
from unittest.mock import patch

import first_batch as batch
import second_batch
from test_first_batch import FirstBatchTests


class SecondBatchTests(FirstBatchTests):
    def test_explicit_selection_does_not_mutate_first_batch_defaults(self):
        args = self.configure_run()
        explicit = (("other", "example.py", "example.json", ("--case", "owned")),)
        original = batch.CASES
        self.assertEqual(batch.run(args, cases=explicit, kind="second-batch-index"), 0)
        self.assertEqual(batch.CASES, original)
        self.assertEqual(len(self.calls), 2)
        self.assertTrue(all(command[command.index("--case") + 1] == "owned" for command in self.calls))
        index = json.loads((args.artifact_dir / "batch-report.json").read_text())
        self.assertEqual(index["kind"], "second-batch-index")
        self.assertEqual([r["case"] for r in index["cases"]], ["other", "other"])

    def test_explicit_selection_still_stops_after_missing_child_report(self):
        args = self.configure_run("missing")
        explicit = (("other", "example.py", "example.json", ("--case", "owned")),)
        self.assertEqual(batch.run(args, cases=explicit, kind="second-batch-index"), 1)
        self.assertEqual(len(self.calls), 1)
        index = json.loads((args.artifact_dir / "batch-report.json").read_text())
        self.assertEqual([r["status"] for r in index["cases"]], ["FAIL", "NOT_RUN"])


class SelectionContractTests(unittest.TestCase):
    def test_all_seven_parameterized_contracts_have_matching_real_manifests(self):
        self.assertEqual(len(second_batch.CASES), 7)
        self.assertEqual(len({row[0] for row in second_batch.CASES}), 7)
        for name, driver, manifest, arguments in second_batch.CASES:
            with self.subTest(name=name):
                details = json.loads((batch.ROOT / "test/e2e/coverage" / manifest).read_text())
                self.assertEqual(details["driver_file"], "test/e2e/" + driver)
                self.assertEqual(arguments, ("--case", name))
                self.assertIn("agent", details["lane"])
                self.assertEqual(set(details["required_evidence"]), {"ui", "request", "persistence", "execution", "cleanup"})


if __name__ == "__main__":
    unittest.main()
