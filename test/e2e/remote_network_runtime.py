"""Real serving-network binding and GPU inference, on an explicitly selected node.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: python3 test/e2e/remote_runtime_acceptance.py --config <private-json> --case network
Environment/config: remote_runtime_acceptance.py module docstring.
The fixture owns the model deployment/API key; this case owns only its network.
"""
from __future__ import annotations

import ipaddress
import json
from urllib.parse import parse_qs, urlsplit

from playwright.sync_api import expect

from local_browser_fixture import ROOT
from network_storage_lifecycle import mutate
from platform_token_read_revoke import button, modal_geometry


# Mainline: gpuworks/network/serving-direct-real-gpu; 本轮真实GPU deployment + management基线
# → UI创建serving以太网定义并选择真实节点 → 刷新和GET验证唯一绑定/真实网卡地址
# → 同模型真实chat及direct forward证据 → UI解绑、删除 → management恢复且同模型仍可推理。
def run_network(r):
    page = r.page
    t = json.loads((ROOT / 'ui/src/i18n/locales/en/networks.json').read_text())
    node_uuid = r.node['uuid']
    node_path = '/api/v1/nodes/' + node_uuid + '/networks'
    baseline = r.api('GET', node_path)
    serving = [n for n in baseline['networks'] if n['role'] == 'serving']
    assert len(serving) == 1 and serving[0]['source'] == 'management' and serving[0]['network_id'] == 0, \
        'BLOCKED_INFRA: dedicated serving binding exists or management serving is disabled'
    assert baseline['serving_forward'] is None, 'BLOCKED_INFRA: existing dedicated forwarder'
    assert baseline['inventory_reported'], 'BLOCKED_INFRA: agent network inventory required'
    management = baseline['management']
    address = ipaddress.ip_address(management['ip'])
    assert address.version == 4 and not address.is_loopback, 'BLOCKED_INFRA: non-loopback IPv4 required'
    interface = management['interface']
    candidates = [i for i in baseline['interfaces'] if i['name'] == interface]
    assert len(candidates) == 1 and candidates[0]['state'] == 'up', 'BLOCKED_INFRA: management NIC is not up'
    assert any(ipaddress.ip_interface(value).ip == address for value in candidates[0]['addrs']), \
        'BLOCKED_INFRA: management address absent from agent NIC inventory'
    instances = r.api('GET', '/api/v1/instances')['items']
    node_instances = [i for i in instances if i['node_uuid'] == node_uuid]
    assert node_instances and all(i['deployment_id'] == r.deployment['id'] for i in node_instances), \
        'BLOCKED_INFRA: serving changes would affect another deployment on this server'
    assert all(i['state'] == 'running' and i.get('container_id') for i in node_instances), \
        'BLOCKED_INFRA: owned deployment must have real running containers'
    host_nic = r.host(
        'import subprocess\n'
        'interfaces=json.loads(subprocess.check_output(["ip","-j","address","show","dev",'
        + repr(interface) + '], text=True))\n'
        'result={"interfaces":interfaces}\n'
    )
    assert any(item['local'] == str(address) for nic in host_nic['interfaces'] for item in nic['addr_info']), \
        'agent inventory address is not present on the real host'
    baseline_chat = r.chat()
    r.evidence('runtime', {'phase': 'management-baseline', 'node_uuid': node_uuid,
        'deployment_id': r.deployment['id'], 'instances': [i['uuid'] for i in node_instances],
        'host_interface': host_nic, 'networks': baseline['networks'],
        'chat_id': baseline_chat['id'], 'usage': baseline_chat['usage']})

    name = 'e2e-' + r.run_id + '-serving'
    draft = {'name': name, 'description': 'Owned real serving path acceptance', 'roles': ['serving'],
        'type': 'host_ethernet', 'cidr': str(address) + '/32', 'interface': interface}
    prefix = '/api/v1/networks'
    page.goto(r.base + '/networks')
    expect(page.get_by_role('heading', name=t['title'], exact=True)).to_be_visible()
    page.get_by_role('button').filter(has=page.get_by_text(t['actions']['create'], exact=True)).click()
    dialog = page.get_by_role('dialog', name=t['form']['createTitle'], exact=True)
    modal_geometry(dialog, button(dialog, t['actions']['save']))
    for field in ('name', 'description', 'cidr', 'interface'):
        dialog.get_by_label(t['form'][field], exact=True).fill(draft[field])
    expect(dialog.get_by_role('radio', name=t['type']['host_ethernet'], exact=True)).to_be_checked()
    dialog.get_by_text(t['role']['serving'], exact=True).click()
    expect(dialog.get_by_role('checkbox', name=t['role']['serving'], exact=True)).to_be_checked()
    evidence = []
    created = mutate(page, button(dialog, t['actions']['save']).click, 'POST', prefix, draft, 201, evidence)
    network_id = created['id']
    r.own('network', network_id)
    path = prefix + '/' + str(network_id)
    expect(page).to_have_url(r.base + '/networks/' + str(network_id))
    expect(page.get_by_role('heading', name=name, exact=True)).to_be_visible()
    for item in evidence:
        r.evidence(item.pop('layer'), item)
    saved = r.api('GET', path)
    assert all(saved[key] == value for key, value in draft.items())
    assert saved['attachments'] == []

    button(page, t['actions']['attach']).click()
    dialog = page.get_by_role('dialog', name=t['attach']['title'].replace('{{name}}', name), exact=True)
    modal_geometry(dialog, button(dialog, t['actions']['save']))
    expect(dialog.get_by_role('radio', name=t['attach']['modeNode'], exact=True)).to_be_checked()
    picker = dialog.get_by_role('combobox', name=t['attach']['nodes'], exact=True)
    node_name = r.node.get('name', '').strip() or r.node.get('hostname', '').strip() or node_uuid[:8]
    picker.click()
    with page.expect_response(lambda response: urlsplit(response.url).path == '/api/v1/nodes'
            and parse_qs(urlsplit(response.url).query).get('q') == [node_name]) as search:
        picker.fill(node_name)
    assert search.value.status == 200
    options = search.value.json()['items']
    assert len(options) == 1 and options[0]['uuid'] == node_uuid, \
        'BLOCKED_INFRA: selected node must have a unique searchable human-readable name'
    expect(page.get_by_role('option', name=node_name, exact=True)).to_have_count(1)
    picker.press('ArrowDown')
    picker.press('Enter')
    expect(dialog.get_by_text(node_name, exact=True)).to_be_visible()
    evidence = []
    attached = mutate(page, button(dialog, t['actions']['save']).click, 'POST', path + '/attachments/batch',
        {'node_uuids': [node_uuid]}, 200, evidence)
    assert attached['results'] == [{'node_uuid': node_uuid, 'status': 'attached'}]
    for item in evidence:
        r.evidence(item.pop('layer'), item)
    expect(dialog).not_to_be_visible()
    page.reload()
    expect(page.get_by_role('heading', name=name, exact=True)).to_be_visible()
    saved = r.api('GET', path)
    assert all(saved[key] == value for key, value in draft.items())
    assert len(saved['attachments']) == 1 and saved['attachments'][0]['node_uuid'] == node_uuid
    attachment_id = saved['attachments'][0]['id']
    assert len(saved['nodes']) == 1 and saved['nodes'][0]['node_uuid'] == node_uuid
    assert saved['nodes'][0]['ip'] == str(address) and saved['nodes'][0]['interface'] == interface
    assert saved['nodes'][0]['warnings'] == []
    row = page.get_by_role('row').filter(has_text=node_name).filter(has=page.get_by_role('button', name=t['actions']['detach'], exact=True))
    expect(row).to_have_count(1)
    expect(page.get_by_role('row').filter(has_text=node_name).filter(has_text=str(address))).to_have_count(1)
    selected = r.api('GET', node_path)
    selected_serving = [n for n in selected['networks'] if n['role'] == 'serving']
    assert len(selected_serving) == 1
    assert selected_serving[0]['network_id'] == network_id and selected_serving[0]['source'] == 'node'
    assert selected_serving[0]['attachment_id'] == attachment_id and selected_serving[0]['warnings'] == []
    assert selected_serving[0]['ip'] == str(address) and selected_serving[0]['interface'] == interface
    assert selected['management'] == management
    r.evidence('persistence', {'network': saved, 'node_networks': selected})

    direct_chat = r.chat()
    forward = r.api('GET', node_path)['serving_forward']
    assert forward['address'] == str(address) + ':19443'
    assert forward.get('mode') == 'direct' and not forward.get('reason'), \
        'successful inference must use the explicit direct serving forwarder'
    assert forward.get('at'), 'direct connection observation must have a timestamp'
    r.evidence('runtime', {'phase': 'dedicated-serving', 'network_id': network_id,
        'serving_forward': forward, 'chat_id': direct_chat['id'], 'usage': direct_chat['usage'],
        'path_contract': 'explicit serving uses the TLS forwarder; no tunnel fallback'})

    button(row, t['actions']['detach']).click()
    popup = page.get_by_role('tooltip').filter(has_text=t['actions']['confirmDetach'])
    expect(popup).to_be_in_viewport(ratio=1)
    evidence = []
    detached = mutate(page, button(popup, t['actions']['detach']).click, 'DELETE',
        path + '/attachments/' + str(attachment_id), None, 200, evidence)
    assert detached['attachments'] == [] and detached['nodes'] == []
    for item in evidence:
        r.evidence(item.pop('layer'), item)
    page.reload()
    expect(page.get_by_text(t['detail']['noAttachments'], exact=True)).to_be_visible()
    after_detach = r.api('GET', path)
    assert after_detach['attachments'] == [] and after_detach['nodes'] == []
    restored = r.api('GET', node_path)
    assert restored['networks'] == baseline['networks'] and restored['management'] == management
    assert restored['serving_forward'] is None

    button(page, t['actions']['delete']).click()
    confirmation = t['actions']['confirmDelete'].replace('{{name}}', name)
    popup = page.get_by_role('tooltip').filter(has_text=confirmation)
    expect(popup).to_be_in_viewport(ratio=1)
    evidence = []
    deleted = mutate(page, button(popup, t['actions']['delete']).click, 'DELETE', path, None, 200, evidence)
    assert deleted == {'status': 'deleted'}
    for item in evidence:
        r.evidence(item.pop('layer'), item)
    expect(page).to_have_url(r.base + '/networks')
    page.reload()
    expect(page.get_by_role('heading', name=t['title'], exact=True)).to_be_visible()
    expect(page.get_by_role('row').filter(has_text=name)).to_have_count(0)
    assert r.api('GET', path, expected=404)['code'] == 'NETWORK_NOT_FOUND'
    assert network_id not in [n['id'] for n in r.api('GET', prefix)['items']]
    restored_chat = r.chat()
    restored = r.api('GET', node_path)
    assert restored['networks'] == baseline['networks'] and restored['management'] == management
    assert restored['serving_forward'] is None
    r.forget('network', network_id)
    r.evidence('ui', {'checks': ['named dialogs in viewport', 'human-readable node search and keyboard selection',
        'refreshed attachment and resolved endpoint', 'UI detach and delete', 'deleted row absent']})
    r.evidence('runtime', {'phase': 'management-restored', 'networks': restored['networks'],
        'chat_id': restored_chat['id'], 'usage': restored_chat['usage']})
    r.evidence('cleanup', {'network_id': network_id, 'detail_status': 404,
        'attachment_removed': True, 'node_networks_restored': True,
        'fixture_deployment_cleanup': 'runner owns deployment and API key'})
