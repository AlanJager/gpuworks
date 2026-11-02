"""Native command-editor contracts shared by the six real deployment flows.

Run through each deployment driver with --check-family manual-editor. These
checks use browser keyboard/DOM input APIs and real evaluate requests. A held
request is continued to the server; no management response is substituted.
Source contract: DeployRuntimeCommand.tsx and deployHelpers.ts (trimmed command).
"""
import copy
import re
import shlex
import sys
from urllib.parse import urlsplit

from playwright.sync_api import expect
from platform_token_read_revoke import check


def utf16_length(value):
    """Native textarea offsets count UTF-16 code units, including astral text."""
    return len(value.encode('utf-16-le')) // 2


def manual_body(structured, script):
    expected = copy.deepcopy(structured)
    expected.setdefault('container_overrides', {}).update(
        command_mode='manual', command_includes_preparation=True,
        cmd_display=script.strip())
    return expected


def input_state(editor):
    return editor.evaluate('e => ({value:e.value, start:e.selectionStart, end:e.selectionEnd})')


def assert_input(editor, script, start, end, same, report, key):
    report['last_step'] = 'manual-editor-navigation-' + key
    expect(editor).to_have_value(script)
    expected = {'value': script, 'start': start, 'end': end}
    same(input_state(editor), expected, report['last_step'])
    report.setdefault('manual_editor_navigation', []).append({'key': key, **expected})


def pending_action(page, extra, custom, title, visible_button, report):
    """Inspect the actual pending state before releasing one real evaluation."""
    held = []
    pattern = re.compile(r'/api/v1/evaluate(?:\?|$)')

    def hold(route):
        if route.request.method == 'POST' and route.request.post_data_json.get('extra_args') == custom:
            held.append(route)
        else:
            route.continue_()

    page.route(pattern, hold)
    try:
        with page.expect_request(lambda request: request.method == 'POST'
                and urlsplit(request.url).path == '/api/v1/evaluate'
                and request.post_data_json.get('extra_args') == custom):
            extra.fill(custom)
        section = page.locator('.deploy-runtime-command')
        report['last_step'] = 'manual-editor-pending-readonly'
        expect(section.locator('pre')).to_have_text(title['generating'])
        expect(visible_button(section, title['edit'])).to_be_disabled()
        expect(section.locator('textarea')).to_have_count(0)
        check(len(held) == 1, 'editor pending probe did not hold exactly one real evaluate')
        report['manual_editor_pending'] = {'edit_disabled': True, 'textarea_count': 0,
            'guidance': title['generating'], 'held_method': 'POST', 'held_path': '/api/v1/evaluate'}
    finally:
        # Cleanup stages are independent. Keep the primary UI exception if one
        # is active, and retain every cleanup error in the report.
        primary_failure = sys.exc_info()[0] is not None
        cleanup_errors = []
        for index, route in enumerate(held):
            try:
                route.continue_()
            except Exception as error:
                cleanup_errors.append({'stage': 'continue', 'held_index': index, 'error': str(error)})
        try:
            page.unroute(pattern, hold)
        except Exception as error:
            cleanup_errors.append({'stage': 'unroute', 'error': str(error)})
        if cleanup_errors:
            report.setdefault('manual_editor_pending_cleanup_errors', []).extend(cleanup_errors)
            if not primary_failure:
                raise AssertionError('manual editor pending cleanup failed: ' + repr(cleanup_errors))


def structured_preview(page, title, report, launch, same):
    report['last_step'] = 'manual-editor-structured-readonly'
    section = page.locator('.deploy-runtime-command')
    expect(section.locator('textarea')).to_have_count(0)
    preview = section.locator('.deploy-rank-launch > pre')
    expect(preview).to_have_count(1)
    expect(preview).to_be_visible()
    command = shlex.split(preview.inner_text())
    same(command, launch['command'], 'manual editor complete structured preview command')
    check(preview.evaluate('e => e.isContentEditable') is False,
          'generated command preview is editable')
    expect(section.get_by_text(title['description'], exact=True)).to_be_visible()
    expect(section.get_by_text(title['dynamicBindings'], exact=True)).to_be_visible()
    report['manual_editor_structured'] = {'readonly': True, 'textarea_count': 0, 'command': command,
        'description': title['description'], 'dynamic_bindings': title['dynamicBindings']}


def navigation(page, editor, generated, structured, title, evaluation, same, report):
    """Explicit keystrokes have fixed offsets; DOM reads never define expected text."""
    report['last_step'] = 'manual-editor-initial-content'
    expect(editor).to_be_enabled()
    expect(editor).to_be_editable()
    expect(editor).to_have_value(generated)
    expect(page.locator('.deploy-runtime-command').get_by_text(
        title['manualWarning'], exact=True)).to_be_visible()
    check(generated.splitlines()[0] == 'set -eu', 'unexpected fixture shell policy')
    posts = []

    def observe(request):
        if request.method == 'POST' and urlsplit(request.url).path == '/api/v1/deployments':
            posts.append({'method': request.method, 'path': urlsplit(request.url).path,
                          'body': request.post_data_json})

    page.on('request', observe)
    try:
        editor.focus()
        editor.press('Control+Home')
        assert_input(editor, generated, 0, 0, same, report, 'ctrl-home')
        editor.press('ArrowRight')
        assert_input(editor, generated, 1, 1, same, report, 'arrow-right')
        editor.press('Home')
        assert_input(editor, generated, 0, 0, same, report, 'home')
        editor.press('End')
        assert_input(editor, generated, 7, 7, same, report, 'end')
        editor.press('Shift+ArrowLeft')
        assert_input(editor, generated, 6, 7, same, report, 'shift-selection')
        editor.press('ArrowRight')
        assert_input(editor, generated, 7, 7, same, report, 'selection-collapse')
        editor.press('Control+End')
        length = utf16_length(generated)
        assert_input(editor, generated, length, length, same, report, 'ctrl-end')
        editor.press('ArrowLeft')
        assert_input(editor, generated, length - 1, length - 1, same, report, 'arrow-left')
        report['last_step'] = 'manual-editor-navigation-tab'
        editor.press('Tab')
        expect(editor).not_to_be_focused()
        page.keyboard.press('Shift+Tab')
        expect(editor).to_be_focused()
        assert_input(editor, generated, length - 1, length - 1, same, report, 'shift-tab')
        editor.press('Control+Home')
        prefix = '# keyboard probe'

        def changed(action, script, key):
            record = evaluation(page, action, lambda body: body.get('container_overrides', {}).get(
                'cmd_display') == script.strip(), report, 'manual-editor-edit-' + key)
            same(record['request'], manual_body(structured, script), 'manual editor full wire ' + key)
            expect(editor).to_have_value(script)
            return record

        changed(lambda: page.keyboard.insert_text(prefix), prefix + generated, 'insert')
        changed(lambda: editor.press('Enter'), prefix + '\n' + generated, 'enter')
        changed(lambda: editor.press('Control+z'), prefix + generated, 'undo-enter')
        editor.press('Control+Home')
        for _ in prefix:
            editor.press('Shift+ArrowRight')
        assert_input(editor, prefix + generated, 0, len(prefix), same, report, 'select-inserted-text')
        changed(lambda: editor.press('Delete'), generated, 'delete-restore')
        same(posts, [], 'native editor keyboard unexpectedly created a deployment')
        report['manual_editor_contract'] = {'initial_script': generated,
            'warning': title['manualWarning'], 'editable': True,
            'inserted_text': prefix, 'enter_inserts_newline': True, 'undo_verified': True,
            'tab_leaves_and_shift_tab_returns': True, 'original_text_restored': True,
            'create_posts': posts, 'scope': 'native shell textarea; no YAML/schema claim'}
    finally:
        page.remove_listener('request', observe)
