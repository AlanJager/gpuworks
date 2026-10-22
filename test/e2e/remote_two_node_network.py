"""Two real agents exchange TCP traffic through the management-network UI.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: python3 test/e2e/remote_runtime_acceptance.py --config <private-json> --case two-node-network
Environment/config: remote_runtime_acceptance.py module docstring.
Environment preparation owns temporary-agent onboarding and removal. The product keeps test
history; this case never deletes the built-in management network or its rows.
"""
from __future__ import annotations

from datetime import datetime
import ipaddress
import json
import math
import re
from urllib.parse import urlsplit

from playwright.sync_api import expect

from local_browser_fixture import ROOT
from network_storage_lifecycle import mutate
from platform_token_read_revoke import button, modal_geometry


# Mainline: gpuworks/network/two-node-management-tcp; 两个真实在线agent和管理网地址
# → UI选择单节点校验不发请求，再选择两节点发起1秒双向吞吐
# → 精确POST及同一run的双向RTT/吞吐、刷新回读与键盘详情
# → 两端临时TCP监听器消失，管理网及绑定保持原样；run保留为产品测试历史。
def run_two_node_network(r):
    page = r.page
    t = json.loads((ROOT / 'ui/src/i18n/locales/en/networks.json').read_text())
    first_uuid = r.node['uuid']
    second_uuid = r.config['second_node_uuid']
    assert first_uuid != second_uuid, 'BLOCKED_INFRA: two distinct real agents required'
    second = r.api('GET', '/api/v1/nodes/' + second_uuid)
    assert second['state'] == 'online', 'BLOCKED_INFRA: second agent must be online'
    terminal = r.api('GET', '/api/v1/nodes/' + second_uuid + '/terminal/settings')
    assert terminal['can_open'], 'BLOCKED_INFRA: second host terminal required for listener evidence'
    path = '/api/v1/networks/0'
    baseline = r.api('GET', path)
    assert baseline['builtin'] and baseline['test_supported']
    assert not baseline['latest_test'] or baseline['latest_test']['status'] != 'running', \
        'BLOCKED_INFRA: another management-network test is running'
    uuids = [first_uuid, second_uuid]
    nodes = {node['node_uuid']: node for node in baseline['nodes'] if node['node_uuid'] in uuids}
    assert set(nodes) == set(uuids), 'BLOCKED_INFRA: both agents must report management addresses'
    for node in nodes.values():
        address = ipaddress.ip_address(node['ip'])
        assert not address.is_loopback and not address.is_unspecified, \
            'BLOCKED_INFRA: routable management addresses required'
        assert node['source'] == 'management' and node['interface']
        assert sum(item['node_name'] == node['node_name'] for item in baseline['nodes']) == 1, \
            'BLOCKED_INFRA: selected nodes need unique human-readable names'
    assert nodes[first_uuid]['ip'] != nodes[second_uuid]['ip'], \
        'BLOCKED_INFRA: cross-host evidence needs distinct management addresses'
    baseline_node_networks = {uuid: r.api('GET', '/api/v1/nodes/' + uuid + '/networks')
        for uuid in uuids}
    workload_baseline = {uuid: r.host(_WORKLOAD_SNAPSHOT, node_uuid=uuid) for uuid in uuids}
    assert workload_baseline[first_uuid]['boot_id'] != workload_baseline[second_uuid]['boot_id'], \
        'BLOCKED_INFRA: distinct host boot identities required'
    r.evidence('runtime', {'phase': 'two-node-preflight', 'nodes': list(nodes.values()),
        'agent_versions': {first_uuid: r.node['agent_version'], second_uuid: second['agent_version']},
        'network_id': 0, 'bandwidth_duration_seconds': 1, 'workload_baseline': workload_baseline,
        'scope': 'authenticated host TCP; no GPU, container or RDMA operation'})

    page.goto(r.base + '/networks/0')
    expect(page.get_by_role('heading', name=t['management']['name'], exact=True)).to_be_visible()
    page.get_by_role('tab', name=t['tabs']['test'], exact=True).click()
    expect(page.get_by_role('radio', name=t['test']['patternName']['mesh'], exact=True)).to_be_checked()
    page.get_by_text(t['test']['scopeNodes'], exact=True).click()
    expect(page.get_by_role('radio', name=t['test']['scopeNodes'], exact=True)).to_be_checked()
    picker = page.get_by_role('combobox', name=t['test']['nodes'], exact=True)
    picker.click()
    picker.fill(nodes[first_uuid]['node_name'])
    expect(page.get_by_role('option', name=nodes[first_uuid]['node_name'], exact=True)).to_have_count(1)
    picker.press('ArrowDown')
    picker.press('Enter')
    picker.press('Escape')
    expect(page.get_by_role('switch', name=t['test']['bandwidth'], exact=True)).to_be_checked()
    page.get_by_role('spinbutton', name=t['test']['seconds'], exact=True).fill('1')

    mutations = []
    def observe(request):
        if request.method == 'POST' and urlsplit(request.url).path == path + '/tests':
            mutations.append(request)
    page.on('request', observe)
    try:
        button(page, t['test']['start']).click()
        expect(page.get_by_text(t['test']['nodesRequired'], exact=True)).to_be_visible()
        assert mutations == [], 'single-node validation must not submit a network test'
        r.evidence('ui', {'phase': 'single-node-validation', 'selected_node_uuid': first_uuid,
            'error': t['test']['nodesRequired'], 'mutation_count': 0})

        picker.click()
        picker.fill(nodes[second_uuid]['node_name'])
        expect(page.get_by_role('option', name=nodes[second_uuid]['node_name'], exact=True)).to_have_count(1)
        picker.press('ArrowDown')
        picker.press('Enter')
        picker.press('Escape')
        expect(page.get_by_text(t['test']['nodesRequired'], exact=True)).not_to_be_visible()
        body = {'kind': 'tcp', 'pattern': 'mesh', 'scope': 'nodes',
            'bandwidth': True, 'seconds': 1, 'node_uuids': uuids}
        evidence = []
        started = mutate(page, button(page, t['test']['start']).click, 'POST', path + '/tests',
            body, 202, evidence)
        assert len(mutations) == 1, 'exactly one valid UI action must create exactly one run'
        assert started['id'] > 0 and started['status'] == 'running'
        assert started['node_count'] == 2 and started['pair_count'] == 2
        for item in evidence:
            r.evidence(item.pop('layer'), item)
    finally:
        page.remove_listener('request', observe)

    run_id = started['id']
    r.report.setdefault('retained_product_history', []).append({
        'kind': 'network_test', 'network_id': 0, 'id': run_id,
        'reason': 'Management-network test runs have no public delete API; product retains the latest 10.'})
    r.flush()
    detail = r.poll(lambda: r.api('GET', path),
        lambda value: value['latest_test'] is not None and value['latest_test']['id'] == run_id
        and value['latest_test']['status'] != 'running', timeout=120)
    run = detail['latest_test']
    assert run['status'] == 'done'
    assert {key: run[key] for key in ('kind', 'pattern', 'scope', 'bandwidth')} == {
        'kind': 'tcp', 'pattern': 'mesh', 'scope': 'nodes', 'bandwidth': True}
    assert set(run['node_uuids']) == set(uuids) and len(run['node_uuids']) == 2
    assert run['node_count'] == run['pair_count'] == run['passed_count'] == 2
    assert run['partial_count'] == 0 and len(run['pairs']) == 2
    assert {(pair['source_uuid'], pair['target_uuid']) for pair in run['pairs']} == {
        (first_uuid, second_uuid), (second_uuid, first_uuid)}
    assert datetime.fromisoformat(run['finished_at'].replace('Z', '+00:00')) >= \
        datetime.fromisoformat(run['started_at'].replace('Z', '+00:00'))
    ports = {}
    for pair in run['pairs']:
        assert pair['status'] == 'passed' and pair['passed'] and pair['reachable']
        assert not pair.get('error') and not pair.get('code')
        assert all(math.isfinite(pair[key]) for key in ('rtt_ms_p50', 'rtt_ms_p99', 'gbps'))
        assert 0 < pair['rtt_ms_p50'] <= pair['rtt_ms_p99'] and pair['gbps'] > 0
        target = urlsplit('tcp://' + pair['target'])
        assert ipaddress.ip_address(target.hostname) == ipaddress.ip_address(nodes[pair['target_uuid']]['ip'])
        assert target.port is not None and 0 < target.port <= 65535
        assert pair['source_name'] == nodes[pair['source_uuid']]['node_name']
        assert pair['target_name'] == nodes[pair['target_uuid']]['node_name']
        ports[pair['target_uuid']] = target.port
    r.evidence('runtime', {'phase': 'two-way-tcp-transfer', 'network_id': 0, 'run': run,
        'measurement_contract': 'Agent measures authenticated RTT and throughput from peer-acknowledged received bytes; byte counts are not exposed by the management DTO.'})

    page.reload()
    expect(page.get_by_role('heading', name=t['management']['name'], exact=True)).to_be_visible()
    page.get_by_role('tab', name=t['tabs']['test'], exact=True).click()
    reread = r.api('GET', path)
    assert reread['latest_test'] == run, 'refresh must preserve this exact run and pair measurements'
    for key in ('id', 'name', 'builtin', 'roles', 'type', 'attachments', 'nodes'):
        assert reread[key] == baseline[key], 'TCP test changed network definition or node bindings: ' + key
    r.evidence('persistence', {'path': path, 'run_id': run_id, 'after_browser_reload': reread['latest_test'],
        'network_definition_and_bindings_unchanged': True})

    for pair in run['pairs']:
        # The product formats measurements with Number.toFixed; Python's
        # half-even formatting differs at valid RTT boundaries such as 0.125.
        formatted = page.evaluate('pair => ({gbps:pair.gbps.toFixed(1), '
            'p50:pair.rtt_ms_p50.toFixed(2), p99:pair.rtt_ms_p99.toFixed(2)})', pair)
        label = t['test']['gbps'].replace('{{value}}', formatted['gbps'])
        title = pair['source_name'] + ' → ' + pair['target_name']
        trigger = page.get_by_role('button', name=title + ': ' + label, exact=True)
        expect(trigger).to_be_visible()
        trigger.press('Enter')
        dialog = page.get_by_role('dialog', name=title, exact=True)
        modal_geometry(dialog, dialog.get_by_role('button').filter(
            has_text=re.compile('^' + re.escape(t['actions']['close']) + '$')))
        expect(dialog.get_by_text(t['test']['status']['passed'], exact=True)).to_be_visible()
        expect(dialog.get_by_text(pair['target'], exact=True)).to_be_visible()
        expect(dialog.get_by_text(label, exact=True)).to_be_visible()
        rtt = t['test']['rtt'].replace('{{p50}}', formatted['p50']).replace('{{p99}}', formatted['p99'])
        expect(dialog.get_by_text(rtt, exact=True)).to_be_visible()
        for _ in range(6):
            page.keyboard.press('Tab')
            assert dialog.evaluate('(el) => el.contains(document.activeElement)'), 'modal focus escaped to background'
        dialog.press('Escape')
        expect(dialog).not_to_be_visible()
        expect(trigger).to_be_focused()
    r.evidence('ui', {'phase': 'persisted-pair-details', 'run_id': run_id,
        'checks': ['both directed pair values match API', 'named dialogs and close controls fully in viewport',
            'keyboard open, Tab focus containment and Escape close', 'focus restored to exact pair trigger']})

    listeners = []
    for uuid, port in ports.items():
        observed = r.host('from pathlib import Path\n'
            'rows=[]\n'
            'for filename in ("/proc/net/tcp", "/proc/net/tcp6"):\n'
            '    for line in Path(filename).read_text().splitlines()[1:]:\n'
            '        columns=line.split()\n'
            '        if columns[3]=="0A" and int(columns[1].rsplit(":",1)[1],16)==' + str(port) + ':\n'
            '            rows.append({"table":filename,"local":columns[1],"state":columns[3]})\n'
            'result={"port":' + str(port) + ',"listeners":rows}\n', node_uuid=uuid)
        assert observed == {'port': port, 'listeners': []}, 'agent retained this run\'s TCP listener'
        after = r.api('GET', '/api/v1/nodes/' + uuid + '/networks')
        for key in ('networks', 'management', 'serving_forward'):
            assert after[key] == baseline_node_networks[uuid][key], 'network test modified node routing'
        listeners.append({'node_uuid': uuid, **observed})
    workload_after = {uuid: r.host(_WORKLOAD_SNAPSHOT, node_uuid=uuid) for uuid in uuids}
    for uuid in uuids:
        assert workload_after[uuid]['boot_id'] == workload_baseline[uuid]['boot_id']
        for process in workload_baseline[uuid]['gpu_processes']:
            assert process in workload_after[uuid]['gpu_processes'], 'pre-existing GPU process disappeared or restarted'
    r.evidence('cleanup', {'run_id': run_id, 'listeners': listeners, 'workload_after': workload_after,
        'node_routing_unchanged': True, 'retained_history': True,
        'reason': 'Built-in management network and completed test history are retained by product contract.',
        'second_agent_cleanup': 'Environment preparation owns temporary-agent teardown; this case verifies baseline GPU processes unchanged.'})


# Read-only optional GPU process snapshot, not a GPU functionality claim.
_WORKLOAD_SNAPSHOT = r"""from pathlib import Path
import shutil, subprocess
processes=[]
if shutil.which('nvidia-smi'):
    output=subprocess.check_output(['nvidia-smi','--query-compute-apps=pid','--format=csv,noheader,nounits'], text=True, timeout=15)
    for value in output.splitlines():
        pid=int(value.strip())
        stat=Path('/proc')/str(pid)/'stat'
        # Fields after the parenthesized comm start with field 3 (state).
        fields=stat.read_text().rsplit(')',1)[1].split()
        processes.append({'pid':pid,'start_ticks':int(fields[19])})
result={'boot_id':Path('/proc/sys/kernel/random/boot_id').read_text().strip(),'gpu_processes':processes}
"""
