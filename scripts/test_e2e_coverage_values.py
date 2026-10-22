"""Frozen value denominators must fail conservatively under drift and bad anchors."""
import copy
import hashlib
from pathlib import Path
import tempfile
import unittest

from e2e_coverage_components import components


class ValueCoverageTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        (self.root / "product.ts").write_text('export type Choice = "a" | "b";\n')
        (self.root / "case.py").write_text('def exercise():\n    checked(actual == expected, "saved selection")\n')
        self.declaration = {"id": "choice", "name": "选择项", "scope": "product", "universe": ["a", "b"],
            "basis": "Reviewed product union; source snapshot is authoritative for drift detection.",
            "sources": [{"file": "product.ts", "sha256": hashlib.sha256((self.root / "product.ts").read_bytes()).hexdigest()}],
            "covered": {"a": [{"file": "case.py", "function": "exercise", "assertion": "saved selection"}]}}

    def contract(self, identifier="case-a", declaration=None):
        return {"id": identifier, "name": identifier, "kind": "business", "module": "models",
                "manifest": identifier + ".json", "test_file": "case.py", "test_function": "exercise",
                "details": {"component_value_coverage": [copy.deepcopy(declaration or self.declaration)]}}

    def rows(self, *contracts):
        return [r for r in components(self.root, {"contracts": list(contracts or [self.contract()])}) if r["id"].startswith("values#")]

    def test_known_set_and_contract_binding(self):
        row, = self.rows()
        self.assertEqual((row["numerator"], row["denominator"], row["percent"]), (1, 2, 50))
        self.assertEqual(row["missing"], ["b"])
        self.assertEqual(row["value_contract_ids"], {"a": ["case-a"], "b": []})
        self.assertIn("人工冻结", row["basis"])

    def test_consistent_cases_union_values_without_double_counting(self):
        second = copy.deepcopy(self.declaration)
        second["covered"] = {"b": second["covered"]["a"]}
        row, = self.rows(self.contract(), self.contract("case-b", second))
        self.assertEqual((row["numerator"], row["denominator"]), (2, 2))
        self.assertEqual(row["value_contract_ids"], {"a": ["case-a"], "b": ["case-b"]})

    def test_changed_source_keeps_denominator_unknown(self):
        (self.root / "product.ts").write_text('export type Choice = "a" | "b" | "c";\n')
        row, = self.rows()
        self.assertIsNone(row["denominator"])
        self.assertEqual(row["covered"], [])
        self.assertEqual(row["value_contract_ids"], {})

    def test_conflicting_sets_never_shrink_to_one_hundred_percent(self):
        other = copy.deepcopy(self.declaration)
        other["universe"] = ["a"]
        row, = self.rows(self.contract(), self.contract("case-b", other))
        self.assertIsNone(row["percent"])

    def test_bad_or_missing_anchor_does_not_reduce_denominator(self):
        (self.root / "case.py").write_text('def exercise():\n    # saved selection\n    pass\n')
        row, = self.rows()
        self.assertEqual((row["numerator"], row["denominator"]), (0, 2))
        self.assertEqual(row["missing"], ["a", "b"])
        self.assertTrue(row["unknown"])

    def test_unused_nested_function_is_not_parent_assertion(self):
        (self.root / "case.py").write_text('def exercise():\n    def unused():\n        checked(actual == expected, "saved selection")\n')
        row, = self.rows()
        self.assertEqual(row["numerator"], 0)

    def test_literal_true_is_not_assertion_proof(self):
        (self.root / "case.py").write_text('def exercise():\n    checked(True, "saved selection")\n')
        row, = self.rows()
        self.assertEqual(row["numerator"], 0)

    def test_same_helper_and_exact_fstring_static_fragment(self):
        (self.root / "case.py").write_text('def exercise():\n    same(actual, expected, f"saved selection{detail}")\n')
        row, = self.rows()
        self.assertEqual(row["numerator"], 1)

    def test_value_outside_set_is_invalid_instead_of_dropped(self):
        self.declaration["covered"]["not-an-option"] = self.declaration["covered"]["a"]
        row, = self.rows()
        self.assertIsNone(row["denominator"])

    def test_duplicate_or_empty_set_is_unknown(self):
        for values in ([], ["a", "a"]):
            with self.subTest(values=values):
                self.declaration["universe"] = values
                row, = self.rows()
                self.assertIsNone(row["percent"])

    def test_non_string_universe_is_unknown_without_crashing(self):
        self.declaration["universe"] = ["a", {"invalid": "value"}]
        row, = self.rows()
        self.assertIsNone(row["denominator"])

    def test_wrong_function_cannot_borrow_another_functions_assertion(self):
        self.declaration["covered"]["a"][0]["function"] = "not_executed"
        row, = self.rows()
        self.assertEqual((row["numerator"], row["denominator"]), (0, 2))

    def test_escaped_source_path_cannot_validate_snapshot(self):
        self.declaration["sources"][0]["file"] = "../product.ts"
        row, = self.rows()
        self.assertIsNone(row["denominator"])

    def test_fixture_scope_is_explicit_not_all_product(self):
        self.declaration["scope"] = "fixture"
        row, = self.rows()
        self.assertIn("本 fixture", row["basis"])
        self.assertTrue(row["id"].endswith("/fixture"))

    def test_existing_but_uncalled_function_cannot_borrow_owner(self):
        (self.root / "case.py").write_text('def exercise():\n    pass\ndef never_called():\n    checked(actual == expected, "saved selection")\n')
        self.declaration["covered"]["a"][0]["function"] = "never_called"
        row, = self.rows()
        self.assertEqual((row["numerator"], row["denominator"]), (0, 2))
        self.assertEqual(row["value_contract_ids"], {"a": [], "b": []})

    def test_actual_same_file_helper_call_retains_binding(self):
        (self.root / "case.py").write_text('def exercise():\n    assert_ready()\ndef assert_ready():\n    checked(actual == expected, "saved selection")\n')
        self.declaration["covered"]["a"][0]["function"] = "assert_ready"
        row, = self.rows()
        self.assertEqual(row["numerator"], 1)

    def test_cross_file_orphan_is_not_an_entrypoint_assertion(self):
        (self.root / "orphan.py").write_text('def never_called():\n    checked(actual == expected, "saved selection")\n')
        self.declaration["covered"]["a"][0].update(file="orphan.py", function="never_called")
        row, = self.rows()
        self.assertEqual(row["numerator"], 0)

    def test_constant_expressions_and_literal_equality_helpers_do_not_count(self):
        for expression in ['checked(1 == 1, "saved selection")',
                           'checked(True and not False, "saved selection")',
                           'checked([1, 2] == [1, 2], "saved selection")',
                           'checked(len([1]) == 1, "saved selection")',
                           'checked(1 if True else 0, "saved selection")',
                           'same(1 == 1, True, "saved selection")',
                           'same([1], [1], "saved selection")']:
            with self.subTest(expression=expression):
                (self.root / "case.py").write_text('def exercise():\n    ' + expression + '\n')
                row, = self.rows()
                self.assertEqual(row["numerator"], 0)

    def test_literal_expected_value_can_be_compared_with_runtime_data(self):
        (self.root / "case.py").write_text('def exercise():\n    same("a", actual, "saved selection")\n')
        row, = self.rows()
        self.assertEqual(row["numerator"], 1)

    def helper_chain(self, entry, helper=None, suffix=''):
        helper = helper or 'def assert_ready():\n    checked(actual == expected, "saved selection")\n'
        (self.root / "case.py").write_text(entry + helper + suffix)
        self.declaration["covered"]["a"][0]["function"] = "assert_ready"
        return self.rows()

    def test_shadowed_helper_name_never_proves_execution(self):
        variants = {
            "parameter": 'def exercise(assert_ready):\n    assert_ready()\n',
            "local_lambda": 'def exercise():\n    assert_ready = lambda: None\n    assert_ready()\n',
            "import": 'def exercise():\n    from other_mod import assert_ready\n    assert_ready()\n',
        }
        for name, entry in variants.items():
            with self.subTest(variant=name):
                row, = self.helper_chain(entry)
                self.assertEqual((row["numerator"], row["denominator"]), (0, 2))

    def test_module_rebinding_after_def_never_proves_execution(self):
        row, = self.helper_chain('def exercise():\n    assert_ready()\n', suffix='\nassert_ready = lambda: None\n')
        self.assertEqual((row["numerator"], row["denominator"]), (0, 2))

    def test_shadowed_assertion_name_never_proves_coverage(self):
        variants = {
            "parameter": 'def exercise(checked):\n    checked(actual == expected, "saved selection")\n',
            "local_lambda": 'def exercise():\n    checked = lambda *args: None\n    checked(actual == expected, "saved selection")\n',
            "import": 'def exercise():\n    from helpers import checked\n    checked(actual == expected, "saved selection")\n',
        }
        for name, source in variants.items():
            with self.subTest(variant=name):
                (self.root / "case.py").write_text(source)
                row, = self.rows()
                self.assertEqual((row["numerator"], row["denominator"]), (0, 2))

    def test_dead_code_call_never_proves_execution(self):
        variants = {
            "after_return": 'def exercise():\n    return\n    assert_ready()\n',
            "after_raise": 'def exercise():\n    raise RuntimeError\n    assert_ready()\n',
            "if_false": 'def exercise():\n    if False:\n        assert_ready()\n',
            "if_not_true": 'def exercise():\n    if not True:\n        assert_ready()\n',
        }
        for name, entry in variants.items():
            with self.subTest(variant=name):
                row, = self.helper_chain(entry)
                self.assertEqual((row["numerator"], row["denominator"]), (0, 2))

    def test_dead_assertion_inside_helper_never_proves_coverage(self):
        variants = {
            "after_return": 'def assert_ready():\n    return\n    checked(actual == expected, "saved selection")\n',
            "if_false": 'def assert_ready():\n    if False:\n        checked(actual == expected, "saved selection")\n',
        }
        for name, helper in variants.items():
            with self.subTest(variant=name):
                row, = self.helper_chain('def exercise():\n    assert_ready()\n', helper)
                self.assertEqual((row["numerator"], row["denominator"]), (0, 2))

    def test_live_branches_still_prove_execution(self):
        variants = {
            "if_true": 'def exercise():\n    if True:\n        assert_ready()\n',
            "unknown_condition": 'def exercise(flag):\n    if flag:\n        assert_ready()\n',
            "loop": 'def exercise():\n    for _ in range(2):\n        assert_ready()\n',
        }
        for name, entry in variants.items():
            with self.subTest(variant=name):
                row, = self.helper_chain(entry)
                self.assertEqual(row["numerator"], 1)

    def test_literal_grammar_variants_are_not_assertion_proof(self):
        for expression in ['checked(f"{1}", "saved selection")',
                           'checked([True][0], "saved selection")',
                           'checked("abc"[:2] == "ab", "saved selection")',
                           'checked((x := 1) == 1, "saved selection")',
                           'checked(*[True], "saved selection")']:
            with self.subTest(expression=expression):
                (self.root / "case.py").write_text('def exercise():\n    ' + expression + '\n')
                row, = self.rows()
                self.assertEqual(row["numerator"], 0)

    def test_constant_by_construction_is_not_assertion_proof(self):
        for expression in ['checked({**{"k": 1}} == {"k": 1}, "saved selection")',
                           'checked("ab".upper() == "AB", "saved selection")',
                           'same({"k": 1} | {}, {"k": 1}, "saved selection")']:
            with self.subTest(expression=expression):
                (self.root / "case.py").write_text('def exercise():\n    ' + expression + '\n')
                row, = self.rows()
                self.assertEqual(row["numerator"], 0)

    def test_except_as_and_match_capture_never_prove_execution(self):
        variants = {
            "except_as_inside": 'def exercise():\n    try:\n        pass\n    except Exception as assert_ready:\n        assert_ready()\n',
            "except_as_after": 'def exercise():\n    try:\n        pass\n    except Exception as assert_ready:\n        pass\n    assert_ready()\n',
            "match_capture_inside": 'def exercise(hint):\n    match hint:\n        case assert_ready:\n            assert_ready()\n',
            "match_capture_after": 'def exercise(hint):\n    match hint:\n        case assert_ready:\n            pass\n    assert_ready()\n',
        }
        for name, entry in variants.items():
            with self.subTest(variant=name):
                row, = self.helper_chain(entry)
                self.assertEqual((row["numerator"], row["denominator"]), (0, 2))

    def test_global_rebinding_or_del_never_proves_execution(self):
        variants = {
            "global_in_helper": ('def exercise():\n    assert_ready()\n',
                'def assert_ready():\n    global assert_ready\n    assert_ready = lambda: None\n'
                '    checked(actual == expected, "saved selection")\n', ''),
            "global_in_other_function": ('def exercise():\n    assert_ready()\n', None,
                '\ndef evil():\n    global assert_ready\n    assert_ready = lambda: None\n'),
            "module_del": ('def exercise():\n    assert_ready()\n', None, '\ndel assert_ready\n'),
            "function_del": ('def exercise():\n    del assert_ready\n    assert_ready()\n', None, ''),
            "augassign": ('def exercise():\n    assert_ready += 1\n    assert_ready()\n', None, ''),
        }
        for name, (entry, helper, suffix) in variants.items():
            with self.subTest(variant=name):
                row, = self.helper_chain(entry, helper, suffix)
                self.assertEqual((row["numerator"], row["denominator"]), (0, 2))

    def test_loop_and_jump_dead_code_never_proves_execution(self):
        variants = {
            "while_false": 'def exercise():\n    while False:\n        assert_ready()\n',
            "after_while_true": 'def exercise():\n    while True:\n        pass\n    assert_ready()\n',
            "after_break": 'def exercise(items):\n    for item in items:\n        break\n        assert_ready()\n',
            "after_continue": 'def exercise(items):\n    for item in items:\n        continue\n        assert_ready()\n',
        }
        for name, entry in variants.items():
            with self.subTest(variant=name):
                row, = self.helper_chain(entry)
                self.assertEqual((row["numerator"], row["denominator"]), (0, 2))

    def test_empty_literal_iterable_never_proves_execution(self):
        variants = {
            "empty_list": 'def exercise():\n    for item in []:\n        assert_ready()\n',
            "empty_tuple": 'def exercise():\n    for item in ():\n        assert_ready()\n',
            "empty_dict": 'def exercise():\n    for item in {}:\n        assert_ready()\n',
            "empty_string": 'def exercise():\n    for item in "":\n        assert_ready()\n',
            "empty_set_call": 'def exercise():\n    for item in set():\n        assert_ready()\n',
            "range_zero": 'def exercise():\n    for item in range(0):\n        assert_ready()\n',
            "range_equal_bounds": 'def exercise():\n    for item in range(1, 1):\n        assert_ready()\n',
            "range_descending": 'def exercise():\n    for item in range(5, 2):\n        assert_ready()\n',
            "range_negative_step_ascending": 'def exercise():\n    for item in range(2, 5, -1):\n        assert_ready()\n',
        }
        for name, entry in variants.items():
            with self.subTest(variant=name):
                row, = self.helper_chain(entry)
                self.assertEqual((row["numerator"], row["denominator"]), (0, 2))

    def test_nonempty_or_unknown_iterable_still_proves_execution(self):
        variants = {
            "fixture_loop": 'def exercise(data):\n    for storage in data["storages"]:\n        assert_ready()\n',
            "literal_loop": 'def exercise():\n    for item in [1, 2]:\n        assert_ready()\n',
            "range_call": 'def exercise():\n    for _ in range(2):\n        assert_ready()\n',
            "range_bounds": 'def exercise():\n    for _ in range(1, 3):\n        assert_ready()\n',
            "range_negative_step": 'def exercise():\n    for _ in range(5, 0, -1):\n        assert_ready()\n',
            "range_zero_step": 'def exercise():\n    for _ in range(0, 10, 0):\n        assert_ready()\n',
            "range_nonliteral": 'def exercise(count):\n    for _ in range(count):\n        assert_ready()\n',
        }
        for name, entry in variants.items():
            with self.subTest(variant=name):
                row, = self.helper_chain(entry)
                self.assertEqual(row["numerator"], 1)

    def test_real_full_wire_same_anchor_stays_scored(self):
        # Shape of test/e2e/deployment_acquisition_browser.py exercise(): the
        # full-wire same() anchor backing the two 100% provider rows sits after
        # a with expect_response block and a try/finally in the same block.
        (self.root / "case.py").write_text(
            'def exercise(page, data):\n'
            '    with page.expect_response(lambda r: r.request.method == "POST") as registered:\n'
            '        submit.click()\n'
            '    expected_register = {"source": data["provider"], "revision": REVISION}\n'
            '    same(registered.value.request.post_data_json, expected_register, "saved selection")\n'
            '    try:\n'
            '        deploy.click()\n'
            '    finally:\n'
            '        restore(data)\n'
            '    same(created.value.request.post_data_json, expected_body(data), "created wire")\n')
        row, = self.rows()
        self.assertEqual(row["numerator"], 1)

    def test_real_wire_state_anchor_stays_scored(self):
        # Shape of test/e2e/model_files_browser.py: entry loops over storages and
        # calls assert_ready(); the helper asserts subscript/compare facts and
        # guards a with expect_response reload block behind an unknown if.
        row, = self.helper_chain(
            'def exercise(page, data):\n'
            '    for storage in data["storages"]:\n'
            '        download(page, data, storage)\n'
            '        assert_ready(page, storage)\n',
            'def assert_ready(page, storage, reload=True):\n'
            '    row = document(page, storage)\n'
            '    checked(row["state"] == "present", "saved selection")\n'
            '    if reload:\n'
            '        with page.expect_response(lambda r: r.request.method == "GET") as refreshed:\n'
            '            page.reload()\n'
            '        checked(refreshed.value.status == 200, "detail reload failed")\n'
            '    return row\n')
        self.assertEqual(row["numerator"], 1)

    def test_array_or_object_scope_never_crashes_or_claims_full_coverage(self):
        for scope in ({}, []):
            with self.subTest(scope=scope):
                self.declaration["scope"] = scope
                row, = self.rows()
                self.assertIsNone(row["denominator"])
                self.assertTrue(row["unknown"])

    def test_nul_source_or_anchor_path_is_conservative(self):
        for position in ("source", "anchor"):
            with self.subTest(position=position):
                declaration = copy.deepcopy(self.declaration)
                if position == "source":
                    declaration["sources"][0]["file"] = "bad\x00source"
                else:
                    declaration["covered"]["a"][0]["file"] = "bad\x00anchor.py"
                row, = self.rows(self.contract(declaration=declaration))
                self.assertTrue(row["unknown"])
                self.assertNotEqual(row["percent"], 100)

    def test_conflict_contract_cannot_disappear_and_shrink_denominator(self):
        single = copy.deepcopy(self.declaration)
        single["universe"] = ["a"]
        conflicted = self.contract("case-b")
        conflicted["conflict"] = True
        row, = self.rows(self.contract(declaration=single), conflicted)
        self.assertIsNone(row["denominator"])
        self.assertEqual(row["value_contract_ids"], {})
        self.assertNotIn("case-b", row["contract_ids"])

    def test_duplicate_source_metric_also_poisons_other_contract(self):
        single = copy.deepcopy(self.declaration)
        single["universe"] = ["a"]
        conflicted = self.contract("case-b")
        conflicted["details"] = {}
        conflicted["conflict"] = True
        conflicted["duplicate_sources"] = [self.contract("case-b-duplicate")]
        row, = self.rows(self.contract(declaration=single), conflicted)
        self.assertIsNone(row["percent"])
        self.assertTrue(any("case-b-duplicate.json" in u for u in row["unknown"]))


if __name__ == "__main__":
    unittest.main()
