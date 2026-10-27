"""Replace a RunPod key and prove persisted use without returning its plaintext.

Only the fixture's independently issued valid key changes. This is credential
replacement, not a claim about expiry, revocation or the full 7.4 lifecycle.
"""
from playwright.sync_api import expect
from platform_token_read_revoke import button, check, collection
from provider_form_checks import family_enabled
from provider_text_value_checks import (
    AccountFlow, ROOT, external_fixture, pods_call, registration_calls, type_secret,
)


def exercise_key_rotation(page, context, server, artifacts, report, copy, label,
                          default_fixture=None, expected_keys=()):
    if not family_enabled(report, 'key-rotation'):
        return []
    key_a = 'e2e-provider-key-a-' + report['run_id']
    key_b = 'e2e-provider-key-b-' + report['run_id']
    artifacts.remember(key_a)
    artifacts.remember(key_b)
    flow = AccountFlow(page, context, server, artifacts, report, copy, label, 'provider_key_rotation')
    try:
        with external_fixture(key_a, report, 'key-rotation', expected_keys) as fixture:
            flow.watch_external(fixture)
            name = 'e2e-' + report['run_id'] + '-key-rotation'
            endpoint = fixture.base + '/proxy-a/v2'
            before, keys = len(fixture.requests), list(fixture.keys)
            flow.new(name, endpoint, key_a)
            account = flow.submit({'kind': 'runpod', 'name': name, 'endpoint': endpoint, 'api_key': key_a},
                                  stage='provider-key-rotation-create-a')
            expected, after = registration_calls(keys, expected_keys)
            flow.verify_external(fixture, before, expected, 'key A create')
            check(fixture.keys == after, 'key rotation fixture lost existing SSH keys')
            original = dict(account)
            flow.edit(account)
            # This boundary fact is independent of the product's encrypted DB:
            # only B authenticates from now on, so an ignored update cannot pass.
            fixture.api_key = key_b
            type_secret(flow.key, key_b, context, artifacts)
            before = len(fixture.requests)
            account = flow.submit({'name': name, 'endpoint': endpoint, 'api_key': key_b}, account=account,
                                  stage='provider-key-rotation-a-to-b')
            flow.verify_external(fixture, before, [pods_call()], 'explicit key B validation')
            check(account == original, 'rotating only the secret changed public configuration')
            flow.sync(account, fixture, 'provider-key-rotation-stored-b-sync')

            # net/http rejects NUL before dialing. The target is a fixture-owned
            # ASCII URL; Go's url.Error adds Get + quoted URL, and transport
            # reports only the field name, never the invalid credential value.
            flow.edit(account)
            report['last_step'] = 'provider-key-rotation-invalid-header-input'
            type_secret(flow.key, key_b + '\x00', context, artifacts)
            before = len(fixture.requests)
            reason = ('credential validation failed: Get "' + endpoint + '/pods": '
                      'net/http: invalid header field value for "Authorization"')
            flow.submit({'name': name, 'endpoint': endpoint, 'api_key': key_b + '\x00'},
                        account=account, rejected=True, expected_reason=reason,
                        stage='provider-key-rotation-invalid-header-rejected')
            flow.verify_external(fixture, before, [], 'invalid header never reaches the external API')
            type_secret(flow.key, '', context, artifacts)
            before = len(fixture.requests)
            account = flow.submit({'name': name, 'endpoint': endpoint, 'api_key': ''}, account=account,
                                  stage='provider-key-rotation-invalid-header-cleared')
            flow.verify_external(fixture, before, [pods_call()], 'cleared invalid header retains key B')
            check(account == original, 'invalid key draft changed public configuration')
            flow.sync(account, fixture, 'provider-key-rotation-invalid-header-retained-b-sync')

            for suffix, patch in [('empty', {}), ('whitespace', {'api_key': '   '})]:
                flow.edit(account)
                expect(flow.key).to_have_value('')
                if patch:
                    type_secret(flow.key, patch['api_key'], context, artifacts)
                before = len(fixture.requests)
                account = flow.submit({'name': name, 'endpoint': endpoint, **patch}, account=account,
                                      stage='provider-key-rotation-b-retained-' + suffix)
                flow.verify_external(fixture, before, [pods_call()], 'retained key B validation')
                check(account == original, 'blank secret edit changed another field')
                flow.sync(account, fixture, 'provider-key-rotation-b-retained-' + suffix + '-sync')
            flow.edit(account)
            expect(flow.key).to_have_value('')
            button(flow.dialog, copy['config']['cancel']).click()
            expect(flow.dialog).to_be_hidden()
            flow.remove(account)
            flow.verify_all_external()
            flow.assert_no_owned_accounts()
            check(collection(page.request.get(server.base + ROOT)) == [], 'key rotation left an account')
            report['provider_key_rotation_scope'] = {'a_to_b': True, 'new_key_validated': True,
                'persisted_b_used_by_ui_sync': True, 'empty_omitted_and_whitespace_retains_b': True,
                'public_configuration_unchanged': True, 'secret_exposed_by_get': False,
                'invalid_header_rejected_before_external_request': True,
                'invalid_draft_cleared_and_stored_b_authenticated': True,
                'full_2_4_character_matrix': False,
                'full_7_4_lifecycle': False}
            return list(flow.errors)
    finally:
        flow.finish()
