"""Provider URL UI/wire/storage/outbound checks with strict local boundaries.

No management mocks or API replay in place of browser actions. Valid proxy
query is fixture-owned routing identity. Empty create and empty edit have
separate default-fallback versus merge-retention semantics. Diagnostic flags
isolate faults; formal all runs retain every original account assertion.
"""
from contextlib import contextmanager
import json
import traceback
from urllib.parse import urlsplit

from playwright.sync_api import expect

from platform_token_read_revoke import button, check, collection
from provider_default_endpoint_checks import EXISTING_KEY
from provider_endpoint_fixture import RunPodFixture
from provider_form_checks import family_enabled, form_action

PODS = {'pods': [], 'pagination': {'nextCursor': None, 'hasNextPage': False}}


def needs_default_fixture(report):
    return any(family_enabled(report, family) for family in ('url', 'url-errors', 'url-default'))


def _pods(path, status=200):
    response = PODS if status == 200 else {'title': 'Not Found', 'status': 404, 'detail': 'resource not found'}
    return {'method': 'GET', 'path': path, 'status': status, 'authorized': True,
            'body': None, 'response': response}


def _keys(path, keys, *, write=False):
    return {'method': 'PUT' if write else 'GET', 'path': path, 'status': 200, 'authorized': True,
            'body': {'keys': list(keys)} if write else None, 'response': {'keys': list(keys)}}


def assert_calls(actual, expected, scope):
    check(actual == expected, f'{scope}: external method/path/query/auth/body/response/order differs')


def close_fixture(fixture, report, label, *, started=True):
    """Attempt every close stage and require independently observed release."""
    failures = []
    stages = ([('shutdown', fixture.server.shutdown)] if started else [])
    stages.append(('listener-and-handlers', fixture.server.server_close))
    if started:
        stages.append(('join-loop', lambda: fixture.thread.join(timeout=5)))
    for stage, action in stages:
        try:
            action()
        except Exception:
            failures.append({'stage': stage, 'error': traceback.format_exc()})
    observed = {'loop_stopped': not fixture.thread.is_alive(),
                'listener_closed': fixture.server.socket.fileno() == -1}
    report.setdefault('provider_url_fixture_cleanup', {})[label] = {**observed, 'failures': failures}
    if failures or not all(observed.values()):
        report['status'] = 'FAIL_TEST'
        if not all(observed.values()):
            report.setdefault('remaining_resources', []).append({'kind': 'fixture', 'name': label,
                'scope': 'owned local RunPod HTTP boundary', 'endpoint': fixture.base,
                'reclaim': 'stop only this run process and its recorded local listener'})
        raise AssertionError(f'{label}: external fixture cleanup failed; observed={observed}, failures={failures}')


@contextmanager
def _fixture(secret, report, label, *, query=None):
    fixture = RunPodFixture(secret, keys=[EXISTING_KEY], query=query)
    started = False
    try:
        fixture.__enter__()
        started = True
        yield fixture
    finally:
        report.setdefault('provider_url_external_requests', {})[label] = list(fixture.requests)
        close_fixture(fixture, report, label, started=started)


def exercise_provider_urls(page, context, server, artifacts, report, copy, label, default_fixture, expected_keys):
    if not needs_default_fixture(report):
        return []
    check(default_fixture is not None, 'URL diagnosis requires default TLS fixture before server startup')
    expected_errors = []
    dialog = page.get_by_role('dialog')
    endpoint_input = dialog.get_by_label(label('endpoint'), exact=True)
    name_input = dialog.get_by_label(copy['config']['name'], exact=True)
    key_input = dialog.get_by_label(label('api_key'), exact=True)
    records = report['provider_url_cases'] = []
    baseline = collection(page.request.get(server.base + '/api/v1/providers'))
    check(baseline == [], 'original provider chain left an account before URL cases')

    def new_dialog(name, secret, raw_endpoint):
        # Secret entry is deliberately absent from every trace segment.
        artifacts.stop_trace(context)
        artifacts.remember(secret)
        page.reload()
        add = copy['page']['addKind'].replace('{{kind}}', copy['kind']['runpod'])
        button(page, add).click()
        expect(endpoint_input).to_have_value('https://api.runpod.io/v2', timeout=12000)
        name_input.fill(name)
        key_input.fill(secret)
        expect(key_input).to_have_attribute('type', 'password', timeout=12000)
        endpoint_input.fill(raw_endpoint)
        expect(endpoint_input).to_have_value(raw_endpoint, timeout=12000)

    def read(account, saved_endpoint):
        path = f"/api/v1/providers/{account['id']}"
        expected = {'id': account['id'], 'kind': 'runpod', 'name': account['name'],
                    'endpoint': saved_endpoint, 'public_key': '', 'extra': {}}
        response = page.request.get(server.base + path)
        check(response.status == 200 and response.json() == expected,
              'URL independent GET normalized stored input or exposed secret fields')
        report['evidence'].append({'layer': 'persistence', 'path': path, 'body': expected, 'status': 200,
                                  'step': report.get('last_step')})
        return expected

    def edit(account, saved_endpoint):
        expected = read(account, saved_endpoint)
        page.reload()
        row = page.get_by_role('row').filter(has=page.get_by_text(account['name'], exact=True))
        expect(row.get_by_role('cell', name=account['name'], exact=True)).to_have_text(account['name'], timeout=12000)
        path = f"/api/v1/providers/{account['id']}"
        with page.expect_response(lambda response: urlsplit(response.url).path == path
                                  and response.request.method == 'GET') as loaded:
            button(row, copy['table']['edit']).click()
        check(loaded.value.status == 200 and loaded.value.json() == expected, 'URL browser edit GET differs')
        expect(form_action(dialog, copy['config']['save'])).to_be_enabled(timeout=12000)
        expect(name_input).to_have_value(account['name'], timeout=12000)
        expect(endpoint_input).to_have_value(saved_endpoint, timeout=12000)
        expect(key_input).to_have_value('', timeout=12000)
        report['evidence'].append({'layer': 'ui', 'url_account_id': account['id'],
            'refreshed_edit_endpoint': saved_endpoint, 'secret_empty': True})

    def submit(name, raw_endpoint, *, account=None, secret=None, reason=None, stage):
        report['last_step'] = 'provider-url-' + stage
        creating = account is None
        method = 'POST' if creating else 'PUT'
        path = '/api/v1/providers' if creating else f"/api/v1/providers/{account['id']}"
        wire = {'name': name, 'endpoint': raw_endpoint}
        if creating:
            wire.update(kind='runpod', api_key=secret)
        expect(name_input).to_have_value(name, timeout=12000)
        expect(endpoint_input).to_have_value(raw_endpoint, timeout=12000)
        with page.expect_response(lambda response: urlsplit(response.url).path == path
                                  and response.request.method == method) as changed:
            form_action(dialog, copy['config']['add' if creating else 'save']).click()
        response = changed.value
        body = response.json()
        if creating and response.status == 201:
            # Register any returned ID before validating other response fields.
            report.setdefault('remaining_resources', []).append({'kind': 'provider',
                'id': body.get('id') if isinstance(body, dict) else None, 'name': name,
                'scope': 'isolated SQLite database in private/',
                'reclaim': 'restore only this run private archive/DB for diagnosis'})
        expected_status = 400 if reason else 201 if creating else 200
        report['last_request'] = {'method': method, 'path': path, 'body': response.request.post_data_json,
            'status': response.status, 'expected_status': expected_status, 'response': body}
        check(response.request.post_data_json == wire, 'URL browser wire input differs from the typed raw value')
        check(response.status == expected_status,
              f'{stage}: expected {expected_status}, received {response.status}')
        report['evidence'].append({'layer': 'request', 'method': method, 'path': path,
                                  'body': wire, 'status': expected_status})
        if reason:
            check(body == {'error': reason}, f'{stage}: exact Go/provider validation error differs')
            message = copy['config']['operationFailed'].replace(
                '{{operation}}', copy['config']['operation']['add' if creating else 'update']).replace('{{detail}}', reason)
            notice = page.locator('.ant-message-error').filter(has_text=message)
            expect(notice).to_be_visible(timeout=12000)
            expect(notice).to_have_text(message, timeout=12000)
            expect(dialog).to_be_visible(timeout=12000)
            expect(endpoint_input).to_have_value(raw_endpoint, timeout=12000)
            expect(name_input).to_have_value(name, timeout=12000)
            if creating:
                expect(key_input).to_have_value(secret, timeout=12000)
                expect(key_input).to_have_attribute('type', 'password', timeout=12000)
            expected_errors.append({'path': path, 'status': 400, 'method': method})
        else:
            if creating:
                account = {'id': body.get('id'), 'name': name}
                check(type(account['id']) is int and account['id'] > 0, 'URL account lacks a positive integer ID')
            check(body == {'id': account['id'], 'kind': 'runpod', 'name': name}, 'URL write projection differs')
            expect(dialog).to_be_hidden(timeout=12000)
            if creating:
                artifacts.start_trace(context)
        records.append({'stage': stage, 'input_endpoint': raw_endpoint, 'method': method,
                        'status': expected_status, 'account_id': account['id'] if account else None,
                        'error': body if reason else None, 'localized_error': message if reason else None})
        return account

    def remove(account):
        report['last_step'] = 'provider-url-delete'
        row = page.get_by_role('row').filter(has=page.get_by_text(account['name'], exact=True))
        button(row, copy['table']['delete']).click()
        path = f"/api/v1/providers/{account['id']}"
        with page.expect_response(lambda response: urlsplit(response.url).path == path
                                  and response.request.method == 'DELETE') as deleted:
            button(page.get_by_role('tooltip'), copy['table']['delete']).click()
        check(deleted.value.status == 200, 'URL account UI cleanup failed')
        expect(row).to_have_count(0, timeout=12000)
        check(page.request.get(server.base + path).status == 404, 'deleted URL account detail remains readable')
        check(collection(page.request.get(server.base + '/api/v1/providers')) == [], 'URL account leaked in list')
        report['remaining_resources'] = [entry for entry in report.get('remaining_resources', [])
            if not (entry.get('kind') == 'provider' and entry.get('id') == account['id'])]
        report['evidence'].append({'layer': 'cleanup', 'url_provider_id': account['id'],
                                  'ui_delete': 200, 'independent_detail': 404, 'list': []})

    # Error diagnosis precedes positive query cases; independent flags prevent
    # a query mutation from masking the malformed-URL rejection assertions.
    if family_enabled(report, 'url-errors'):
        secret = 'e2e-runpod-url-errors-' + report['run_id']
        artifacts.remember(secret)
        with _fixture(secret, report, 'url-errors') as fixture:
            base = fixture.base
            name = 'e2e-' + report['run_id'] + '-url-errors'
            bad_ftp = base.replace('http://', 'ftp://') + '/proxy-a/v2'
            reason = 'credential validation failed: Get ' + json.dumps(bad_ftp + '/pods') + ': unsupported protocol scheme "ftp"'
            default_before = list(default_fixture.requests)
            new_dialog(name, secret, bad_ftp)
            submit(name, bad_ftp, secret=secret, reason=reason, stage='ftp-rejected')
            unexpected = collection(page.request.get(server.base + '/api/v1/providers'))
            for row in unexpected:
                if row.get('name') == name:
                    report.setdefault('remaining_resources', []).append({'kind': 'provider', 'id': row['id'],
                        'name': name, 'scope': 'isolated SQLite database in private/',
                        'reclaim': 'restore only this run private archive/DB for diagnosis'})
            check(unexpected == [], 'bad create persisted a URL account')
            assert_calls(fixture.requests, [], 'FTP create')
            check(default_fixture.requests == default_before, 'bad protocol reached default upstream')
            good = base + '/proxy-a/v2'
            endpoint_input.fill(good)
            account = submit(name, good, secret=secret, stage='ftp-corrected-create')
            planned = [_pods('/proxy-a/v2/pods'), _keys('/proxy-a/v2/account/ssh-keys', [EXISTING_KEY]),
                       _keys('/proxy-a/v2/account/ssh-keys', expected_keys, write=True)]
            assert_calls(fixture.requests, planned, 'corrected create')
            read(account, good)
            cases = [
                ('missing-host', 'http:///proxy-a/v2',
                 'credential validation failed: Get "http:///proxy-a/v2/pods": http: no Host in request URL'),
                ('port-abc', 'http://127.0.0.1:abc/proxy-a/v2',
                 'credential validation failed: parse "http://127.0.0.1:abc/proxy-a/v2": invalid port ":abc" after host'),
                ('port-65536', 'http://127.0.0.1:65536/proxy-a/v2',
                 'credential validation failed: Get "http://127.0.0.1:65536/proxy-a/v2/pods": dial tcp: address 65536: invalid port'),
                ('duplicate-declared-prefix', base + '/proxy-a/v2/v2',
                 'credential validation failed: runpod: validate credentials: unexpected status 404')]
            for number, (stage, invalid, error) in enumerate(cases):
                edit(account, good)
                endpoint_input.fill(invalid)
                submit(name, invalid, account=account, reason=error, stage=stage + '-rejected')
                if stage == 'duplicate-declared-prefix':
                    planned.append(_pods('/proxy-a/v2/v2/pods', 404))
                assert_calls(fixture.requests, planned, stage + ' refusal')
                check(default_fixture.requests == default_before, 'invalid URL contacted default upstream')
                read(account, good)
                corrected_prefix = '/proxy-b/v2' if number % 2 == 0 else '/proxy-a/v2'
                corrected = base + corrected_prefix
                endpoint_input.fill(corrected)
                submit(name, corrected, account=account, stage=stage + '-corrected')
                planned.append(_pods(corrected_prefix + '/pods'))
                assert_calls(fixture.requests, planned, stage + ' recovery')
                good = corrected
                read(account, good)
            check(fixture.keys == expected_keys, 'URL error recovery changed unrelated external account keys')
            remove(account)
            assert_calls(fixture.requests, planned, 'URL errors complete')
            report['evidence'].append({'layer': 'runtime', 'url_errors': list(fixture.requests)})

    if family_enabled(report, 'url'):
        secret = 'e2e-runpod-url-query-' + report['run_id']
        artifacts.remember(secret)
        query = 'tenant=e2e-' + report['run_id'] + '&label=one%2Ftwo%2Bthree&tail=value/'
        with _fixture(secret, report, 'url', query=query) as fixture:
            name = 'e2e-' + report['run_id'] + '-url-query'
            raw_a = '  ' + fixture.base + '/proxy-a/v2///?' + query + '  '
            raw_b = fixture.base + '/proxy-b/v2/?' + query
            default_before = list(default_fixture.requests)
            new_dialog(name, secret, raw_a)
            account = submit(name, raw_a, secret=secret, stage='raw-query-create')
            suffix = '?' + query
            planned = [_pods('/proxy-a/v2/pods' + suffix),
                       _keys('/proxy-a/v2/account/ssh-keys' + suffix, [EXISTING_KEY]),
                       _keys('/proxy-a/v2/account/ssh-keys' + suffix, expected_keys, write=True)]
            assert_calls(fixture.requests, planned, 'raw HTTP create normalization')
            edit(account, raw_a)
            endpoint_input.fill(raw_b)
            submit(name, raw_b, account=account, stage='raw-query-update')
            planned.append(_pods('/proxy-b/v2/pods' + suffix))
            assert_calls(fixture.requests, planned, 'raw HTTP update normalization')
            edit(account, raw_b)
            endpoint_input.fill('')
            submit(name, '', account=account, stage='empty-edit-retains-override')
            planned.append(_pods('/proxy-b/v2/pods' + suffix))
            assert_calls(fixture.requests, planned, 'empty edit retains proxy')
            edit(account, raw_b)
            form_action(dialog, copy['config']['cancel']).click()
            expect(dialog).to_be_hidden(timeout=12000)
            check(default_fixture.requests == default_before, 'HTTP overrides unexpectedly used the default upstream')
            check(fixture.keys == expected_keys, 'query URL changes dropped external keys')
            remove(account)
            assert_calls(fixture.requests, planned, 'URL query complete')
            report['evidence'].append({'layer': 'runtime', 'url_query': list(fixture.requests),
                'raw_endpoint': raw_a, 'stored_endpoint': raw_a, 'raw_query': query})

    if family_enabled(report, 'url-default'):
        report['last_step'] = 'provider-url-empty-create-default-plan'
        original_default_ran = family_enabled(report, 'default-endpoint')
        initial_keys = expected_keys if original_default_ran else [EXISTING_KEY]
        expected_prefix = ([_pods('/v2/pods'), _keys('/v2/account/ssh-keys', [EXISTING_KEY]),
                            _keys('/v2/account/ssh-keys', expected_keys, write=True)] if original_default_ran else [])
        assert_calls(default_fixture.requests, expected_prefix, 'known prior default-account operations')
        check(default_fixture.protocol.keys == initial_keys, 'default account keys diverged from the prior scenario plan')
        name = 'e2e-' + report['run_id'] + '-url-empty'
        secret = default_fixture.protocol.api_key
        new_dialog(name, secret, '')
        account = submit(name, '', secret=secret, stage='empty-create-default')
        delta = [_pods('/v2/pods'), _keys('/v2/account/ssh-keys', initial_keys)]
        if not original_default_ran:
            delta.append(_keys('/v2/account/ssh-keys', expected_keys, write=True))
        assert_calls(default_fixture.requests, expected_prefix + delta, 'empty create default fallback')
        check(default_fixture.protocol.keys == expected_keys, 'empty create lost or duplicated an external account key')
        expected_events = [{'event': 'connect', 'authority': 'api.runpod.io:443', 'accepted': True},
                           {'event': 'sni', 'hostname': 'api.runpod.io', 'accepted': True},
                           {'event': 'tls', 'alpn': 'http/1.1'}] * (len(expected_prefix) + len(delta))
        check(list(default_fixture.events) == expected_events, 'empty create default TLS identity/count differs')
        edit(account, '')
        form_action(dialog, copy['config']['cancel']).click()
        expect(dialog).to_be_hidden(timeout=12000)
        remove(account)
        report['provider_url_default'] = {'endpoint_wire': '', 'stored_endpoint': '',
            'refreshed_edit_endpoint': '', 'omission_test': False, 'prior_default_creation': original_default_ran,
            'expected_new_upstream_calls': len(delta), 'requests': list(default_fixture.requests),
            'tls_events': list(default_fixture.events)}
        report['evidence'].append({'layer': 'runtime', 'empty_endpoint_default': report['provider_url_default']})
    report['evidence'].append({'layer': 'ui', 'url_cases': records})
    return expected_errors
