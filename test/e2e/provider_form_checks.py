"""RunPod text, secret and concurrent-edit checks using real browser/API paths."""
import re
from urllib.parse import urlsplit

from playwright.sync_api import expect

from platform_token_read_revoke import button, check, collection

NAME_SUFFIX = ' · <b>中文 & "quoted"</b>'


def form_action(dialog, caption):
    """Match visible action text while AntD's loading icon leaves the footer."""
    pattern = r'\s*'.join(caption) if re.fullmatch(r'[\u3400-\u9fff]{2}', caption) else re.escape(caption)
    action = dialog.locator('.ant-modal-footer').get_by_role('button').filter(
        has_text=re.compile('^' + pattern + '$'))
    expect(action).to_have_count(1)
    return action
CHECK_FAMILIES = ('all', 'default-endpoint', 'text-contract', 'clipboard', 'literal-name', 'secret', 'concurrent', 'url', 'url-errors', 'url-default', 'conditional-fields', 'text-values', 'key-rotation', 'secret-errors', 'secret-errors-http')


def family_enabled(report, family):
    return report.get('check_family', 'all') in ('all', family)


def expected_upstream(report):
    """Exact ordered external calls for the original chain plus enabled checks."""
    calls = []
    if family_enabled(report, 'secret'):
        calls.append(('GET', '/proxy-a/v2/pods', 401, False))
    calls.extend([('GET', '/proxy-a/v2/pods', 200, True),
                  ('GET', '/proxy-a/v2/account/ssh-keys', 200, True),
                  ('PUT', '/proxy-a/v2/account/ssh-keys', 200, True),
                  ('GET', '/proxy-a/v2/pods', 200, True)])
    if family_enabled(report, 'secret'):
        calls.extend([('GET', '/proxy-a/v2/pods', 200, True)] * 2)
    calls.extend([('GET', '/proxy-b/v2/pods', 401, True),
                  ('GET', '/proxy-b/v2/pods', 200, True)])
    if family_enabled(report, 'concurrent'):
        calls.extend([('GET', '/proxy-b/v2/pods', 200, True)] * 2)
    return calls


def observe_mutations(page):
    writes = []

    def seen(request):
        path = urlsplit(request.url).path
        if path.startswith('/api/v1/providers') and request.method in ('POST', 'PUT', 'DELETE'):
            writes.append({'method': request.method, 'path': path,
                           'body': request.post_data_json if request.post_data else None})

    page.on('request', seen)
    return writes


def text_contract(dialog, copy, label, fields, report, *, editing, ready=True):
    if not family_enabled(report, 'text-contract'):
        return
    report['last_step'] = 'provider-text-field-contract'
    names = ((copy['config']['name'], 'name', True), (label('endpoint'), 'endpoint', False),
             (label('api_key'), 'api_key', not editing))
    for caption, name, required in names:
        control = dialog.get_by_label(caption, exact=True)
        expect(control).to_be_visible(timeout=12000)
        if ready:
            expect(control).to_be_enabled(timeout=12000)
        else:
            expect(control).to_be_disabled(timeout=12000)
        check((control.get_attribute('aria-required') == 'true') == required,
              f'{name} required state changed')
        check(control.get_attribute('maxlength') is None, f'{name} gained an unsupported maximum')
        placeholder = (copy['config']['namePlaceholder'].replace('{{kind}}', copy['kind']['runpod'])
                       if name == 'name' else fields[name]['placeholder'][report['locale']])
        expect(control).to_have_attribute('placeholder', placeholder, timeout=12000)
        expect(control).to_have_attribute('type', 'password' if name == 'api_key' else 'text', timeout=12000)
    expect(dialog.get_by_text(copy['config']['runpodSecureOnly'], exact=True)).to_be_visible(timeout=12000)
    if editing:
        expect(dialog.get_by_text(copy['config']['credentialsRetained'], exact=True)).to_be_visible(timeout=12000)
        # The fixed kind is a display field, not a selectable update value.
        kind = dialog.get_by_label(copy['config']['type'], exact=True)
        expect(kind).to_have_count(1, timeout=12000)
        expect(kind).to_be_visible(timeout=12000)
        expect(kind).to_have_attribute('aria-label', copy['config']['type'], timeout=12000)
        expect(kind).to_be_disabled(timeout=12000)
        expect(kind).to_have_value(copy['kind']['runpod'], timeout=12000)
        expect(dialog.get_by_label(label('api_key'), exact=True)).to_have_value('', timeout=12000)
    else:
        kind = dialog.get_by_label(copy['config']['type'], exact=True)
        expect(kind).to_have_attribute('role', 'combobox', timeout=12000)
        expect(kind).to_be_enabled(timeout=12000)
        expect(dialog.locator('.ant-select-selection-item')).to_have_text(copy['kind']['runpod'], timeout=12000)
        expect(dialog.get_by_text(copy['config']['credentialsRetained'], exact=True)).to_have_count(0)
    report.setdefault('provider_text_contracts', []).append({'editing': editing, 'ready': ready,
        'fields': ['name', 'endpoint', 'api_key'], 'kind_fixed': editing,
        'kind_accessible_name': copy['config']['type'] if editing else None,
        'credential_help': editing, 'runpod_support_hint': True})


def clipboard_editing(page, context, server, dialog, copy, label, name, endpoint, secret, writes, report):
    """Exercise actual clipboard paste and keyboard changes before tracing secrets."""
    if not family_enabled(report, 'clipboard'):
        return
    report['last_step'] = 'provider-text-clipboard-keyboard'
    context.grant_permissions(['clipboard-read', 'clipboard-write'], origin=server.base)
    expect(dialog.locator('textarea')).to_have_count(0, timeout=12000)
    start = len(writes)
    values = ((copy['config']['name'], name), (label('endpoint'), endpoint), (label('api_key'), secret))
    last = None
    try:
        for caption, value in values:
            control = dialog.get_by_label(caption, exact=True)
            control.focus()
            control.press('Control+A')
            control.press('Backspace')
            expect(control).to_have_value('', timeout=12000)
            page.evaluate('value => navigator.clipboard.writeText(value)', value)
            last = value
            control.press('Control+V')
            expect(control).to_have_value(value, timeout=12000)
            control.press('End')
            control.press('Backspace')
            expect(control).to_have_value(value[:-1], timeout=12000)
            control.type(value[-1])
            expect(control).to_have_value(value, timeout=12000)
        check(len(writes) == start, 'clipboard or keyboard editing submitted the provider')
    finally:
        if last is not None and page.evaluate('() => navigator.clipboard.readText()') == last:
            page.evaluate('() => navigator.clipboard.writeText("")')
        check(page.evaluate('() => navigator.clipboard.readText()') != secret,
              'the run secret remains in the browser clipboard')
    report['provider_text_editing'] = {'pasted_fields': ['name', 'endpoint', 'api_key'],
                                      'keyboard_clear_and_replace': True, 'mutation_count': 0,
                                      'secret_removed_from_clipboard': True}


def secret_create(page, server, dialog, copy, label, name, endpoint, secret, writes, errors, report):
    if not family_enabled(report, 'secret'):
        return
    report['last_step'] = 'provider-secret-empty-required'
    key = dialog.get_by_label(label('api_key'), exact=True)
    dialog.get_by_label(copy['config']['name'], exact=True).fill(name)
    dialog.get_by_label(label('endpoint'), exact=True).fill(endpoint)
    key.fill('')
    start = len(writes)
    form_action(dialog, copy['config']['add']).click()
    item = key.locator("xpath=ancestor::*[contains(concat(' ', normalize-space(@class), ' '), ' ant-form-item ')][1]")
    required = copy['config']['fieldRequired'].replace('{{field}}', label('api_key'))
    expect(item.get_by_text(required, exact=True)).to_be_visible(timeout=12000)
    check(collection(page.request.get(server.base + '/api/v1/providers')) == [],
          'empty API key created an account')
    check(len(writes) == start, 'empty required API key sent a provider mutation')
    check(not errors, f'empty-key validation caused a browser error: {errors}')
    report['last_step'] = 'provider-secret-whitespace-create-rejected'
    key.fill('   ')
    with page.expect_response(lambda response: urlsplit(response.url).path == '/api/v1/providers'
                              and response.request.method == 'POST') as failed:
        form_action(dialog, copy['config']['add']).click()
    response = failed.value
    if response.status == 201:
        created = response.json()
        report.setdefault('remaining_resources', []).append({'kind': 'provider', 'id': created.get('id'),
            'scope': 'isolated SQLite database in private/',
            'reclaim': 'discard only this run directory after diagnosis'})
    expected = {'name': name, 'kind': 'runpod', 'endpoint': endpoint, 'api_key': '   '}
    check(response.status == 400 and response.request.post_data_json == expected,
          'whitespace API key create did not reach the real credential refusal')
    reason = 'credential validation failed: runpod: invalid credentials (status 401)'
    check(response.json() == {'error': reason}, 'whitespace API key error body differs')
    message = copy['config']['operationFailed'].replace(
        '{{operation}}', copy['config']['operation']['add']).replace('{{detail}}', reason)
    expect(page.locator('.ant-message-error').filter(has_text=message)).to_be_visible(timeout=12000)
    expect(dialog).to_be_visible(timeout=12000)
    expect(key).to_have_value('   ', timeout=12000)
    check(collection(page.request.get(server.base + '/api/v1/providers')) == [],
          'invalid API key persisted an account')
    report['secret_create_validation'] = {'required_empty_rejected_without_request': True,
        'whitespace_status': 400, 'upstream_status': 401, 'draft_retained': True,
        'persisted_accounts': 0}
    # The real key is never shown while tracing or taking a screenshot.
    key.fill(secret)
    toggle = dialog.locator('.ant-input-password-icon')
    expect(key).to_have_attribute('type', 'password', timeout=12000)
    toggle.click()
    expect(key).to_have_attribute('type', 'text', timeout=12000)
    expect(key).to_have_value(secret, timeout=12000)
    toggle.click()
    expect(key).to_have_attribute('type', 'password', timeout=12000)
    expect(key).to_have_value(secret, timeout=12000)
    check(len(writes) == start + 1, 'password show/hide sent a mutation')
    report['secret_masking'] = {'default': 'password', 'shown': 'text', 'hidden': 'password',
                                'value_preserved': True, 'show_hide_mutations': 0}


def literal_name(page, dialog, copy, name, report):
    if not family_enabled(report, 'literal-name'):
        return
    report['last_step'] = 'provider-name-escaped'
    check(NAME_SUFFIX in name, 'provider fixture lacks the HTML-character name')
    if dialog is None:
        cell = page.get_by_role('cell', name=name, exact=True)
        expect(cell).to_have_count(1, timeout=12000)
        expect(cell).to_have_text(name, timeout=12000)
        expect(cell.locator('b')).to_have_count(0, timeout=12000)
    else:
        expect(dialog.get_by_label(copy['config']['name'], exact=True)).to_have_value(name, timeout=12000)
    report.setdefault('provider_name_escape', []).append({'name': name,
        'surface': 'edit input' if dialog is not None else 'list cell', 'literal': True})


def concurrent_names(page, context, server, dialog, copy, label, name, endpoint, path,
                     read, save, report):
    """Two real editors follow the API's last-writer-wins contract, then reread."""
    if not family_enabled(report, 'concurrent'):
        return name
    report['last_step'] = 'provider-concurrent-last-writer'
    other_name, final_name = name + '-other', name + '-final'
    other = context.browser.new_context(storage_state=context.storage_state(),
        viewport=page.viewport_size, locale='zh-CN' if report['locale'] == 'zh' else 'en-US', timezone_id='UTC')
    try:
        writer = other.new_page()
        failures = []
        writer.on('pageerror', lambda error: failures.append(str(error)))
        writer.goto(server.base + '/providers')
        row = writer.get_by_role('row').filter(has=writer.get_by_text(name, exact=True))
        with writer.expect_response(lambda response: urlsplit(response.url).path == path
                                    and response.request.method == 'GET') as loaded:
            button(row, copy['table']['edit']).click()
        check(loaded.value.status == 200, 'other editor could not read the saved account')
        editor = writer.get_by_role('dialog')
        expect(editor.get_by_label(copy['config']['name'], exact=True)).to_have_value(name, timeout=12000)
        expect(editor.get_by_label(label('endpoint'), exact=True)).to_have_value(endpoint, timeout=12000)
        expect(editor.get_by_label(label('api_key'), exact=True)).to_have_value('', timeout=12000)
        editor.get_by_label(copy['config']['name'], exact=True).fill(other_name)
        with writer.expect_response(lambda response: urlsplit(response.url).path == path
                                    and response.request.method == 'PUT') as updated:
            button(editor, copy['config']['save']).click()
        check(updated.value.status == 200 and updated.value.request.post_data_json
              == {'name': other_name, 'endpoint': endpoint}, 'other editor update differs')
        expect(editor).to_be_hidden(timeout=12000)
        read(endpoint, other_name)
        expect(dialog.get_by_label(copy['config']['name'], exact=True)).to_have_value(name, timeout=12000)
        dialog.get_by_label(copy['config']['name'], exact=True).fill(final_name)
        save(endpoint, final_name, 200)
        read(endpoint, final_name)
        writer.reload()
        expect(writer.get_by_role('cell', name=final_name, exact=True)).to_be_visible(timeout=12000)
        expect(writer.get_by_role('cell', name=other_name, exact=True)).to_have_count(0, timeout=12000)
        check(not failures, f'other editor browser failures: {failures}')
        report['provider_concurrent_edit'] = {'writers': 2, 'first': other_name, 'last': final_name,
            'stale_draft_retained': True, 'last_writer_wins': True, 'other_session_reloaded': True}
        return final_name
    finally:
        other.close()
