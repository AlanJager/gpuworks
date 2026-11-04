"""UNAPPLIED browser candidate for 12.2/12.4/12.6/15.1/15.3/15.7.

Only a released continue-only request barrier; no fabricated management response.
"""
from contextlib import closing
import copy
import hashlib
import json
import sqlite3
import re
import time
import traceback
import uuid
from urllib.parse import quote, urlsplit

from playwright.sync_api import expect
from local_browser_fixture import ROOT
from template_edit import action, editor_ready
from dialog_checks import assert_drawer_identity, assert_yaml_drawer_focus
from template_control_oracles import geometry, import_wire, changed_title, require, finish_stage

API = '/api/v1/inference-templates'


def model(page, base, identity):
    response = page.request.get(base + API + '/model/' + quote(identity, safe='/'))
    require(response.status == 200, f'model GET HTTP {response.status}')
    data = response.json()
    require(data['model_id'] == identity, 'independent model identity mismatch')
    return data


def yaml_read(page, base, identity):
    response = page.request.get(base + API + '/yaml', params={'model_id': identity})
    require(response.status == 200, f'YAML GET HTTP {response.status}')
    return response.text()


def db_snapshot(server, identity):
    path = server.work / 'server-data/gpuworks.db'
    connection = sqlite3.connect(path.as_uri() + '?mode=ro', uri=True, timeout=5)
    try:
        connection.row_factory = sqlite3.Row
        rows = connection.execute('SELECT * FROM inference_models WHERE model_id=?', (identity,)).fetchall()
        require(len(rows) == 1, 'SQLite model identity missing/duplicated')
        specs = connection.execute('SELECT * FROM inference_specs WHERE model_id=? ORDER BY id', (rows[0]['id'],)).fetchall()
        data = {'model': dict(rows[0]), 'specs': [dict(row) for row in specs]}
        return hashlib.sha256(json.dumps(data, sort_keys=True).encode()).hexdigest()
    finally:
        connection.close()


def package_read(page, server, identity):
    path = server.work / 'server-data/gpuworks.db'
    with closing(sqlite3.connect(path.as_uri() + '?mode=ro', uri=True)) as connection:
        row = connection.execute('SELECT package_digest FROM inference_models WHERE model_id=?', (identity,)).fetchone()
        require(row and re.fullmatch('[0-9a-f]{64}', row[0]), 'upstream package digest absent')
    response = page.request.get(server.base + API + '/export/' + row[0])
    require(response.status == 200 and response.headers['etag'] == '"' + row[0] + '"', 'source export contract drift')
    require(hashlib.sha256(response.body()).hexdigest() == row[0], 'source package content differs from immutable identity')
    return row[0], response.body()


def stored_title(server, identity, expected):
    path = server.work / 'server-data/gpuworks.db'
    with closing(sqlite3.connect(path.as_uri() + '?mode=ro', uri=True)) as connection:
        rows = connection.execute('SELECT display_title, source FROM inference_models WHERE model_id=?', (identity,)).fetchall()
        require(rows == [(expected, 'user')], 'actual SQLite title/source differs from submitted user template')


def measure(page, dialog, editor, save, reset):
    boxes = {name: locator.bounding_box() for name, locator in {
        'drawer': dialog, 'header': dialog.locator('.ant-drawer-header'),
        'title': dialog.locator('.ant-drawer-title'),
        'actions': dialog.locator('.ant-drawer-extra'),
        'body': dialog.locator('.ant-drawer-body'), 'editor': editor,
        'save': save, 'reset': reset}.items()}
    try:
        geometry(boxes, page.viewport_size)
    except AssertionError as reason:
        raise AssertionError(str(reason) + ' boxes=' + json.dumps(boxes)) from reason
    return boxes


def open_editor(page, t):
    trigger = action(page, t['flat']['edit'])
    trigger.click()
    dialog, editor = editor_ready(page, t['flat']['editYAML'])
    require(editor.evaluate('el => el.closest(".cm-editor") !== null'), 'real CodeMirror did not load')
    return trigger, dialog, editor


def editor_bytes(editor, expected):
    """Read the whole CodeMirror document via a real select-all/copy gesture.

    innerText can omit virtualized lines and string to_have_text normalizes
    whitespace. Neither is an exact YAML oracle. Only clipboard setup changes;
    no DOM, document content or focus-return target is injected.
    """
    page = editor.page
    editor.click()
    sentinel = 'e2e-clipboard-not-copied-' + uuid.uuid4().hex
    page.evaluate('(value) => navigator.clipboard.writeText(value)', sentinel)
    page.keyboard.press('Control+A')
    page.keyboard.press('Control+C')
    actual = page.evaluate('navigator.clipboard.readText()')
    # Collapse the read-only selection with a real navigation key: otherwise
    # CodeMirror may consume the matrix's next Escape to simplify selection.
    page.keyboard.press('ArrowLeft')
    if actual != expected:
        diverge = next((i for i, pair in enumerate(zip(actual, expected)) if pair[0] != pair[1]),
                       min(len(actual), len(expected)))
        require(False, 'whole editor YAML bytes differ (including indentation/newlines): '
                f'actual_len={len(actual)} expected_len={len(expected)} diverge_at={diverge} '
                f'actual_around={actual[max(0, diverge - 40):diverge + 40]!r} '
                f'expected_around={expected[max(0, diverge - 40):diverge + 40]!r} '
                f'actual_head={actual[:80]!r} sentinel_leaked={actual.startswith("e2e-clipboard-not-copied-")}')
    return actual


def editor_bytes_settled(editor, expected, timeout=10):
    """Byte-exact editor check that tolerates the CodeMirror typing latch.

    @uiw/react-codemirror defers external value updates (Reset, error restore)
    while the typing latch from the last real keystroke is still open, so the
    document settles one latch window later. Poll the same real copy gesture
    with a deadline instead of weakening the byte comparison.
    """
    deadline = time.monotonic() + timeout
    while True:
        try:
            return editor_bytes(editor, expected)
        except AssertionError as last:
            require(time.monotonic() < deadline,
                    'editor bytes never settled to expected content: ' + str(last))
            editor.page.wait_for_timeout(100)


def editor_bytes_stable(editor, expected, hold=1.5):
    """Require the editor to keep the expected bytes for a hold window.

    A single match cannot see a draft the product reverts one typing-latch
    window (200ms in @uiw/react-codemirror) after a failed save: the v5/v6
    error-restores-draft mutants passed both the instant read and the settle
    poll because the revert lands after the first successful read. hold is a
    stability window with active byte-exact polls, not a blind sleep.
    """
    editor_bytes(editor, expected)
    deadline = time.monotonic() + hold
    while time.monotonic() < deadline:
        editor.page.wait_for_timeout(100)
        editor_bytes(editor, expected)


def replace_editor(editor, value):
    expect(editor).to_have_attribute('contenteditable', 'true')
    editor.fill(value)
    editor_bytes(editor, value)


def reopen_clone_editor(page, server, report, t, identity, baseline, canonical, stored, mutations):
    """A clone's ?edit=1 auto-open has no Edit trigger to return focus to."""
    auto_dialog, auto_editor = editor_ready(page, t['flat']['editYAML'])
    editor_bytes(auto_editor, canonical)
    before = list(mutations)
    auto_dialog.locator('.ant-drawer-close').click()
    expect(auto_dialog).not_to_be_visible()
    require(mutations == before, 'closing clone auto-open drawer sent mutation')
    require(model(page, server.base, identity) == baseline and yaml_read(page, server.base, identity) == canonical
            and db_snapshot(server, identity) == stored, 'closing clone auto-open drawer changed persistence')
    report['clone_auto_drawer_closed'] = {'mutation': False, 'persistence_unchanged': True,
        'focus_return_claimed': False}
    # This genuine Edit click establishes the return target used by the matrix.
    return open_editor(page, t)


def keyboard_and_copy(page, context, editor, canonical):
    # Every content change is from real keys; evaluate below only reads selection.
    editor.focus()
    page.keyboard.press('Control+Home')
    page.keyboard.type('# e2e-keyboard')
    page.keyboard.press('Enter')
    editor_bytes(editor, '# e2e-keyboard\n' + canonical)
    page.keyboard.press('Control+Home')
    page.keyboard.press('End')
    page.keyboard.press('ArrowLeft')
    selection = editor.evaluate('''el => {
        const selection = getSelection();
        if (!selection || !selection.rangeCount) return null;
        const caret = selection.getRangeAt(0), prefix = document.createRange();
        prefix.selectNodeContents(el); prefix.setEnd(caret.startContainer, caret.startOffset);
        return prefix.toString();
    }''')
    require(selection == '# e2e-keyboar', 'Home/End/ArrowLeft navigation was swallowed')
    return editor_bytes(editor, '# e2e-keyboard\n' + canonical)


def dismiss_matrix(page, t, trigger, dialog, editor, canonical, changed, mutations):
    observations = []
    def dismiss(how):
        if how == 'close':
            dialog.locator('.ant-drawer-close').click()
        elif how == 'escape':
            editor.focus()
            page.keyboard.press('Escape')
        else:
            # rc-drawer mask is outside role=dialog. This is a real user click.
            dialog.locator('xpath=ancestor::div[contains(@class,"ant-drawer")][last()]').locator('.ant-drawer-mask').click(position={'x': 8, 'y': 8})
    for how in ('close', 'escape', 'mask'):
        baseline = list(mutations)
        dismiss(how)
        expect(dialog).not_to_be_visible()
        expect(trigger).to_be_focused()
        require(mutations == baseline, f'{how} clean close sent mutation')
        trigger.click()
        editor_bytes(editor, canonical)
        replace_editor(editor, changed)
        dismiss(how)
        child = page.get_by_role('dialog', name=t['editor']['discardTitle'], exact=True)
        expect(child).to_be_visible()
        action(child, t['editor']['keepEditing']).click()
        expect(child).not_to_be_visible()
        expect(dialog).to_be_visible()
        editor_bytes(editor, changed)
        require(mutations == baseline, f'{how} keep editing sent mutation')
        dismiss(how)
        expect(child).to_be_visible()
        action(child, t['editor']['keepEditing']).focus()
        page.keyboard.press('Escape')
        expect(child).not_to_be_visible()
        expect(dialog).to_be_visible()
        editor_bytes(editor, changed)
        require(mutations == baseline, f'{how} nested Escape sent mutation')
        dismiss(how)
        expect(child).to_be_visible()
        action(child, t['editor']['discard']).click()
        expect(child).not_to_be_visible()
        expect(child.get_by_role('button').first).not_to_be_visible()
        expect(dialog).not_to_be_visible()
        expect(trigger).to_be_focused()
        require(mutations == baseline, f'{how} discard sent mutation')
        trigger.click()
        # Deliberate product contract (ui/tests/inferenceTemplatesFlatPages.test.tsx
        # "reopen keeps the draft for a second look"): discard closes the drawer
        # without saving but keeps the in-memory draft; only Reset restores the
        # saved YAML. The modal wording tension is recorded in the run report.
        editor_bytes(editor, changed)
        action(dialog, t['editor']['reset']).click()
        editor_bytes_settled(editor, canonical)
        observations.append({'dismiss': how, 'clean': True, 'keep': True, 'discard': True})
    return observations


def submit_real(page, cdp, request_ids, save, expected, status, report):
    """Hold one outbound request, assert loading/no duplicate, then real server.

    The barrier never changes method/body/headers/response. Every captured route
    is released even on assertion failure; the original exception remains primary.
    """
    held, released = [], set()
    pattern = re.compile(re.escape(API + '/import') + r'(?:\?|$)')
    def barrier(route):
        if route.request.method == 'POST':
            held.append(route)
        else:
            route.continue_()
    def release(route):
        if id(route) not in released:
            route.continue_()
            released.add(id(route))
        return True
    before = len(request_ids)
    page.route(pattern, barrier)
    try:
        save.click()
        expect(save).to_have_class(re.compile(r'\bant-btn-loading\b'))
        save.click()
        require(len(held) == 1, 'duplicate import while real request barrier held')
        with page.expect_response(lambda r: urlsplit(r.url).path == API + '/import' and r.request.method == 'POST') as result:
            release(held[0])
        response = result.value
    except Exception:
        report.setdefault('failure', traceback.format_exc())
        raise
    finally:
        for index, route in enumerate(held):
            finish_stage(report, f'import barrier release {index}', lambda route=route: release(route))
        finish_stage(report, 'remove import barrier', lambda: (page.unroute(pattern, barrier), True)[1])
    require(len(request_ids) == before + 1, 'save did not send exactly one import')
    body = cdp.send('Network.getRequestPostData', {'requestId': request_ids[-1]})['postData']
    import_wire(response.request.method, response.url, response.request.headers['content-type'], body, expected)
    require(response.status == status, f'real import HTTP {response.status}, expected {status}')
    report.setdefault('real_imports', []).append({'status': status, 'held': 1, 'duplicate_posts': 0,
        'body_sha256': hashlib.sha256(body.encode()).hexdigest()})
    return response


def save_real(page, cdp, request_ids, dialog, save, expected, report):
    response = submit_real(page, cdp, request_ids, save, expected, 200, report)
    require(response.json()['models'] == 1, 'save envelope drift')
    expect(dialog).not_to_be_visible()
    return {'method': 'POST', 'path': API + '/import', 'query': {'overwrite': 'true'},
            'body_sha256': hashlib.sha256(expected.encode()).hexdigest(), 'status': response.status}


def exercise(page, context, server, artifacts, report, upstream_id):
    t = json.loads((ROOT / 'ui/src/i18n/locales' / report['locale'] / 'inferenceTemplates.json').read_text())
    auth = json.loads((ROOT / 'ui/src/i18n/locales' / report['locale'] / 'auth.json').read_text())
    page.goto(server.base + '/inference-templates')
    page.get_by_role('textbox', name=auth['username'], exact=True).fill('admin')
    page.get_by_label(auth['password'], exact=True).fill('gpuworks-test-password-2026')
    with page.expect_response(lambda r: urlsplit(r.url).path == '/api/v1/auth/login') as login:
        action(page, auth['submit']).click()
    require(login.value.status == 200, 'browser login rejected')
    for cookie in context.cookies():
        artifacts.remember(cookie['value'])
    context.grant_permissions(['clipboard-read', 'clipboard-write'], origin=server.base)
    report['authenticated'] = True
    artifacts.start_trace(context)
    mutations, imports, browser_errors, failed_responses = [], [], [], []
    page.on('request', lambda r: mutations.append((r.method, urlsplit(r.url).path))
            if urlsplit(r.url).path.startswith(API) and r.method in ('POST', 'DELETE', 'PUT', 'PATCH') else None)
    page.on('pageerror', lambda e: browser_errors.append(str(e)))
    page.on('response', lambda r: failed_responses.append((r.request.method, urlsplit(r.url).path, r.status))
            if r.status >= 400 else None)
    cdp = context.new_cdp_session(page)
    cdp.send('Network.enable')
    cdp.on('Network.requestWillBeSent', lambda e: imports.append(e['requestId'])
           if e['request']['method'] == 'POST' and urlsplit(e['request']['url']).path == API + '/import' else None)
    source = report['fixture_model']
    report['step'] = 'owned template real entry and editor geometry'
    page.get_by_placeholder(t['flat']['search'], exact=True).fill(report['fixture_title'])
    action(page, report['fixture_title']).click()
    original = model(page, server.base, source)
    source_yaml, source_db = yaml_read(page, server.base, source), db_snapshot(server, source)
    source_package = package_read(page, server, source)
    target = clone_real(page, server, report, t, source)
    baseline = model(page, server.base, target)
    require(baseline['source'] == 'user' and baseline['display_title'] == report['fixture_title'] + ' Copy'
            and len(baseline['specs']) == 1 and baseline['specs'][0]['args'] == ['--max-model-len', '4096'],
            'independent fixture model/spec facts differ')
    original_yaml = yaml_read(page, server.base, target)
    original_db = db_snapshot(server, target)
    trigger, dialog, editor = reopen_clone_editor(page, server, report, t, target, baseline,
        original_yaml, original_db, mutations)
    save, reset = action(dialog, t['flat']['save']), action(dialog, t['editor']['reset'])
    report['drawer_identity'] = assert_drawer_identity(page, dialog, t['flat']['editYAML'], editor,
        page.get_by_role('dialog', name=t['editor']['discardTitle'], exact=True))
    report['drawer_focus'] = assert_yaml_drawer_focus(page, dialog, save, editor)
    editor_bytes(editor, original_yaml)
    report['geometry'] = measure(page, dialog, editor, save, reset)
    report['step'] = 'actual keyboard edit, clipboard and reset'
    keyboard_and_copy(page, context, editor, original_yaml)
    reset.click()
    expect(reset).to_be_disabled()
    editor_bytes_settled(editor, original_yaml)
    require(mutations == [('POST', API + '/clone')] and db_snapshot(server, target) == original_db,
            'keyboard/reset mutated persistence')
    updated = changed_title(original_yaml, baseline['display_title'], report['fixture_title'] + ' Edited')
    report['step'] = 'clean and dirty close/escape/mask matrix'
    report['dismissals'] = dismiss_matrix(page, t, trigger, dialog, editor, original_yaml, updated, mutations)
    require(model(page, server.base, target) == baseline and db_snapshot(server, target) == original_db,
            'close matrix changed independently stored model/specs')
    replace_editor(editor, updated)
    report['step'] = 'exact real save request and independent persistence'
    report['save_wire'] = save_real(page, cdp, imports, dialog, save, updated, report)
    expected = copy.deepcopy(baseline)
    expected['display_title'] = report['fixture_title'] + ' Edited'
    require(model(page, server.base, target) == expected, 'save changed other model/spec fields')
    stored_title(server, target, expected['display_title'])
    require(yaml_read(page, server.base, target) == updated, 'saved canonical bytes differ')
    page.reload()
    expect(page.get_by_role('heading', name=expected['display_title'], exact=True)).to_be_visible()
    _, dialog, editor = open_editor(page, t)
    editor_bytes(editor, updated)
    dialog.locator('.ant-drawer-close').click()
    require(model(page, server.base, source) == original and yaml_read(page, server.base, source) == source_yaml
            and db_snapshot(server, source) == source_db and package_read(page, server, source) == source_package,
            'user clone editing changed original model/spec/package bytes')
    report['step'] = 'real upstream read-only and clone boundary'
    upstream_clone(page, server, report, t, upstream_id, cdp, imports, mutations)
    require(not browser_errors, 'uncaught browser exception')
    require(failed_responses == [('POST', API + '/import', 400)], 'unexpected browser HTTP failures')
    report['step'] = 'real named UI deletion and independent absence'
    delete_ui(page, server, report, t, mutations)
    require(not browser_errors and failed_responses == [('POST', API + '/import', 400)],
            'deletion ended with browser exception or unexpected HTTP failure')
    report['evidence'] = [{'layer': 'ui', 'candidate_items': ['12.2', '12.4', '12.6', '15.1', '15.3', '15.7']},
        {'layer': 'request', 'save': report['save_wire']},
        {'layer': 'persistence', 'fixture_before_sha256': original_db,
         'saved_model': expected, 'source_unchanged': report['upstream_unchanged']},
        {'layer': 'cleanup', 'ui_deletions': report['ui_deletions']}]


def clone_real(page, server, report, t, source):
    original = model(page, server.base, source)
    candidate = source + '-copy'
    require(page.request.get(server.base + API + '/model/' + quote(candidate, safe='/')).status == 404,
            'clone target already exists; refusing ambiguous ownership')
    report['owned_models'].append(candidate)
    with page.expect_response(lambda r: urlsplit(r.url).path == API + '/clone' and r.request.method == 'POST') as result:
        action(page, t['detail']['clone']).click()
    response = result.value
    if response.status == 201:
        actual_id = response.json().get('model_id')
        if isinstance(actual_id, str) and actual_id and actual_id not in report['owned_models']:
            report['owned_models'].append(actual_id)
    require(response.status == 201 and response.json()['model_id'] == candidate, 'clone response identity drift')
    require(response.request.post_data_json == {'source_model_id': source} and not urlsplit(response.url).query,
            'clone actual wire differs')
    copied = model(page, server.base, candidate)
    expected_specs = {s['name']: s for s in original['specs']}
    require(copied['source'] == 'user' and len(copied['specs']) == len(original['specs']), 'clone source/spec count wrong')
    for spec in copied['specs']:
        require(spec == dict(expected_specs[spec['name']], id=spec['id'], source='user'), 'clone lost source spec fields')
    return candidate


def delete_ui(page, server, report, t, mutations):
    observations = report.setdefault('ui_deletions', [])
    for identity in list(reversed(report['owned_models'])):
        baseline = model(page, server.base, identity)
        for index, spec in enumerate(baseline['specs']):
            require(spec['source'] == 'user', 'UI cleanup refuses non-user spec')
            page.goto(server.base + '/inference-templates/' + quote(identity, safe='/'))
            trigger = page.get_by_role('button', name=t['editor']['deleteSpec'].replace('{{name}}', spec['name']), exact=True)
            trigger.click()
            confirm = page.get_by_role('tooltip')
            expect(confirm).to_contain_text(t['editor']['deleteSpecTitle'])
            expect(confirm).to_contain_text(t['editor']['deleteSpecBody'].replace('{{name}}', spec['name']))
            before, stored = len(mutations), db_snapshot(server, identity)
            action(confirm, t['flat']['cancel']).click()
            expect(confirm).not_to_be_visible()
            require(len(mutations) == before and db_snapshot(server, identity) == stored,
                    'cancel delete changed request count or SQLite')
            trigger.click()
            with page.expect_response(lambda r: urlsplit(r.url).path == API + '/spec' and r.request.method == 'DELETE') as result:
                action(confirm, t['editor']['deleteSpecOk']).click()
            response = result.value
            from urllib.parse import parse_qs
            last = index == len(baseline['specs']) - 1
            require(response.status == 200 and response.json() == {'deleted': True, 'model_deleted': last},
                    'spec deletion result differs')
            require(parse_qs(urlsplit(response.url).query) == {'model_id': [identity], 'spec': [spec['name']]}
                    and response.request.post_data is None, 'delete actual target/body differs')
            if last:
                require(page.request.get(server.base + API + '/model/' + quote(identity, safe='/')).status == 404,
                        'last-spec deletion did not remove model')
            else:
                current = model(page, server.base, identity)
                require(current == dict(baseline, specs=baseline['specs'][index + 1:]),
                        'deletion changed remaining model/spec fields')
            observations.append({'model_id': identity, 'spec': spec['name'], 'last': last, 'status': 200})
        report['owned_models'].remove(identity)


def upstream_clone(page, server, report, t, upstream_id, cdp, imports, mutations):
    upstream = model(page, server.base, upstream_id)
    require(upstream['source'] == 'upstream' and upstream['specs']
            and all(s['source'] == 'upstream' for s in upstream['specs']), 'requires a real pure-upstream template')
    original_yaml, original_db = yaml_read(page, server.base, upstream_id), db_snapshot(server, upstream_id)
    original_package = package_read(page, server, upstream_id)
    page.goto(server.base + '/inference-templates/' + quote(upstream_id, safe='/'))
    expect(page.get_by_role('heading', name=upstream['display_title'], exact=True)).to_be_visible()
    expect(action(page, t['flat']['edit'])).to_have_count(0)
    expect(page.get_by_role('button', name=re.compile('^' + re.escape(t['editor']['deleteSpec'].split('{{')[0])))).to_have_count(0)
    # Register the expected isolated target before the request; partial failure
    # must not lose the only ID available for scoped recovery.
    candidate = clone_real(page, server, report, t, upstream_id)
    copy_model = model(page, server.base, candidate)
    copy_db = db_snapshot(server, candidate)
    require(copy_model['source'] == 'user' and len(copy_model['specs']) == len(upstream['specs']), 'clone source/spec count wrong')
    expected_specs = {s['name']: s for s in upstream['specs']}
    for spec in copy_model['specs']:
        require(spec == dict(expected_specs[spec['name']], id=spec['id'], source='user'), 'clone altered source spec fields')
    dialog, editor = editor_ready(page, t['flat']['editYAML'])
    baseline_yaml = yaml_read(page, server.base, candidate)
    editor_bytes(editor, baseline_yaml)
    invalid = baseline_yaml + '\nunknown_candidate_field: true\n'
    replace_editor(editor, invalid)
    save = action(dialog, t['flat']['save'])
    response = submit_real(page, cdp, imports, save, invalid, 400, report)
    require('unknown_candidate_field' in response.text(), 'real server reason does not identify bad field')
    # Stable read: the draft must remain for a full hold window; an instant or
    # settle read cannot see a revert that lands one typing-latch window later
    # (v5/v6 error-restores-draft mutants survived both). Stronger, not weaker.
    editor_bytes_stable(editor, invalid)
    expect(dialog.get_by_role('alert')).to_contain_text('unknown_candidate_field')
    expect(dialog.get_by_role('alert')).to_contain_text(t['editor']['saveFailed'])
    expect(save).to_be_enabled()
    expect(save).not_to_have_class(re.compile(r'\bant-btn-loading\b'))
    require(model(page, server.base, candidate) == copy_model and yaml_read(page, server.base, candidate) == baseline_yaml
            and db_snapshot(server, candidate) == copy_db, 'rejected draft changed clone/YAML/SQLite')
    require(model(page, server.base, upstream_id) == upstream and db_snapshot(server, upstream_id) == original_db
            and package_read(page, server, upstream_id) == original_package,
            'clone failure changed upstream')
    updated = changed_title(baseline_yaml, copy_model['display_title'], 'e2e-' + report['run_id'] + ' Clone Edited')
    replace_editor(editor, updated)
    report['upstream_clone_wire'] = save_real(page, cdp, imports, dialog, action(dialog, t['flat']['save']), updated, report)
    expected = dict(copy_model, display_title='e2e-' + report['run_id'] + ' Clone Edited')
    require(model(page, server.base, candidate) == expected, 'clone save lost unrelated specs/fields')
    stored_title(server, candidate, expected['display_title'])
    require(yaml_read(page, server.base, candidate) == updated, 'clone canonical YAML differs after real save')
    page.reload()
    expect(page.get_by_role('heading', name=expected['display_title'], exact=True)).to_be_visible()
    _, dialog, editor = open_editor(page, t)
    editor_bytes(editor, yaml_read(page, server.base, candidate))
    dialog.locator('.ant-drawer-close').click()
    require(model(page, server.base, upstream_id) == upstream and yaml_read(page, server.base, upstream_id) == original_yaml
            and db_snapshot(server, upstream_id) == original_db and package_read(page, server, upstream_id) == original_package,
            'clone success changed upstream model/spec/package identity')
    report['upstream_unchanged'] = {'model_id': upstream_id, 'sqlite_sha256': original_db,
        'yaml_sha256': hashlib.sha256(original_yaml.encode()).hexdigest()}
    require(mutations == [('POST', API + '/clone'), ('POST', API + '/import'), ('POST', API + '/clone'),
                          ('POST', API + '/import'), ('POST', API + '/import')], 'duplicate/unexpected browser mutations')
