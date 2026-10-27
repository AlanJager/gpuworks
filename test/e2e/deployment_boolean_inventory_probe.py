"""Conditional mount-checkbox proof on its own real page; never submits.

Context-level create ledger, independent collection reads and page-local
console/HTTP/errors keep this probe separate from the original sole creation.
"""
from urllib.parse import urljoin, urlsplit

from playwright.sync_api import expect
from local_browser_fixture import http
from platform_token_read_revoke import check


def close_probe(page, context, listener, record, primary=None):
    errors = []
    record['page_created'] = page is not None
    try:
        if page is not None:
            page.close()
            check(page.is_closed(), 'conditional inventory page did not close')
        record['page_closed'] = True if page is not None else None
    except Exception as error:
        record['page_closed'] = False
        errors.append('page close: ' + str(error))
    try:
        context.remove_listener('request', listener)
    except Exception as error:
        errors.append('listener remove: ' + str(error))
    record['cleanup_errors'] = errors
    if errors:
        if primary is None:
            raise AssertionError('; '.join(errors))
        record['primary_failure_preserved'] = str(primary)


def exercise(page, labels, report, token, launch):
    context, probe = page.context, None
    record = {'create_requests': [], 'console_errors': [], 'page_errors': [], 'http_errors': []}
    report['boolean_inventory_conditional_probe'] = record
    base = urljoin(page.url, '/').rstrip('/')
    def collection(label):
        status, document = http(base, 'GET', '/api/v1/deployments', token=token)
        check(status == 200 and document['items'] == [] and document['total'] == 0,
              label + ': conditional probe requires independent empty deployment collection')
        record[label] = {'status': status, 'items': [], 'total': 0}
    def request_seen(request):
        if request.method == 'POST' and urlsplit(request.url).path == '/api/v1/deployments':
            record['create_requests'].append({'method': request.method, 'path': urlsplit(request.url).path})
    collection('before')
    context.on('request', request_seen)
    primary = None
    try:
        probe = context.new_page()
        probe.on('console', lambda message: record['console_errors'].append(message.text) if message.type == 'error' else None)
        probe.on('pageerror', lambda error: record['page_errors'].append(str(error)))
        probe.on('response', lambda response: record['http_errors'].append({
            'method': response.request.method, 'path': urlsplit(response.url).path,
            'status': response.status}) if response.status >= 400 else None)
        probe.goto(urljoin(base, '/deploy'))
        report['last_step'] = 'boolean-inventory-conditional-mount-default-quick'
        add = probe.get_by_role('button', name=labels['containerOverrides']['addMount'], exact=True)
        expect(add).to_be_visible()
        add.click()
        row = probe.locator('.image-defaults-mount-row')
        expect(row).to_have_count(1)
        checkbox = row.get_by_role('checkbox', name=labels['containerOverrides']['readOnly'], exact=True)
        expect(checkbox).to_be_visible()
        expect(checkbox).not_to_be_checked()
        expect(checkbox).to_be_enabled()
        record['quick'] = {'role': 'checkbox', 'name': labels['containerOverrides']['readOnly'],
                           'checked': False, 'disabled': False, 'wire_value': 'unstated default false'}
        launch.mode(probe, labels, 'expert')
        probe.get_by_role('tab', name=labels['page']['step']['runtime'], exact=True).click()
        report['last_step'] = 'boolean-inventory-conditional-mount-default-expert'
        checkbox = probe.locator('.image-defaults-mount-row').get_by_role(
            'checkbox', name=labels['containerOverrides']['readOnly'], exact=True)
        expect(checkbox).to_be_visible()
        expect(checkbox).not_to_be_checked()
        expect(checkbox).to_be_enabled()
        record['expert'] = dict(record['quick'])
        # An explicit choice and return are native checkbox operations; the
        # blank draft has no accelerator, so no evaluate/create is fabricated.
        checkbox.check()
        expect(checkbox).to_be_checked()
        checkbox.uncheck()
        expect(checkbox).not_to_be_checked()
        check(not record['create_requests'], 'conditional mount inventory emitted create')
        check(not record['console_errors'], 'conditional mount console error: ' + str(record['console_errors']))
        check(not record['page_errors'], 'conditional mount page error: ' + str(record['page_errors']))
        check(not record['http_errors'], 'conditional mount HTTP error: ' + str(record['http_errors']))
    except BaseException as error:
        primary = error
        raise
    finally:
        close_probe(probe, context, request_seen, record, primary)
    check(not record['create_requests'], 'conditional mount inventory emitted late create')
    check(not record['console_errors'], 'conditional mount late console error: ' + str(record['console_errors']))
    check(not record['page_errors'], 'conditional mount late page error: ' + str(record['page_errors']))
    check(not record['http_errors'], 'conditional mount late HTTP error: ' + str(record['http_errors']))
    collection('after')
    return {'id': 'conditional/mount-read-only', 'offered': True,
            'role': 'checkbox', 'default': False, 'independent_page': True,
            'context_create_requests': [], 'page_closed': record['page_closed']}
