"""Offline negative checks; real browser acceptance remains separate.

Run: PYTHONPATH=test/e2e python3 -B -m unittest test_deployment_editor_checks -v
"""
import copy
import unittest
from unittest.mock import MagicMock, patch

import deployment_editor_checks as editor
import deployment_launch_checks as launch


class EditorContract(unittest.TestCase):
    def test_manual_wire_preserves_complete_structured_body(self):
        source = {'quick_params': {'enabled': False, 'count': 0}, 'nnodes': 1,
                  'container_overrides': {'env': {'EXAMPLE': 'literal'}, 'shm_bytes': 268435456}}
        before = copy.deepcopy(source)
        expected = {**source, 'container_overrides': {**source['container_overrides'],
            'command_mode': 'manual', 'command_includes_preparation': True,
            'cmd_display': 'set -eu\nexec example'}}
        launch.same(editor.manual_body(source, '\nset -eu\nexec example\n'), expected, 'full manual request')
        self.assertEqual(source, before)
        for key, wrong in [('command_mode', 'engine'), ('command_includes_preparation', 1),
                           ('cmd_display', 'set -eu exec example')]:
            broken = copy.deepcopy(expected)
            broken['container_overrides'][key] = wrong
            with self.assertRaises(AssertionError):
                launch.same(editor.manual_body(source, '\nset -eu\nexec example\n'), broken, key)

    def test_utf16_selection_does_not_count_as_python_codepoints(self):
        self.assertEqual(editor.utf16_length('set -eu\n# 中文😀'), 14)

    def test_selection_or_text_drift_fails_specific_navigation_step(self):
        control, report = MagicMock(), {}
        with patch.object(editor, 'expect'):
            control.evaluate.return_value = {'value': 'set -eu', 'start': 1, 'end': 1}
            with self.assertRaisesRegex(AssertionError, 'manual-editor-navigation-home'):
                editor.assert_input(control, 'set -eu', 0, 0, launch.same, report, 'home')
        self.assertEqual(report['last_step'], 'manual-editor-navigation-home')
        self.assertNotIn('manual_editor_navigation', report)

    def test_pending_assertion_failure_continues_real_request_and_unroutes(self):
        page, field, route, assertion = [MagicMock() for _ in range(4)]
        route.request.method = 'POST'
        route.request.post_data_json = {'extra_args': '--host 0.0.0.0'}
        page.route.side_effect = lambda pattern, handler: handler(route)
        assertion.to_have_text.side_effect = AssertionError('wrong generating guidance')
        with patch.object(editor, 'expect', return_value=assertion):
            with self.assertRaisesRegex(AssertionError, 'wrong generating guidance'):
                editor.pending_action(page, field, '--host 0.0.0.0',
                    {'generating': 'Generating command...', 'edit': 'Edit Command Text'}, MagicMock(), {})
        route.continue_.assert_called_once_with()
        page.unroute.assert_called_once()

    def test_pending_never_fulfills_management_response(self):
        page, field, route = [MagicMock() for _ in range(3)]
        route.request.method = 'POST'
        route.request.post_data_json = {'extra_args': '--host 0.0.0.0'}
        page.route.side_effect = lambda pattern, handler: handler(route)
        report = {}
        with patch.object(editor, 'expect'):
            editor.pending_action(page, field, '--host 0.0.0.0',
                {'generating': 'Generating command...', 'edit': 'Edit Command Text'}, MagicMock(), report)
        route.fulfill.assert_not_called()
        route.continue_.assert_called_once_with()
        self.assertEqual(report['manual_editor_pending']['held_path'], '/api/v1/evaluate')

    def test_multiple_held_routes_all_release_despite_first_failure(self):
        page, field, first, second = [MagicMock() for _ in range(4)]
        for route in (first, second):
            route.request.method = 'POST'
            route.request.post_data_json = {'extra_args': '--host 0.0.0.0'}
        page.route.side_effect = lambda pattern, handler: [handler(route) for route in (first, second)]
        first.continue_.side_effect = RuntimeError('first held request disconnected')
        report = {}
        with patch.object(editor, 'expect'):
            with self.assertRaisesRegex(AssertionError, 'exactly one real evaluate'):
                editor.pending_action(page, field, '--host 0.0.0.0',
                    {'generating': 'Generating command...', 'edit': 'Edit Command Text'}, MagicMock(), report)
        first.continue_.assert_called_once_with()
        second.continue_.assert_called_once_with()
        page.unroute.assert_called_once()
        self.assertEqual(report['manual_editor_pending_cleanup_errors'], [
            {'stage': 'continue', 'held_index': 0, 'error': 'first held request disconnected'}])

    def test_release_and_unroute_errors_preserve_primary_ui_failure(self):
        page, field, route, assertion = [MagicMock() for _ in range(4)]
        route.request.method = 'POST'
        route.request.post_data_json = {'extra_args': '--host 0.0.0.0'}
        page.route.side_effect = lambda pattern, handler: handler(route)
        assertion.to_have_text.side_effect = AssertionError('original UI guidance wrong')
        route.continue_.side_effect = RuntimeError('connection ended')
        page.unroute.side_effect = RuntimeError('page closed')
        report = {}
        with patch.object(editor, 'expect', return_value=assertion):
            with self.assertRaisesRegex(AssertionError, 'original UI guidance wrong'):
                editor.pending_action(page, field, '--host 0.0.0.0',
                    {'generating': 'Generating command...', 'edit': 'Edit Command Text'}, MagicMock(), report)
        self.assertEqual([row['stage'] for row in report['manual_editor_pending_cleanup_errors']],
                         ['continue', 'unroute'])

    def test_release_failure_without_primary_error_fails_probe(self):
        page, field, route = [MagicMock() for _ in range(3)]
        route.request.method = 'POST'
        route.request.post_data_json = {'extra_args': '--host 0.0.0.0'}
        page.route.side_effect = lambda pattern, handler: handler(route)
        route.continue_.side_effect = RuntimeError('release refused')
        with patch.object(editor, 'expect'):
            with self.assertRaisesRegex(AssertionError, 'pending cleanup failed'):
                editor.pending_action(page, field, '--host 0.0.0.0',
                    {'generating': 'Generating command...', 'edit': 'Edit Command Text'}, MagicMock(), {})
        page.unroute.assert_called_once()

    def test_structured_preview_matches_every_evaluated_shell_word(self):
        page, preview = MagicMock(), MagicMock()
        section = page.locator.return_value
        section.locator.side_effect = lambda selector: preview if selector == '.deploy-rank-launch > pre' else MagicMock()
        preview.inner_text.return_value = "engine serve '/model with spaces' --count 2"
        preview.evaluate.return_value = False
        title = {'description': 'description', 'dynamicBindings': 'bindings'}
        report = {}
        with patch.object(editor, 'expect'):
            editor.structured_preview(page, title, report,
                {'command': ['engine', 'serve', '/model with spaces', '--count', '2']}, launch.same)
            with self.assertRaisesRegex(AssertionError, 'complete structured preview command'):
                editor.structured_preview(page, title, report,
                    {'command': ['engine', 'serve', '/model with spaces', '--count', '3']}, launch.same)
        self.assertEqual(report['manual_editor_structured']['command'],
                         ['engine', 'serve', '/model with spaces', '--count', '2'])

    def test_editor_family_dispatch_reaches_manual_probe_without_numeric_dependency(self):
        report = {'deployment_check_family': 'manual-editor'}
        page, labels, body = MagicMock(), {}, {}
        with patch.object(launch, 'freeze') as freeze, \
                patch.object(launch, 'quick_expert') as quick, \
                patch.object(launch, 'manual_roundtrip') as manual:
            self.assertIsNone(launch.draft_checks(page, labels, body, report))
        freeze.assert_called_once_with(report)
        quick.assert_called_once_with(page, labels, body, report)
        manual.assert_called_once_with(page, labels, body, report)

    def test_existing_diagnostic_families_are_retained(self):
        self.assertEqual(launch.FAMILIES[:4], ('all', 'quick-expert', 'manual-roundtrip', 'launch-contract'))
        self.assertIn('manual-editor', launch.FAMILIES)
        self.assertIn('deployment_editor_checks.py', launch.SOURCE_FILES)


if __name__ == '__main__':
    unittest.main()
