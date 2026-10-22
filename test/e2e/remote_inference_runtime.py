"""UI-owned inference credential consumed by a real GPU model, then revoked.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: python3 test/e2e/remote_runtime_acceptance.py --config <private-json> --case inference
Environment/config: remote_runtime_acceptance.py module docstring.
Existing remote keys are preserved; one owned guard keeps authentication enabled.
"""
from __future__ import annotations

from datetime import datetime
import json
from urllib.parse import urlsplit

from playwright.sync_api import expect

from inference_key_lifecycle import DENIED, KEY_PATH
from local_browser_fixture import http
from remote_gpu_fixture import public_request
from platform_token_read_revoke import button, modal_geometry, strings


# Mainline: gpuworks/inference-key/real-gpu-generation-revocation; 本轮真实GPU部署和隔离guard Key
# → UI创建、原样复制一次性推理Key → 刷新+GET脱敏持久化 → copied Key真实非流式/流式生成
# → UI取消删除保持可用、确认删除同请求401 → guard仍可真实生成 → 清理guard且原有Key集合不变。
def run_inference(r):
    page, context, artifacts = r.page, r.context, r.artifacts
    k, _ = strings('en')
    assert page.evaluate('Boolean(window.isSecureContext && navigator.clipboard)'), \
        'BLOCKED_INFRA: browser needs an explicitly trusted remote origin for real clipboard readback'
    context.grant_permissions(['clipboard-read', 'clipboard-write'], origin=r.base)
    baseline = r.api('GET', KEY_PATH)
    assert set(baseline) == {'items', 'total'} and baseline['total'] == len(baseline['items'])
    original = {item['prefix']: {key: value for key, value in item.items() if key != 'last_used'}
        for item in baseline['items']}
    name = 'e2e-' + r.run_id + '-gpu-key'
    guard_name = 'e2e-' + r.run_id + '-gpu-guard'
    assert not any(item['name'] in (name, guard_name) for item in baseline['items'])
    artifacts.stop_trace(context)
    r.report['sensitive_phase'] = True
    guard_payload = r.api('POST', KEY_PATH, {'name': guard_name}, expected=201)
    assert set(guard_payload) == {'name', 'key'} and guard_payload['name'] == guard_name
    guard = guard_payload['key']
    artifacts.remember(guard)
    guard_prefix = guard[:12]
    assert guard_prefix not in original
    r.own('inference_key', guard_prefix)
    r.report['sensitive_phase'] = False
    artifacts.start_trace(context)
    assert http(r.base, 'GET', '/v1/models') == (401, DENIED), 'guard must keep gateway authentication enabled'
    r.evidence('fixture', {'guard_prefix': guard_prefix, 'existing_key_count': len(original),
        'reason': 'owned guard preserves auth while UI target is revoked; existing remote keys never deleted'})

    page.goto(r.base + '/apikeys')
    expect(page.get_by_role('heading', name=k['title'], exact=True)).to_be_visible()
    mutations = []
    def record(request):
        path = urlsplit(request.url).path
        if path.startswith(KEY_PATH) and request.method in ('POST', 'DELETE'):
            mutations.append({'method': request.method, 'path': path})
    page.on('request', record)
    page.get_by_role('button').filter(has=page.get_by_text(k['create']['button'], exact=True)).click()
    dialog = page.get_by_role('dialog', name=k['create']['title'], exact=True)
    submit = dialog.get_by_role('button').filter(has=page.get_by_text(k['actions']['create'], exact=True))
    modal_geometry(dialog, submit)
    field = dialog.get_by_label(k['table']['name'], exact=True)
    expect(field).to_have_value('')
    field.fill(name)
    artifacts.stop_trace(context)
    r.report['sensitive_phase'] = True
    with page.expect_response(lambda response: urlsplit(response.url).path == KEY_PATH
            and response.request.method == 'POST') as created:
        submit.click()
    response = created.value
    assert response.status == 201 and response.request.post_data_json == {'name': name}
    assert not urlsplit(response.url).query
    payload = response.json()
    assert set(payload) == {'name', 'key'} and payload['name'] == name
    secret = payload['key']
    artifacts.remember(secret)
    assert isinstance(secret, str) and secret.startswith('gw-') and len(secret) == 51
    prefix = secret[:12]
    assert prefix not in original and prefix != guard_prefix
    r.own('inference_key', prefix)
    secret_dialog = page.get_by_role('dialog', name=k['secret']['title'], exact=True)
    expect(secret_dialog).to_be_visible()
    assert secret_dialog.get_by_text(secret, exact=True).count() == 1
    modal_geometry(secret_dialog, button(secret_dialog, k['secret']['saved']))
    # Canonical UI passes localized tooltips to Typography; CopyBtn uses that
    # string as its accessible name. Older remote builds are a separate blocker.
    secret_dialog.get_by_role('button', name=k['secret']['copy'], exact=True).click()
    copied = page.evaluate('navigator.clipboard.readText()')
    assert copied == secret, 'real clipboard must equal the once-only response/display'
    page.evaluate('navigator.clipboard.writeText("")')
    button(secret_dialog, k['secret']['saved']).click()
    expect(secret_dialog).not_to_be_visible()
    page.wait_for_function('value => !document.documentElement.outerHTML.includes(value)', arg=secret)
    r.report['sensitive_phase'] = False
    artifacts.start_trace(context)
    r.evidence('request', {'method': 'POST', 'path': KEY_PATH, 'body': {'name': name}, 'status': 201,
        'response_fields': ['name', 'key'], 'clipboard': 'exact match; consumed unchanged; secret omitted'})
    page.reload()
    row = page.get_by_role('row').filter(has_text=name)
    expect(row).to_have_count(1)
    expect(row).to_contain_text(prefix)
    persisted = r.api('GET', KEY_PATH)
    assert persisted['total'] == len(original) + 2 == len(persisted['items'])
    item = next(item for item in persisted['items'] if item['prefix'] == prefix)
    assert set(item) == {'name', 'prefix', 'created_at'} and item['name'] == name
    assert datetime.fromisoformat(item['created_at'].replace('Z', '+00:00')).timestamp() > 0
    assert secret not in page.content() and secret not in json.dumps(persisted)
    r.evidence('persistence', {'method': 'GET', 'path': KEY_PATH, 'target': item,
        'refresh': 'one target row, prefix only; full secret absent'})

    generated = r.public_chat(copied, stream=False)
    streamed = r.public_chat(copied, stream=True)
    assert generated['id'] and streamed['id'] and generated['id'] != streamed['id']
    assert generated['content'].strip() and streamed['content'].strip()
    for result in (generated, streamed):
        usage = result['usage']
        assert usage['prompt_tokens'] > 0 and usage['completion_tokens'] > 0
        assert usage['total_tokens'] == usage['prompt_tokens'] + usage['completion_tokens']
    used = next(item for item in r.api('GET', KEY_PATH)['items'] if item['prefix'] == prefix)
    assert used.get('last_used')
    assert {key: value for key, value in used.items() if key != 'last_used'} == item
    r.evidence('runtime', {'deployment_id': r.deployment['id'], 'node_uuid': r.node['uuid'],
        'auth': 'exact copied bearer; fresh transport without admin cookies',
        'path': '/v1/chat/completions', 'nonstream': generated, 'stream': streamed,
        'last_used': used['last_used']})

    deletion = button(row, k['actions']['delete'])
    deletion.click()
    popup = page.get_by_role('tooltip').filter(has_text=k['delete']['confirm'].replace('{{name}}', name))
    expect(popup).to_be_in_viewport(ratio=1)
    button(popup, k['actions']['cancel']).click()
    expect(popup).not_to_be_visible()
    assert mutations == [{'method': 'POST', 'path': KEY_PATH}], 'cancel sent a mutation'
    cancelled_chat = r.public_chat(copied, stream=False)
    assert cancelled_chat['content'].strip() and cancelled_chat['usage']['completion_tokens'] > 0
    deletion.click()
    delete_path = KEY_PATH + '/' + prefix
    with page.expect_response(lambda response: urlsplit(response.url).path == delete_path
            and response.request.method == 'DELETE') as deleted:
        button(popup, k['actions']['delete']).click()
    assert deleted.value.status == 200 and deleted.value.json() == {'status': 'deleted'}
    assert deleted.value.request.post_data is None and not urlsplit(deleted.value.url).query
    expect(row).to_have_count(0)
    assert mutations == [{'method': 'POST', 'path': KEY_PATH}, {'method': 'DELETE', 'path': delete_path}]
    revoked_body = {'model': r.deployment['served_name'], 'messages': [{'role': 'user', 'content': 'Reply with OK.'}],
        'max_tokens': 16, 'stream': False}
    revoked_status, _, revoked_raw = public_request(r, copied, '/v1/chat/completions', revoked_body)
    assert revoked_status == 401 and json.loads(revoked_raw) == DENIED, \
        'revoked bearer retained model-generation access'
    anonymous_status, _, anonymous_raw = public_request(r, '', '/v1/chat/completions', revoked_body)
    assert anonymous_status == 401 and json.loads(anonymous_raw) == DENIED, \
        'deleting the target must not enable anonymous generation'
    guard_chat = r.public_chat(guard, stream=False)
    assert guard_chat['content'].strip() and guard_chat['usage']['completion_tokens'] > 0
    page.reload()
    expect(row).to_have_count(0)
    expect(page.get_by_role('row').filter(has_text=guard_name)).to_have_count(1)
    remaining = r.api('GET', KEY_PATH)
    assert prefix not in {key['prefix'] for key in remaining['items']}
    assert {key['prefix'] for key in remaining['items']} == set(original) | {guard_prefix}
    r.forget('inference_key', prefix)
    r.evidence('request', {'method': 'DELETE', 'path': delete_path, 'status': 200,
        'body': None, 'response': {'status': 'deleted'}, 'cancel_sent_delete': False})
    r.evidence('runtime', {'phase': 'revoked', 'path': '/v1/chat/completions', 'revoked_status': 401,
        'anonymous_status': 401, 'denial': DENIED, 'cancelled_chat': cancelled_chat,
        'guard_still_generates': guard_chat})
    assert r.api('DELETE', KEY_PATH + '/' + guard_prefix) == {'status': 'deleted'}
    final = r.api('GET', KEY_PATH)
    assert final['total'] == len(original) == len(final['items'])
    assert {key['prefix']: {field: value for field, value in key.items() if field != 'last_used'}
        for key in final['items']} == original, 'original remote key metadata changed'
    r.forget('inference_key', guard_prefix)
    page.remove_listener('request', record)
    r.evidence('ui', {'checks': ['named creation/secret dialogs in viewport', 'real once-only copy',
        'secret removed from DOM', 'refresh prefix', 'cancel deletion', 'confirmed deletion and refresh absence']})
    r.evidence('cleanup', {'target_prefix': prefix, 'guard_prefix': guard_prefix,
        'target_deleted_by_ui': True, 'guard_deleted_by_api': True,
        'original_key_count': len(original), 'original_key_metadata_preserved': True})
