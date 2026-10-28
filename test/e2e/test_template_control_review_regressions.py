"""Offline review regressions; no browser or product execution evidence."""
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import template_control_checks as checks


class ClipboardPage:
    def __init__(self, document, copy_enabled=True):
        self.document, self.copy_enabled = document, copy_enabled
        self.clipboard = document
        self.selected = False
        self.events = []
        self.keyboard = self

    def evaluate(self, expression, *args):
        if expression == '(value) => navigator.clipboard.writeText(value)' and len(args) == 1:
            self.clipboard = args[0]
            return
        if expression == 'navigator.clipboard.readText()' and not args:
            return self.clipboard
        raise AssertionError('unexpected script; DOM injection forbidden')

    def press(self, key):
        self.events.append(key)
        if key == 'Control+A':
            self.selected = True
        elif key == 'Control+C':
            if not self.selected:
                raise AssertionError('copy did not select whole document')
            if self.copy_enabled:
                self.clipboard = self.document
        elif key == 'ArrowLeft':
            self.selected = False
        else:
            raise AssertionError('unexpected editor mutation key')


class ClipboardEditor:
    def __init__(self, page):
        self.page = page

    def click(self):
        self.page.events.append('editor click')

    def inner_text(self):
        raise AssertionError('virtualized innerText cannot prove full document bytes')


class ExactDocumentTests(unittest.TestCase):
    def test_complete_document_copy_preserves_indentation_newlines_and_clears_selection(self):
        document = 'models:\n  - model_id: e2e-long/template\n' + '    # another line\n' * 500
        page = ClipboardPage(document)
        self.assertEqual(checks.editor_bytes(ClipboardEditor(page), document), document)
        self.assertEqual(page.events, ['editor click', 'Control+A', 'Control+C', 'ArrowLeft'])
        self.assertFalse(page.selected, 'copy scaffolding must not consume the next Escape')

    def test_normalized_whitespace_equivalence_is_insufficient(self):
        canonical = 'models:\n  - model_id: e2e-test/template\n'
        for changed in (canonical.replace('  -', ' -'), canonical.replace('\n', ' '),
                        canonical.replace('\n', '\r\n'), canonical.rstrip('\n')):
            with self.subTest(changed=changed), self.assertRaisesRegex(AssertionError, 'YAML bytes differ'):
                checks.editor_bytes(ClipboardEditor(ClipboardPage(changed)), canonical)

    def test_stale_matching_clipboard_cannot_hide_failed_copy(self):
        canonical = 'models:\n  - model_id: e2e-test/template\n'
        page = ClipboardPage(canonical, copy_enabled=False)
        with self.assertRaisesRegex(AssertionError, 'YAML bytes differ'):
            checks.editor_bytes(ClipboardEditor(page), canonical)


class CloneEntryTests(unittest.TestCase):
    def fixture(self, *, mutate_on_close=False, changed_db=False):
        events, mutations = [], [('POST', checks.API + '/clone')]
        state = {'closed': False}
        class Close:
            def click(self):
                events.append('close automatic drawer')
                state['closed'] = True
                if mutate_on_close:
                    mutations.append(('POST', checks.API + '/import'))
        class Dialog:
            def locator(self, selector):
                if selector != '.ant-drawer-close':
                    raise AssertionError('unexpected locator')
                return Close()
        dialog = Dialog()
        class Expect:
            def __init__(self, value):
                if value is not dialog:
                    raise AssertionError('unexpected focus expectation')
            def not_to_be_visible(self):
                if not state['closed']:
                    raise AssertionError('automatic drawer still open')
                events.append('automatic drawer hidden')
        baseline = {'model_id': 'e2e-test/template-copy', 'source': 'user'}
        def read_model(*args):
            if not state['closed']:
                raise AssertionError('baseline read before auto-close')
            events.append('independent model')
            return baseline
        def read_yaml(*args):
            events.append('independent YAML')
            return 'canonical\n'
        def read_db(*args):
            events.append('independent SQLite')
            return 'changed' if changed_db else 'stored'
        def actual_open(*args):
            events.append('actual Edit click')
            return ('actual-trigger', 'actual-dialog', 'actual-editor')
        namespace = dict(editor_ready=lambda *args: (dialog, 'auto-editor'),
            editor_bytes=lambda *args: events.append('automatic exact bytes'), expect=Expect,
            model=read_model, yaml_read=read_yaml, db_snapshot=read_db, open_editor=actual_open)
        return events, mutations, baseline, namespace

    def run_entry(self, baseline, mutations, report):
        return checks.reopen_clone_editor(None, SimpleNamespace(base='http://local'), report,
            {'flat': {'editYAML': 'editor'}}, baseline['model_id'], baseline, 'canonical\n', 'stored', mutations)

    def test_auto_close_then_independent_readback_before_genuine_edit_entry(self):
        events, mutations, baseline, namespace = self.fixture()
        report = {}
        with patch.multiple(checks, **namespace):
            self.assertEqual(self.run_entry(baseline, mutations, report),
                             ('actual-trigger', 'actual-dialog', 'actual-editor'))
        self.assertEqual(events, ['automatic exact bytes', 'close automatic drawer', 'automatic drawer hidden',
            'independent model', 'independent YAML', 'independent SQLite', 'actual Edit click'])
        self.assertFalse(report['clone_auto_drawer_closed']['focus_return_claimed'])

    def test_auto_close_mutation_or_changed_persistence_fails_before_edit_click(self):
        for kwargs in ({'mutate_on_close': True}, {'changed_db': True}):
            events, mutations, baseline, namespace = self.fixture(**kwargs)
            with self.subTest(kwargs=kwargs), patch.multiple(checks, **namespace), self.assertRaises(AssertionError):
                self.run_entry(baseline, mutations, {})
            self.assertNotIn('actual Edit click', events)

    def test_open_editor_uses_actual_click_and_never_focuses_edit_manually(self):
        class Trigger:
            def __init__(self):
                self.clicked = False
            def click(self):
                self.clicked = True
            def focus(self):
                raise AssertionError('focus injection cannot establish user entry')
        trigger = Trigger()
        editor = SimpleNamespace(evaluate=lambda script: True)
        def ready(*args):
            self.assertTrue(trigger.clicked)
            return 'dialog', editor
        with patch.object(checks, 'action', return_value=trigger), patch.object(checks, 'editor_ready', side_effect=ready):
            self.assertEqual(checks.open_editor(None, {'flat': {'edit': 'Edit', 'editYAML': 'editor'}}),
                             (trigger, 'dialog', editor))


if __name__ == '__main__':
    unittest.main()
