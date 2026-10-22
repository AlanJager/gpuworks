"""Run-owned real node/storage preparation and fail-closed model-file cleanup.

Only repository bytes are synthetic. Storage roots always stay below the
isolated LocalServer storage_root; the host default model directory is unused.
"""
import hashlib
import os
from pathlib import Path
import subprocess
import time
from urllib.parse import quote

from local_browser_fixture import http
from local_business_runner import checked
from model_download_fixture import ModelDownload

MODELS = '/api/v1/models'
NODES = '/api/v1/nodes'
STORAGES = '/api/v1/storages'


def payload_facts(files):
    return {name: {'bytes': len(value), 'sha256': hashlib.sha256(value).hexdigest()}
            for name, value in files.items()}


def file_facts(root, files):
    result = {}
    for name, value in files.items():
        path = Path(root) / name
        checked(path.is_file() and not path.is_symlink(), f'missing real payload: {path}')
        payload = path.read_bytes()
        checked(payload == value, f'payload bytes differ: {path}')
        result[name] = {'bytes': len(payload), 'sha256': hashlib.sha256(payload).hexdigest()}
    checked(result == payload_facts(files), 'independent payload digest mismatch')
    return result


def artifact_relative(source_id):
    checked(isinstance(source_id, str) and source_id.startswith('e2e-'), 'unowned source identity')
    return 'url/' + source_id.replace('/', '_').strip('_') + '-main'


def wait_online(server, token, uuid, process, timeout=45):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        code, node = http(server.base, 'GET', NODES + '/' + uuid, token)
        if code == 200 and node.get('state') == 'online':
            return node
        checked(process.poll() is None, 'owned agent exited before becoming online')
        # Preparation has no browser event loop yet; this is a bounded state poll.
        process_wait = min(.1, max(0, deadline - time.monotonic()))
        try:
            process.wait(timeout=process_wait)
        except subprocess.TimeoutExpired:
            pass
    raise AssertionError('owned model-file agent did not become online')


def prepare(server, token, run_id, report, holder, agent_bin, artifacts):
    prefix = 'e2e-' + run_id.lower()
    ledger = report.setdefault('model_files_owned', {'storages': [], 'node': None, 'model': None})
    hub = ModelDownload(run_id)
    holder['model_download'] = hub
    hub.start()
    report['external_repository'] = {'url': hub.url, 'files': payload_facts(hub.files)}
    ledger['node'] = {'name': prefix + '-files-node', 'id': None}
    code, created = http(server.base, 'POST', NODES, token,
                         {'name': ledger['node']['name'], 'server_url': server.base})
    node = created.get('node', {}) if isinstance(created, dict) else {}
    credential = created.get('token') if isinstance(created, dict) else None
    if isinstance(credential, str) and credential:
        artifacts.remember(credential)
    if isinstance(node.get('uuid'), str) and node['uuid']:
        ledger['node']['id'] = node['uuid']
    checked(code == 201 and ledger['node']['id'], f'owned node creation HTTP {code}')
    checked(isinstance(credential, str) and credential, 'owned node credential missing')
    state = server.work / 'model-files-agent-state'
    state.mkdir()
    holder['log'] = (server.work / 'agent.log').open('w')
    holder['agent'] = subprocess.Popen([str(agent_bin.resolve()), 'run', '--server', server.base,
        '--token', credential, '--state', str(state)], stdout=holder['log'], stderr=subprocess.STDOUT,
        env={**os.environ, 'GPUWORKS_FAKE_GPU': 'A100x2,24576', 'GPUWORKS_FAKE_GPU_NO_INJECT': '1'})
    report['agent_pid'] = holder['agent'].pid
    wait_online(server, token, node['uuid'], holder['agent'])
    for role in ('target', 'guard'):
        root = server.storage_root / (prefix + '-' + role)
        entry = {'id': None, 'name': prefix + '-' + role, 'root': str(root), 'role': role}
        ledger['storages'].append(entry)
        root.mkdir()
        body = {'name': entry['name'], 'type': 'local', 'is_default': False, 'all_nodes': False,
                'targets': [{'node_uuid': node['uuid']}], 'path': str(root)}
        code, storage = http(server.base, 'POST', STORAGES, token, body)
        if isinstance(storage, dict) and type(storage.get('id')) is int:
            entry['id'] = storage['id']
        checked(code == 201 and entry['id'], f'owned storage creation HTTP {code}: {storage}')
        checked(storage['path'] == str(root) and not storage['is_default'], 'storage root/default drift')
    source_id = prefix + '/transfer'
    ledger['model'] = {'id': None, 'name': prefix + '-model', 'source_id': source_id}
    code, model = http(server.base, 'POST', MODELS + '/register-url', token,
        {'source_url': hub.url, 'source_id': source_id, 'name': ledger['model']['name']})
    if isinstance(model, dict) and type(model.get('id')) is int:
        ledger['model']['id'] = model['id']
    checked(code == 201 and ledger['model']['id'], f'owned URL model registration HTTP {code}')
    checked(model['source'] == 'url' and model['source_id'] == source_id and not model.get('nodes'),
            'registration invented a file replica')
    relative = artifact_relative(source_id)
    for entry in ledger['storages']:
        entry['payload_path'] = str(Path(entry['root']) / relative)
    report['model_file_oracle'] = {'relative_path': relative, 'files': payload_facts(hub.files),
                                 'payload_total': sum(map(len, hub.files.values()))}
    return {'hub': hub, 'model': model, 'node': node, 'storages': ledger['storages'],
            'relative_path': relative, 'source_id': source_id, 'name': ledger['model']['name']}


def cleanup(server, token, report, holder):
    """Keep ownership on every query/error; later stages still execute."""
    ledger = report.get('model_files_owned', {})
    remaining = report.setdefault('remaining_resources', [])
    stages = report.setdefault('model_files_cleanup', [])
    okay = True

    def stage(kind, entry, action):
        nonlocal okay
        try:
            observed = action()
            checked(observed is True, f'{kind} disappearance was not observed')
            stages.append({'kind': kind, 'id': entry.get('id'), 'ok': True})
            return True
        except Exception as error:
            okay = False
            stages.append({'kind': kind, 'id': entry.get('id'), 'ok': False, 'error': str(error)})
            remaining.append({'kind': kind, **entry, 'state': str(error),
                              'reclaim': 'use this exact owned ID/name in isolated server; do not clear unknown resources'})
            return False

    hub = holder.get('model_download')
    if hub:
        hub.release()
    model = ledger.get('model')
    node = ledger.get('node')
    if model:
        def discover_model():
            code, listed = http(server.base, 'GET', MODELS + '?source_id=' + quote(model['source_id'], safe=''), token)
            checked(code == 200, 'owned model discovery failed')
            matches = [m for m in listed['items'] if m['source_id'] == model['source_id']]
            checked(len(matches) <= 1 and len(matches) == listed['total'], 'ambiguous owned model discovery')
            if matches:
                model['id'] = matches[0]['id']
            return True
        stage('model discovery', model, discover_model)
        if model.get('id'):
            for storage in ledger.get('storages', []):
                if not storage.get('id') or not node or not node.get('id'):
                    continue
                def evict(storage=storage):
                    path = f"{MODELS}/{model['id']}/nodes/{node['id']}?storage_id={storage['id']}"
                    code, _ = http(server.base, 'DELETE', path, token)
                    checked(code == 200, f'owned model eviction HTTP {code}')
                    payload = storage.get('payload_path')
                    checked(payload is None or not Path(payload).exists(), 'owned payload remains after agent eviction')
                    return True
                stage('model files', {**storage, 'model_id': model['id'], 'node_uuid': node['id']}, evict)
            def remove_model():
                path = MODELS + '/' + str(model['id'])
                code, _ = http(server.base, 'DELETE', path, token)
                observed, _ = http(server.base, 'GET', path, token)
                checked(code in (204, 404) and observed == 404, 'owned model still exists')
                return True
            if stage('model', model, remove_model):
                if any(s['kind'] == 'model files' and not s['ok'] for s in stages):
                    model['metadata_absent'] = True
                else:
                    ledger['model'] = None
        elif not any(s['kind'] == 'model discovery' and not s['ok'] for s in stages):
            ledger['model'] = None
    for storage in reversed(ledger.get('storages', [])):
        def remove_storage(storage=storage):
            if storage.get('id') is None:
                code, listed = http(server.base, 'GET', STORAGES, token)
                checked(code == 200, 'storage discovery failed')
                matches = [s for s in listed['items'] if s['name'] == storage['name']]
                checked(len(matches) <= 1, 'ambiguous owned storage name')
                if matches:
                    storage['id'] = matches[0]['id']
            if storage.get('id'):
                path = STORAGES + '/' + str(storage['id'])
                code, _ = http(server.base, 'DELETE', path, token)
                observed, _ = http(server.base, 'GET', path, token)
                checked(code in (204, 404) and observed == 404, 'owned storage still exists')
            return True
        if stage('storage', storage, remove_storage):
            storage['reclaimed'] = True
    agent = holder.get('agent')
    if agent is not None:
        def stop_owned_agent():
            if agent.poll() is None:
                agent.terminate()
                try:
                    agent.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    agent.kill()
                    agent.wait(timeout=5)
            return agent.poll() is not None
        stage('agent', {'id': agent.pid}, stop_owned_agent)
    if node:
        def remove_node():
            if not node.get('id'):
                code, listed = http(server.base, 'GET', NODES + '?q=' + quote(node['name']), token)
                checked(code == 200, 'node discovery failed')
                matches = [n for n in listed['items'] if n['name'] == node['name']]
                checked(len(matches) <= 1, 'ambiguous owned node')
                if matches:
                    node['id'] = matches[0]['uuid']
            if node.get('id'):
                path = NODES + '/' + node['id']
                code, response = http(server.base, 'DELETE', path + '?force=true', token)
                observed, _ = http(server.base, 'GET', path, token)
                checked((code == 404 or (code == 200 and response ==
                    {'status': 'deleted', 'agent_uninstalled': False})) and observed == 404,
                    'owned node force-delete wire response or absence invalid')
            return True
        if stage('node', node, remove_node):
            ledger['node'] = None
    if hub:
        def stop_repository():
            observed = hub.stop()
            report['repository_stopped'] = observed
            report['repository_audit'] = hub.audit()
            return observed['stopped'] and observed['handlers'] == 0 and observed['listener_stopped']
        stage('repository-listener', {'id': hub.base}, stop_repository)
    return okay
