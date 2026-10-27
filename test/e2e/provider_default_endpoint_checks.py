"""Required-fields-only UI creation against RunPod's real default HTTPS URL.

The control plane remains real. Its external HTTPS boundary is the strict
CONNECT/TLS fixture; the browser never intercepts a management response.
The prefilled endpoint is present in the POST: this is not an omission test.
"""
from urllib.parse import urlsplit

from playwright.sync_api import expect

from platform_token_read_revoke import button, check, collection

DEFAULT_ENDPOINT = 'https://api.runpod.io/v2'
EXISTING_KEY = ('ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILXGDN/SclOozk1xsDztpmhGiKkkrfQB9SKoO8dSIQQZ '
                'existing@example.com')


def exercise_default_endpoint(page, server, artifacts, report, copy, label, fields, fixture, expected_keys):
    report['last_step'] = 'provider-default-network-isolation'
    updates = page.request.get(server.base + '/api/v1/updates')
    usage = page.request.get(server.base + '/api/v1/usage-report')
    check(updates.status == 200 and updates.json().get('auto_check') is False,
          'provider fixture installation did not disable unrelated automatic update checks')
    check(usage.status == 200 and usage.json().get('enabled') is False,
          'provider fixture installation did not disable unrelated usage reporting')
    report['provider_network_isolation'] = {'auto_check_updates': False, 'usage_report': False,
        'observed_via': ['/api/v1/updates', '/api/v1/usage-report']}
    report['last_step'] = 'provider-default-required-fields-only'
    check(fixture is not None, 'default HTTPS fixture was not installed before server startup')
    check(fields['endpoint']['default_value'] == DEFAULT_ENDPOINT and not fields['endpoint']['required'],
          'RunPod optional endpoint schema/default changed')
    check(fields['api_key']['required'], 'RunPod API key is no longer required')
    check(fixture.requests == [], 'default HTTPS fixture received an unexpected earlier call')
    name = 'e2e-' + report['run_id'] + '-default'
    secret = fixture.protocol.api_key
    artifacts.remember(secret)
    add = copy['page']['addKind'].replace('{{kind}}', copy['kind']['runpod'])
    button(page, add).click()
    dialog = page.get_by_role('dialog')
    expect(dialog.locator('.ant-select-selection-item')).to_have_text(copy['kind']['runpod'])
    endpoint = dialog.get_by_label(label('endpoint'), exact=True)
    key = dialog.get_by_label(label('api_key'), exact=True)
    expect(endpoint).to_have_value(DEFAULT_ENDPOINT, timeout=12000)
    expect(key).to_have_value('', timeout=12000)
    expect(key).to_have_attribute('type', 'password', timeout=12000)
    # Deliberately touch only the two required inputs. In particular, do not
    # fill, clear, or reconstruct the optional endpoint's prefilled value.
    dialog.get_by_label(copy['config']['name'], exact=True).fill(name)
    key.fill(secret)
    with page.expect_response(lambda response: urlsplit(response.url).path == '/api/v1/providers'
                              and response.request.method == 'POST') as created:
        button(dialog, copy['config']['add']).click()
    response = created.value
    body = response.json()
    provider_id = body.get('id') if isinstance(body, dict) else None
    if response.status == 201:
        report.setdefault('remaining_resources', []).append({'kind': 'provider', 'id': provider_id, 'name': name,
            'scope': 'isolated SQLite database in private/',
            'reclaim': 'discard only this run directory after diagnosis'})
    report['last_request'] = {'method': 'POST', 'path': '/api/v1/providers',
        'status': response.status, 'expected_status': 201,
        'body': response.request.post_data_json, 'response_text': response.text()}
    check(response.status == 201, f'default HTTPS account create returned {response.status}')
    check(type(provider_id) is int and provider_id > 0, 'default account id is not a positive integer')
    expected_create = {'kind': 'runpod', 'name': name, 'api_key': secret, 'endpoint': DEFAULT_ENDPOINT}
    check(response.request.post_data_json == expected_create, 'required-only default endpoint POST differs')
    check(body == {'id': provider_id, 'kind': 'runpod', 'name': name}, 'default account create projection differs')
    expect(dialog).to_be_hidden(timeout=12000)
    path = f'/api/v1/providers/{provider_id}'
    expected_read = {'id': provider_id, 'kind': 'runpod', 'name': name, 'endpoint': DEFAULT_ENDPOINT,
                     'public_key': '', 'extra': {}}
    read = page.request.get(server.base + path)
    check(read.status == 200 and read.json() == expected_read,
          'default endpoint independent GET differs or leaks a secret')
    # Refresh proves the rendered row and edit inputs are not optimistic state.
    report['last_step'] = 'provider-default-refresh-and-edit'
    page.reload()
    row = page.get_by_role('row').filter(has=page.get_by_text(name, exact=True))
    expect(row.get_by_role('cell', name=name, exact=True)).to_have_text(name, timeout=12000)
    expect(row.get_by_role('cell', name=copy['kind']['runpod'], exact=True)).to_be_visible(timeout=12000)
    expect(row.get_by_text(copy['table']['state']['valid'], exact=True)).to_be_visible(timeout=12000)
    with page.expect_response(lambda response: urlsplit(response.url).path == path
                              and response.request.method == 'GET') as loaded:
        button(row, copy['table']['edit']).click()
    check(loaded.value.status == 200 and loaded.value.json() == expected_read,
          'browser default-account edit GET differs')
    expect(dialog.get_by_label(copy['config']['name'], exact=True)).to_have_value(name, timeout=12000)
    expect(endpoint).to_have_value(DEFAULT_ENDPOINT, timeout=12000)
    expect(key).to_have_value('', timeout=12000)
    expect(button(dialog, copy['config']['save'])).to_be_enabled(timeout=12000)
    button(dialog, copy['config']['cancel']).click()
    expect(dialog).to_be_hidden(timeout=12000)
    check(collection(page.request.get(server.base + '/api/v1/providers'))[0]['id'] == provider_id,
          'default account is absent from independent list')
    expected_upstream = [('GET', '/v2/pods', 200, True),
                         ('GET', '/v2/account/ssh-keys', 200, True),
                         ('PUT', '/v2/account/ssh-keys', 200, True)]
    report['last_step'] = 'provider-default-https-wire'
    requests = fixture.requests
    report['default_endpoint_tls_events'] = list(fixture.events)
    report['default_endpoint_upstream_requests'] = list(requests)
    check([(r['method'], r['path'], r['status'], r['authorized']) for r in requests] == expected_upstream,
          f'default HTTPS calls differ: {requests}')
    check(requests[1]['response'] == {'keys': [EXISTING_KEY]}, 'existing default-account key oracle differs')
    check(requests[2]['body'] == {'keys': expected_keys} and fixture.protocol.keys == expected_keys,
          'default-account platform registration lost an existing key')
    check([event for event in fixture.events if event['event'] == 'connect']
          == [{'event': 'connect', 'authority': 'api.runpod.io:443', 'accepted': True}] * 3,
          'default URL did not use only the declared CONNECT authority')
    check([event for event in fixture.events if event['event'] == 'sni']
          == [{'event': 'sni', 'hostname': 'api.runpod.io', 'accepted': True}] * 3,
          'default URL TLS SNI differs')
    check([event for event in fixture.events if event['event'] == 'tls']
          == [{'event': 'tls', 'alpn': 'http/1.1'}] * 3,
          'default URL TLS/HTTP negotiation differs')
    report['default_endpoint_creation'] = {'filled_fields': ['name', 'api_key'],
        'untouched_optional_endpoint': DEFAULT_ENDPOINT, 'endpoint_wire_present': True,
        'omission_test': False, 'id': provider_id, 'refreshed_edit': expected_read,
        'secret_empty_in_edit': True, 'upstream': requests, 'tls_events': fixture.events}
    report['evidence'].extend([
        {'layer': 'ui', 'default_required_only': ['name', 'api_key'], 'endpoint_untouched': DEFAULT_ENDPOINT},
        {'layer': 'request', 'method': 'POST', 'path': '/api/v1/providers', 'status': 201, 'body': expected_create},
        {'layer': 'persistence', 'path': path, 'status': 200, 'body': expected_read},
        {'layer': 'runtime', 'boundary': 'offline CONNECT/TLS at unchanged RunPod default URL',
         'requests': requests, 'tls_events': fixture.events}])
    report['last_step'] = 'provider-default-delete'
    button(row, copy['table']['delete']).click()
    with page.expect_response(lambda response: urlsplit(response.url).path == path
                              and response.request.method == 'DELETE') as deleted:
        button(page.get_by_role('tooltip'), copy['table']['delete']).click()
    check(deleted.value.status == 200, 'default account UI delete failed')
    expect(row).to_have_count(0, timeout=12000)
    check(page.request.get(server.base + path).status == 404, 'default account still exists after UI delete')
    check(collection(page.request.get(server.base + '/api/v1/providers')) == [],
          'default account or unexpected provider leaked')
    report['remaining_resources'] = [item for item in report.get('remaining_resources', [])
                                     if not (item.get('kind') == 'provider' and item.get('id') == provider_id)]
    report['evidence'].append({'layer': 'cleanup', 'default_provider_id': provider_id,
                              'deleted_via_browser': True, 'independent_get': 404, 'list': []})
