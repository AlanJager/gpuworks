"""Three public file lifecycle contracts, driven through real browser controls.

The download confirm is held, then continued into the real server. No management
status is fulfilled. Expected file bytes come from the external repository, not
a product DTO. CLI: model_files_lifecycle.py --case CASE --locale en|zh.
"""
import hashlib
import json
import re
import sqlite3
import time
from pathlib import Path
from urllib.parse import parse_qs, urlsplit

from playwright.sync_api import expect
from local_browser_fixture import ROOT, http
from local_business_runner import checked, unattributed_console_errors
from dialog_checks import assert_dialog_shell, assert_nested_dismissals, cancel_button, close_button, escape, mask
from platform_token_read_revoke import button, modal_geometry
from model_files_fixture import MODELS, file_facts, payload_facts


def strings(locale, namespace):
    return json.loads((ROOT / f'ui/src/i18n/locales/{locale}/{namespace}.json').read_text())


def poll(page, read, label, timeout=90):
    deadline, last = time.monotonic() + timeout, None
    while time.monotonic() < deadline:
        last = read()
        if last:
            return last
        page.wait_for_timeout(100)
    raise AssertionError(f'{label} did not converge: {last!r}')


def strict(actual, expected, label):
    checked(json.dumps(actual, sort_keys=True) == json.dumps(expected, sort_keys=True),
            f'{label}: expected {expected!r}; actual {actual!r}')


def document(server, token, data):
    code, model = http(server.base, 'GET', MODELS + '/' + str(data['model']['id']), token)
    checked(code == 200, f'independent model GET HTTP {code}')
    strict({k: model[k] for k in ('id', 'source', 'source_id', 'source_url', 'name')},
           {'id': data['model']['id'], 'source': 'url', 'source_id': data['source_id'],
            'source_url': data['hub'].url, 'name': data['name']}, 'model identity')
    return model


def presence(model, data, storage):
    rows = [r for r in model.get('nodes', []) if r['node_uuid'] == data['node']['uuid']
            and r.get('storage_id') == storage['id'] and not r.get('cache')]
    checked(len(rows) <= 1, 'duplicate file presence for exact node/storage')
    return rows[0] if rows else None


def wait_state(page, server, token, data, storage, state, report):
    def read():
        model = document(server, token, data)
        report['last_model_observation'] = model
        row = presence(model, data, storage)
        return row if row and row['state'] == state else None
    return poll(page, read, f'exact storage {storage["id"]} state={state}')


def watch(page, report):
    # Unattributed console errors and unexpected >=400 responses fail the case
    # in finish; only the anonymous pre-login session probe is attributed.
    report['page_errors'], report['console_errors'], report['http_errors'] = [], [], []
    session = {'authenticated': False}
    def console_seen(message):
        if message.type != 'error':
            return
        path = urlsplit(message.location.get('url', '')).path
        if not session['authenticated'] and path == '/api/v1/auth/me' and '401' in message.text:
            return  # Exactly the anonymous session probe, before login.
        report['console_errors'].append({'path': path, 'text': message.text})
    def responded(response):
        path = urlsplit(response.url).path
        if (response.request.method, path, response.status) in (report.get('allowed_http') or []):
            report.setdefault('attributed_responses', []).append({'path': path, 'status': response.status})
            return
        if response.status >= 400 and not (not session['authenticated']
                                           and path == '/api/v1/auth/me' and response.status == 401):
            report['http_errors'].append({'method': response.request.method, 'path': path,
                                          'status': response.status})
    page.on('pageerror', lambda error: report['page_errors'].append(str(error)))
    page.on('console', console_seen)
    page.on('response', responded)
    return session


def begin(page, context, server, artifacts, report, locale, data):
    auth, labels = strings(locale, 'auth'), strings(locale, 'models')
    report['sensitive_phase'] = True
    session = watch(page, report)
    page.goto(server.base + '/models')
    page.get_by_role('textbox', name=auth['username'], exact=True).fill('admin')
    page.get_by_label(auth['password'], exact=True).fill('gpuworks-test-password-2026')
    with page.expect_response(lambda r: urlsplit(r.url).path == '/api/v1/auth/login'
                              and r.request.method == 'POST') as logged:
        button(page, auth['submit']).click()
    checked(logged.value.status == 200, 'browser admin login failed')
    session['authenticated'] = True
    for cookie in context.cookies():
        artifacts.remember(cookie['value'])
    report['sensitive_phase'] = False
    artifacts.start_trace(context)
    expect(page.get_by_role('heading', name=labels['page']['title'], exact=True)).to_be_visible()
    page.get_by_role('img', name=labels['view']['list'], exact=True).click()
    expect(page.get_by_role('radio', name=labels['view']['list'], exact=True)).to_be_checked()
    report['mutations'] = []
    def requested(request):
        parts = urlsplit(request.url)
        if parts.path.startswith(MODELS) and request.method in ('POST', 'PUT', 'PATCH', 'DELETE'):
            report['mutations'].append({'method': request.method, 'path': parts.path,
                'query': parts.query, 'body': request.post_data_json if request.post_data else None})
    page.on('request', requested)
    return labels


def choose(page, control, label):
    # A non-searchable AntD Select overlays its readonly input with the
    # selection label. Open through the real combobox keyboard contract.
    control.focus()
    control.press('ArrowDown')
    popup_id = control.get_attribute('aria-controls')
    checked(popup_id, 'combobox does not identify its controlled popup')
    popup = page.locator('.ant-select-dropdown').filter(has=page.locator('[id=' + json.dumps(popup_id) + ']'))
    expect(popup).to_have_count(1)
    option = popup.get_by_text(label, exact=True)
    expect(option).to_be_visible()
    option.click()


def select_node(page, dialog, labels, data):
    control = dialog.get_by_role('combobox', name=labels['download']['nodes'], exact=True)
    control.click()
    control.fill(data['node']['name'])
    # Exact readable node option, using the real keyboard selection contract.
    name = labels['download']['gpuCount'].replace('{{count}}', '2')
    option_label = data['node']['name'] + ' · ' + name
    option = page.get_by_role('option', name=option_label, exact=True)
    expect(option).to_have_count(1)
    control.press('Enter')
    expect(option).to_have_attribute('aria-selected', 'true')
    control.press('Escape')
    expect(dialog.locator('.ant-select-selection-item').filter(has_text=data['node']['name'])).to_have_count(1)


def _files_db(server):
    conn = sqlite3.connect(server.work / 'server-data' / 'gpuworks.db', timeout=5)
    conn.execute('PRAGMA busy_timeout=5000')
    return conn


def drop_storage_target(server, storage_id, node_uuid):
    # downloadModel rejects STORAGE_UNAVAILABLE_ON_NODE before it writes a file
    # row. The membership row is the only switch, and it is put back before return.
    conn = _files_db(server)
    try:
        node = conn.execute('SELECT id FROM nodes WHERE uuid = ?', (node_uuid,)).fetchone()
        checked(node is not None, 'owned node row missing')
        cols = [info[1] for info in conn.execute('PRAGMA table_info(storage_nodes)')]
        rows = conn.execute(
            f"SELECT {', '.join(cols)} FROM storage_nodes WHERE storage_id = ? AND node_id = ?",
            (storage_id, node[0])).fetchall()
        checked(len(rows) == 1, f'storage target rows {len(rows)}')
        cur = conn.execute('DELETE FROM storage_nodes WHERE storage_id = ? AND node_id = ?',
                           (storage_id, node[0]))
        conn.commit()
        checked(cur.rowcount == 1, f'storage target delete updated {cur.rowcount} rows')
        return {'cols': cols, 'row': rows[0]}
    finally:
        conn.close()


def restore_storage_target(server, saved):
    cols, row = saved['cols'], saved['row']
    conn = _files_db(server)
    try:
        conn.execute(
            f"INSERT INTO storage_nodes ({', '.join(cols)}) VALUES ({', '.join('?' * len(cols))})",
            row)
        conn.commit()
    finally:
        conn.close()


def prove_refused_model_download(page, dialog, server, token, path, body, report, labels, data, storage):
    """Hold Download: loading, no second write, real 409, storage and node kept."""
    ok = dialog.locator('.ant-modal-footer .ant-btn-primary')
    expect(ok).to_be_enabled()
    held = []
    pattern = re.compile(re.escape(path) + r'(?:\?|$)')

    def hold_post(route):
        if route.request.method != 'POST' or urlsplit(route.request.url).path != path:
            route.continue_()
            return
        held.append(route)

    page.route(pattern, hold_post)
    dropped = None
    try:
        ok.click()
        report['last_step'] = 'model-download-refused-inflight'
        expect(ok).to_have_class(re.compile(r'\bant-btn-loading\b'))
        ok.click()
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline and not held:
            page.wait_for_timeout(100)
        checked(len(held) == 1, 'duplicate model download while the refusal is held')
        dropped = drop_storage_target(server, storage['id'], data['node']['uuid'])
        report.setdefault('allowed_http', []).append(('POST', path, 409))
        with page.expect_response(lambda response: urlsplit(response.url).path == path
                                   and response.request.method == 'POST') as received:
            held[0].continue_()
        refused = received.value
    finally:
        try:
            page.unroute(pattern, hold_post)
        except Exception:
            pass
        if dropped is not None:
            restore_storage_target(server, dropped)
    checked(refused.status == 409, f'refused download HTTP {refused.status}')
    payload = refused.json()
    strict(payload.get('code'), 'STORAGE_UNAVAILABLE_ON_NODE', 'refused download error code')
    strict(refused.request.post_data_json, body, 'refused download draft')
    expect(dialog).to_be_visible()
    expect(dialog.locator('.ant-select-selection-item').filter(has_text=storage['name'])).to_have_count(1)
    expect(dialog.locator('.ant-select-selection-item').filter(has_text=data['node']['name'])).to_have_count(1)
    shown = labels['download']['failed'].replace(
        '{{detail}}', strings(report['locale'], 'common')['apiError']['STORAGE_UNAVAILABLE_ON_NODE'])
    expect(page.get_by_text(shown, exact=True)).to_be_visible()
    model = document(server, token, data)
    checked(presence(model, data, storage) is None, 'refused download persisted a file row')
    report.setdefault('evidence', []).append({'layer': 'ui', 'inflight_posts': 1, 'confirm_loading': True,
        'draft_storage': storage['name'], 'draft_node': data['node']['name']})
    report.setdefault('evidence', []).append({'layer': 'request', 'method': 'POST', 'path': path,
        'body': body, 'status': 409})
    report.setdefault('evidence', []).append({'layer': 'persistence', 'file_row': None,
        'independent_get': MODELS + '/' + str(data['model']['id'])})
    report['last_step'] = 'model-download-refused'
    return refused


def hold_model_download(page, dialog, path, body, report):
    # setSubmitting runs before the download POST. Continue the real request;
    # the later 503 is the fixture weight server, not a fulfilled status.
    held = []
    pattern = re.compile(re.escape(path) + r'(?:\?|$)')
    ok = dialog.locator('.ant-modal-footer .ant-btn-primary')

    def hold_post(route):
        if route.request.method != 'POST' or urlsplit(route.request.url).path != path:
            route.continue_()
            return
        held.append(route)

    page.route(pattern, hold_post)
    ok.click()
    report['last_step'] = 'model-download-inflight'
    expect(ok).to_have_class(re.compile(r'\bant-btn-loading\b'))
    ok.click()
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline and not held:
        page.wait_for_timeout(100)
    checked(len(held) == 1, 'duplicate model download while the first request is held')
    strict(held[0].request.post_data_json, body, 'held model download body differs')
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == 'POST') as received:
        held[0].continue_()
    page.unroute(pattern, hold_post)
    response = received.value
    checked(response.status == 202, f'POST {path}: HTTP {response.status}; {response.text()}')
    strict(response.request.post_data_json if response.request.post_data else None, body, 'actual browser mutation body')
    strict(parse_qs(urlsplit(response.url).query), {}, 'actual browser mutation query')
    report.setdefault('browser_responses', []).append({'method': 'POST', 'path': path,
        'query': {}, 'body': body, 'status': response.status})
    report.setdefault('evidence', []).append({'layer': 'ui', 'inflight_posts': 1, 'confirm_loading': True})
    return response


def mutation(page, report, action, method, path, body, status, query=None):
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == method) as received:
        action()
    response = received.value
    checked(response.status == status, f'{method} {path}: HTTP {response.status}; {response.text()}')
    strict(response.request.post_data_json if response.request.post_data else None, body, 'actual browser mutation body')
    strict(parse_qs(urlsplit(response.url).query), query or {}, 'actual browser mutation query')
    report.setdefault('browser_responses', []).append({'method': method, 'path': path,
        'query': parse_qs(urlsplit(response.url).query), 'body': body, 'status': response.status})
    return response


def open_detail(page, server, labels, data):
    name = data['name']
    page.get_by_role('button', name=name, exact=True).click()
    dialog = page.get_by_role('dialog', name=name, exact=True)
    expect(dialog.locator('#model-files')).to_be_visible()
    return dialog


def close_detail(page, labels, data):
    dialog = page.get_by_role('dialog', name=data['name'], exact=True)
    button(dialog.locator('.ant-modal-footer'), labels['action']['close']).click()
    expect(dialog).not_to_be_visible()


def download(page, labels, data, storage, report, server=None, token=None, refuse=False, shell=False):
    report['step'] = 'UI select explicit non-default storage and target node'
    row = page.get_by_role('row').filter(has=page.get_by_role('button', name=data['name'], exact=True))
    trigger = row.get_by_role('button', name=labels['action']['downloadToNode'], exact=True)
    title = labels['download']['title'].replace('{{name}}', data['name'])
    path = MODELS + '/' + str(data['model']['id']) + '/download'
    posts = []
    page.on('request', lambda request: posts.append(path)
            if request.method == 'POST' and urlsplit(request.url).path == path else None)

    def reopen():
        trigger.click()
        opened = page.get_by_role('dialog', name=title, exact=True)
        expect(opened).to_be_visible()
        return opened

    # The list table stays mounted. This dialog is not nested in another dialog.
    dialog = assert_nested_dismissals(reopen(), page.locator('.ant-table').first, reopen, [
        ('cancel', cancel_button(labels['action']['cancel'])), ('escape', escape),
        ('close', close_button), ('mask', mask)], lambda: len(posts), trigger)
    checked(posts == [], 'download dialog dismissal started a download')
    if server is not None and token is not None:
        checked(presence(document(server, token, data), data, storage) is None,
                'download dialog dismissal created a file row')
    report.setdefault('evidence', []).append({'layer': 'ui', 'download_dialog': title,
        'dismissals': ['cancel', 'escape', 'close', 'mask'], 'focus_returned': True, 'download_posts': 0})
    submit = button(dialog, labels['download']['action'])
    expect(submit).to_be_disabled()
    storage_control = dialog.get_by_role('combobox', name=labels['download']['storage'], exact=True)
    common = strings(report['locale'], 'common')
    choose(page, storage_control, storage['name'] + ' · ' + common['storageType']['local'])
    select_node(page, dialog, labels, data)
    expect(submit).to_be_enabled()
    if shell:
        # Same dialog shell as the landed modal header fix: identity, 1440x900
        # stacking, and the focus trap. Geometry stays inside that check.
        report['last_step'] = 'model-download-dialog-shell'
        assert_dialog_shell(page, dialog, dialog.get_by_role(
            'combobox', name=labels['download']['nodes'], exact=True), submit)
        report.setdefault('evidence', []).append({'layer': 'ui', 'dialog_shell': title, 'viewport': '1440x900'})
    else:
        modal_geometry(dialog, submit)
    body = {'storage_id': storage['id'], 'node_uuids': [data['node']['uuid']]}
    if refuse:
        prove_refused_model_download(page, dialog, server, token, path, body, report, labels, data, storage)
    hold_model_download(page, dialog, path, body, report)
    expect(dialog).not_to_be_visible()


def bytes_text(value):
    units = ('B', 'KiB', 'MiB', 'GiB', 'TiB')
    number, unit = float(value), 0
    while number >= 1024 and unit < 4:
        number, unit = number / 1024, unit + 1
    return (str(round(number)) if unit == 0 else f'{number:.1f}') + ' ' + units[unit]


def assert_ready(page, server, token, labels, data, storage, report, *, reload=True):
    report['step'] = 'external bytes = actual chosen path = authoritative file row = rendered units'
    row = wait_state(page, server, token, data, storage, 'present', report)
    total = sum(map(len, data['hub'].files.values()))
    expected = {'node_uuid': data['node']['uuid'], 'storage_id': storage['id'],
                'storage_name': storage['name'], 'storage_type': 'local',
                'path': storage['payload_path'], 'state': 'present', 'acquisition': 'download',
                'size_bytes': total, 'downloaded': total, 'progress': 1, 'deletable': True}
    strict({key: row.get(key) for key in expected}, expected, 'independent file row facts')
    checked(row['storage_type'] == 'local', 'selected storage is actual local file placement')
    checked(row['acquisition'] == 'download', 'public file acquisition is download')
    checked(row['state'] == 'present', 'downloaded file reaches actual present state')
    actual = file_facts(storage['payload_path'], data['hub'].files)
    # Select by the API-stable group heading when two locations share node/path.
    dialog = page.get_by_role('dialog', name=data['name'], exact=True)
    if not dialog.is_visible():
        dialog = open_detail(page, server, labels, data)
    files = dialog.locator('#model-files')
    group = files.locator(':scope > div').filter(has_text=storage['name'])
    if len(document(server, token, data).get('nodes', [])) > 1:
        visible = group.locator('tr.ant-table-row')
    else:
        visible = files.locator('tr.ant-table-row')
    expect(visible).to_have_count(1)
    expect(visible.get_by_text(data['node']['name'], exact=True)).to_be_visible()
    expect(visible.get_by_text(storage['payload_path'], exact=True)).to_be_visible()
    expect(visible.get_by_text(labels['presence']['present'], exact=True)).to_be_visible()
    expect(visible.get_by_text(bytes_text(total), exact=True)).to_be_visible()
    expect(visible.get_by_text(bytes_text(total) + ' / ' + bytes_text(total), exact=True)).to_be_visible()
    # AntD renders a check-circle at100; the progressbar retains exact
    # accessible numeric bounds instead of a literal100% text label.
    expect(visible.get_by_role('progressbar')).to_have_attribute('aria-valuenow', '100')
    expect(visible.get_by_role('progressbar')).to_have_attribute('aria-valuemin', '0')
    expect(visible.get_by_role('progressbar')).to_have_attribute('aria-valuemax', '100')
    expect(visible.locator('.ant-progress-text').get_by_role('img', name='check-circle')).to_be_visible()
    expect(visible.get_by_text(labels['acquisition']['download'], exact=True)).to_be_visible()
    report.setdefault('completed_locations', []).append({'storage_id': storage['id'],
        'node_uuid': data['node']['uuid'], 'actual_path': storage['payload_path'], 'files': actual, 'api': row})
    if reload:
        with page.expect_response(lambda r: urlsplit(r.url).path == MODELS + '/' + str(data['model']['id'])
                                  and r.request.method == 'GET') as refreshed:
            page.reload()
        checked(refreshed.value.status == 200, 'detail reload failed')
        strict(presence(refreshed.value.json(), data, storage), row, 'full presence row after reload')
        checked(file_facts(storage['payload_path'], data['hub'].files) == actual, 'refresh changed payload')
        expect(page.get_by_role('dialog', name=data['name'], exact=True).locator('#model-files')).to_be_visible()
    return row


def wire_proof(data, start):
    hub = data['hub']
    checked(hub.wait_idle(), 'repository handlers remain after payload completed')
    records = hub.audit()[start:]
    proof = {}
    for name, payload in hub.files.items():
        intervals = []
        for record in records:
            if urlsplit(record['target']).path != hub.root + '/' + name or record['method'] != 'GET' or record['status'] not in (200, 206):
                continue
            match = re.fullmatch(r'bytes=(\d+)-(\d*)', record['range'] or '')
            offset = int(match[1]) if match else 0
            body = payload[offset:offset + record['bytes_sent']]
            strict(record['sha256_sent'], hashlib.sha256(body).hexdigest(), 'actual wire slice SHA256')
            checked(record['bytes_sent'] <= len(payload) - offset, 'wire overrun')
            intervals.append((offset, offset + record['bytes_sent']))
        end = 0
        for left, right in sorted(intervals):
            checked(left <= end, f'wire coverage hole for {name} at {end}')
            end = max(end, right)
        checked(end == len(payload), f'wire did not deliver complete {name}: {end}/{len(payload)}')
        proof[name] = {'covered_bytes': end, 'intervals': intervals, **payload_facts({name: payload})[name]}
    return {'records': records, 'files': proof}


def finish(page, artifacts, report, data, source):
    checked(not report['page_errors'], 'browser page errors: ' + repr(report['page_errors']))
    # A deliberate 409 is recorded in attributed_responses. Every other console
    # error and every other >=400 still fail the case.
    rest = unattributed_console_errors(report['console_errors'], report.get('attributed_responses') or [])
    checked(not rest, 'browser console errors: ' + repr(rest))
    checked(not report['http_errors'], 'unexpected browser HTTP errors: ' + repr(report['http_errors']))
    artifacts.write_json('repository-wire.json', source)
    artifacts.write_json('component-assertions.json', {'case': report['contract'],
        'declared_scope': 'URL local storage only; enum/state denominators are in the committed manifest',
        'actual_locations': report.get('completed_locations'), 'mutations': report['mutations']})
    report['evidence'].extend([{'layer': 'ui', 'locations': report.get('completed_locations')},
        {'layer': 'request', 'requests': report['mutations'], 'responses': report['browser_responses']},
        {'layer': 'persistence', 'model': report['last_model_observation']},
        {'layer': 'execution', 'source': source, 'agent_pid': report['agent_pid']}])
    report['last_step'] = 'exact UI/request/disk/wire facts and refresh passed'


# Mainline: download two owned same-node/storage-distinct replicas; cancel delete,
# delete only target; guard bytes and model metadata survive.
def exercise_download_delete(page, context, server, artifacts, report, locale, data, token, holder):
    labels = begin(page, context, server, artifacts, report, locale, data)
    start = len(data['hub'].audit())
    for index, storage in enumerate(data['storages']):
        download(page, labels, data, storage, report, server=server, token=token,
                 refuse=index == 0, shell=index == 0)
        assert_ready(page, server, token, labels, data, storage, report)
        close_detail(page, labels, data)
    source = wire_proof(data, start)
    dialog = open_detail(page, server, labels, data)
    target, guard = data['storages']
    before = file_facts(guard['payload_path'], data['hub'].files)
    files = dialog.locator('#model-files')
    group = files.locator(':scope > div').filter(has_text=target['name'])
    remove = group.get_by_role('button', name=labels['files']['deleteOnNode'], exact=True)
    remove.click()
    title = labels['files']['deleteOnNodeConfirm'].replace('{{node}}', data['node']['name'])
    popup = page.locator('.ant-popconfirm').filter(has=page.get_by_text(title, exact=True))
    expect(popup.get_by_text(title, exact=True)).to_be_visible()
    expect(popup.get_by_text(labels['files']['deleteOnNodeDescription'], exact=True)).to_be_visible()
    before_mutations = list(report['mutations'])
    button(popup, labels['action']['cancel']).click()
    expect(popup).not_to_be_visible()
    strict(report['mutations'], before_mutations, 'cancel must send no mutation')
    file_facts(target['payload_path'], data['hub'].files)
    path = f"{MODELS}/{data['model']['id']}/nodes/{data['node']['uuid']}"
    from model_file_delete_blocks import exercise_node_delete_blocks
    exercise_node_delete_blocks(page, report, server, token, labels, locale, data, target, remove, path,
                                document, presence)
    remove.click()
    popup = page.locator('.ant-popconfirm').filter(has=page.get_by_text(title, exact=True))
    expect(popup.get_by_text(title, exact=True)).to_be_visible()
    mutation(page, report, button(popup, labels['action']['delete']).click,
             'DELETE', path, None, 200, {'storage_id': [str(target['id'])]})
    poll(page, lambda: not Path(target['payload_path']).exists(), 'target directory absent')
    current = document(server, token, data)
    checked(presence(current, data, target) is None, 'deleted storage/node row remains')
    strict(file_facts(guard['payload_path'], data['hub'].files), before, 'guard replica unchanged')
    checked(presence(current, data, guard)['state'] == 'present', 'guard replica not present')
    expect(files.locator('tr.ant-table-row')).to_have_count(1)
    expect(files.get_by_text(guard['name'], exact=True)).to_have_count(0)  # single local group hides heading
    page.reload()
    expect(page.get_by_role('dialog', name=data['name'], exact=True).locator('#model-files tr.ant-table-row')).to_have_count(1)
    strict(file_facts(guard['payload_path'], data['hub'].files), before, 'guard survives reload')
    report['deletion'] = {'node_uuid': data['node']['uuid'], 'storage_id': target['id'],
                          'target_absent': True, 'guard_files': before, 'model_still_exists': current['id']}
    finish(page, artifacts, report, data, source)


# Mainline: real source503 -> failed row -> same row Retry -> source recovery,
# retaining the originally chosen non-default storage and node.
def exercise_failure_retry(page, context, server, artifacts, report, locale, data, token, holder):
    labels = begin(page, context, server, artifacts, report, locale, data)
    hub, target = data['hub'], data['storages'][0]
    hub.set_weight_failure(True)
    start = len(hub.audit())
    download(page, labels, data, target, report, server=server, token=token, shell=True)
    failed = wait_state(page, server, token, data, target, 'failed', report)
    checked(failed['state'] == 'failed', 'source503 reaches actual failed file state')
    checked(failed['acquisition'] == 'download' and failed['path'] == target['payload_path'], 'failed row lost download identity')
    host = urlsplit(hub.base).netloc
    expected_error = (f'download model.safetensors: {host} failed: {hub.url}/model.safetensors: '
                      'download: unexpected probe status 503')
    strict(failed.get('message'), expected_error, 'exact external503 failure cause')
    checked(any(r['status'] == 503 and r['target'] == hub.root + '/model.safetensors' for r in hub.audit()[start:]),
            'no actual weight503 observed')
    checked(not Path(target['payload_path'], 'model.safetensors').exists(), 'source503 incorrectly left complete weight')
    dialog = open_detail(page, server, labels, data)
    row = dialog.locator('#model-files tr.ant-table-row')
    expect(row).to_have_count(1)
    expect(row.get_by_text(labels['presence']['failed'], exact=True)).to_be_visible()
    expect(row.get_by_text(failed['message'], exact=True)).to_be_visible()
    page.reload()
    row = page.get_by_role('dialog', name=data['name'], exact=True).locator('#model-files tr.ant-table-row')
    expect(row.get_by_text(labels['presence']['failed'], exact=True)).to_be_visible()
    hub.set_weight_failure(False)
    before = len(report['mutations'])
    path = MODELS + '/' + str(data['model']['id']) + '/download'
    body = {'storage_id': target['id'], 'node_uuids': [data['node']['uuid']]}
    mutation(page, report, button(row, labels['action']['retry']).click, 'POST', path, body, 202)
    strict(report['mutations'][before:], [{'method': 'POST', 'path': path, 'query': '', 'body': body}], 'exactly one same-row public Retry request')
    assert_ready(page, server, token, labels, data, target, report)
    report['failure_recovery'] = {'failed': failed, 'retry_body': body, 'same_row': True}
    from model_file_delete_blocks import delete_present_node_copy
    delete_present_node_copy(page, report, server, token, labels, locale, data, target, document, presence,
                             lambda *args: mutation(page, report, *args))
    finish(page, artifacts, report, data, wire_proof(data, start))


# Mainline: partial response held in-flight -> observe actual downloading and
# partial file -> close source stream -> automatic nonzero Range resume -> hashes.
def exercise_interrupted_recovery(page, context, server, artifacts, report, locale, data, token, holder):
    labels = begin(page, context, server, artifacts, report, locale, data)
    hub, target = data['hub'], data['storages'][0]
    hub.interrupt_next_weight(65536, hold=True)
    start = len(hub.audit())
    download(page, labels, data, target, report, server=server, token=token, refuse=True, shell=True)
    poll(page, lambda: hub.wait_interrupted(0), 'real partial-send barrier')
    interrupted = next(r for r in hub.audit()[start:] if r['interrupted'])
    checked(interrupted['bytes_sent'] == 65536 and not interrupted['complete'], 'partial response not held inside file')
    def partial_observation():
        current = document(server, token, data)
        report['last_model_observation'] = current
        row = presence(current, data, target)
        return row if row and row['state'] == 'downloading' and 0 < row['downloaded'] < row['size_bytes'] else None
    moving = poll(page, partial_observation, 'held transfer incomplete progress')
    checked(moving['state'] == 'downloading', 'held response reaches actual downloading file state')
    checked(moving['size_bytes'] == sum(map(len, hub.files.values())), 'in-flight total not exact fixture payload')
    part = Path(target['payload_path']) / 'model.safetensors.part'
    checked(part.is_file() and not part.is_symlink(), 'no real in-progress partial file')
    dialog = open_detail(page, server, labels, data)
    expect(dialog.locator('#model-files').get_by_text(labels['presence']['downloading'], exact=True)).to_be_visible()
    page.reload()
    expect(page.get_by_role('dialog', name=data['name'], exact=True).locator('#model-files').get_by_text(labels['presence']['downloading'], exact=True)).to_be_visible()
    hub.release()
    assert_ready(page, server, token, labels, data, target, report)
    source = wire_proof(data, start)
    records = source['records']
    broken_index = next(i for i, r in enumerate(records) if r['interrupted'])
    broken = records[broken_index]
    match = re.fullmatch(r'bytes=(\d+)-(\d+)', broken['range'] or '')
    checked(match is not None, 'interrupted agent request was not a range chunk')
    expected_range = f'bytes={int(match[1]) + broken["bytes_sent"]}-{match[2]}'
    checked(any(r['target'] == broken['target'] and r['range'] == expected_range and r['complete']
                and r['status'] == 206 for r in records[broken_index+1:]),
            'automatic resume did not request the exact next unsent byte to the same chunk end')
    checked(len(report['mutations']) == 2, 'resume added a mutation or the refused download was not sent')
    report['interrupted_recovery'] = {'held_state': moving, 'interrupted': broken,
        'resumed_range': expected_range, 'automatic': True, 'part_removed': not part.exists()}
    checked(not part.exists() and not Path(str(part) + '.state').exists(), 'successful download left partial/state')
    from model_file_delete_blocks import delete_present_node_copy
    delete_present_node_copy(page, report, server, token, labels, locale, data, target, document, presence,
                             lambda *args: mutation(page, report, *args))
    finish(page, artifacts, report, data, source)
