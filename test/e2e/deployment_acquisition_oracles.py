"""Independent fixture-byte, container-view and launch assertions for acquisition.

The fake engine checks config presence; no assertion claims real weight inference.
"""
import hashlib
import io
import json
from pathlib import Path
import re
import subprocess
import tarfile
from urllib.parse import parse_qs, unquote, urlsplit

from platform_token_read_revoke import check
from model_download_fixture import REVISION
from deployment_acquisition_fixture import IMAGE, SPEC


def same(actual, expected, label):
    dump = lambda v: json.dumps(v, sort_keys=True, separators=(',', ':'), allow_nan=False)
    check(dump(actual) == dump(expected), f'{label}: expected={expected!r}; actual={actual!r}')


def expected_body(data):
    quick = {'dtype': 'auto', 'gpu_memory_utilization': .9 if data['engine'] == 'vllm' else .88,
             'enable_cache_report': True, 'kv_cache_dtype': 'auto'}
    if data['engine'] == 'vllm':
        # Unknown fake-engine image release leaves version-gated per-request
        # metrics to the engine (param_applicability contract), not explicit true.
        quick.update(performance_mode='balanced')
    else:
        quick.update(log_level='info', enable_metrics=True)
    env = {'E2E_ACQUISITION': data['run_id']}
    if data['engine'] == 'sglang':
        env['SGLANG_LOGGING_CONFIG_PATH'] = '/tmp/e2e-fixture-owned-logging.json'
    return {'model_id': data['model_id'], 'served_name': data['served_name'],
        'template_id': data['template']['id'], 'template_spec_source': 'user',
        'template_spec_name': SPEC, 'scenario': 'balanced', 'target_vendor': 'nvidia',
        'node_uuid': data['node']['uuid'], 'engine': data['engine'], 'image': IMAGE,
        'quick_params': quick, 'placement_strategy': 'spread',
        'container_overrides': {'env': env}, 'nnodes': 1, 'replicas': 1,
        'placement_mode': 'auto', 'placement': {'gpus': [0], 'tp': 1},
        'model_source': {'provider': data['provider'], 'storage_id': data['storages'][1]['id']},
        'name': data['name']}


def file_facts(root, files):
    root = Path(root)
    facts = {}
    for name, expected in files.items():
        path = root / name
        check(path.is_file() and not path.is_symlink(), f'expected regular acquired file {name}')
        actual = path.read_bytes()
        same({'bytes': len(actual), 'sha256': hashlib.sha256(actual).hexdigest()},
             {'bytes': len(expected), 'sha256': hashlib.sha256(expected).hexdigest()}, 'acquired file ' + name)
        facts[name] = {'bytes': len(actual), 'sha256': hashlib.sha256(actual).hexdigest()}
    return facts


def container_file(container, path):
    result = subprocess.run(['docker', 'cp', container + ':' + path, '-'], capture_output=True, timeout=30)
    check(result.returncode == 0, 'docker cp failed: ' + result.stderr.decode(errors='replace'))
    with tarfile.open(fileobj=io.BytesIO(result.stdout), mode='r:*') as archive:
        members = archive.getmembers()
        check(len(members) == 1 and members[0].isfile(), 'docker cp did not return exactly one regular file')
        return archive.extractfile(members[0]).read()


def source_transfer(audit, data):
    """Verify bytes actually sent, including exact range fragments and coverage."""
    expected_provider = '/hf/' if data['provider'] == 'huggingface' else '/ms/'
    other_provider = '/ms/' if data['provider'] == 'huggingface' else '/hf/'
    received = {name: [] for name in data['hub'].files}
    records = []
    for record in audit:
        parts = urlsplit(record['target'])
        path = unquote(parts.path)
        query = parse_qs(parts.query)
        name = next((n for n in received if path.endswith('/' + n) or query.get('FilePath') == [n]), None)
        if name is None or record['method'] != 'GET':
            continue
        check(not path.startswith(other_provider), 'unselected provider served model bytes')
        check(path.startswith(expected_provider), 'model bytes escaped chosen provider origin')
        repo_prefix = '/hf/' + data['repo'] + '/' if data['provider'] == 'huggingface' else '/ms/models/' + data['repo'] + '/'
        check(path.startswith(repo_prefix), 'model bytes came from another repository')
        # Fixture metadata may read config via raw; actual model download must
        # include the pinned immutable revision in the resolved file URL.
        check('/' + REVISION + '/' in path, 'model transfer used an unpinned revision')
        content = data['hub'].files[name]
        span = record.get('range')
        start, end = 0, len(content) - 1
        if span:
            match = re.fullmatch(r'bytes=(\d+)-(\d*)', span)
            check(match is not None, 'unexpected transfer Range header')
            start = int(match[1])
            end = min(int(match[2]) if match[2] else end, end)
        expected = content[start:end + 1]
        same(record['status'], 206 if span else 200, 'source transfer HTTP status')
        same(record['bytes_sent'], len(expected), 'source sent byte count')
        same(record['sha256_sent'], hashlib.sha256(expected).hexdigest(), 'source sent bytes hash')
        check(record['complete'] is True and not record['interrupted'], 'source transfer incomplete')
        received[name].append((start, end + 1))
        records.append(record)
    for name, spans in received.items():
        covered = 0
        for start, end in sorted(spans):
            check(start <= covered, f'{name} source byte coverage has a gap at {covered}')
            covered = max(covered, end)
        same(covered, len(data['hub'].files[name]), name + ' complete source byte coverage')
    return records


def execution(container, instance, model_path, data, report):
    inspected = json.loads(subprocess.check_output(['docker', 'inspect', container], text=True))[0]
    same(inspected['Config']['Image'], IMAGE, 'container image ref')
    same(inspected['Image'], report['fake_engine_image_id'], 'container image content ID')
    mounts = [m for m in inspected['Mounts'] if m['Destination'] == '/model']
    check(len(mounts) == 1, 'expected exactly one model mount')
    same({k: mounts[0][k] for k in ('Source', 'Destination', 'RW')},
         {'Source': model_path, 'Destination': '/model', 'RW': False}, 'actual mount source/destination/read-only')
    facts = {}
    for name, expected in data['hub'].files.items():
        actual = container_file(container, '/model/' + name)
        same({'bytes': len(actual), 'sha256': hashlib.sha256(actual).hexdigest()},
             {'bytes': len(expected), 'sha256': hashlib.sha256(expected).hexdigest()}, 'container-view bytes ' + name)
        facts[name] = {'bytes': len(actual), 'sha256': hashlib.sha256(actual).hexdigest()}
    argv = json.loads(container_file(container, '/tmp/fake-engine-argv.json'))
    check(isinstance(argv, list) and all(isinstance(v, str) for v in argv), 'fake engine argv schema differs')
    command = (inspected['Config'].get('Entrypoint') or []) + (inspected['Config'].get('Cmd') or [])
    same(command, [data['engine']] + argv, 'Docker command differs from actual process argv')
    for flag, value in {'--dialect': data['engine'], '--served-model-name': data['served_name'],
                        '--port': str(instance['api_port']),
                        '--tensor-parallel-size' if data['engine'] == 'vllm' else '--tp-size': '1'}.items():
        check(argv.count(flag) == 1, f'actual argv did not contain exactly one {flag}')
        same(argv[argv.index(flag) + 1], value, 'actual argv ' + flag)
    if data['engine'] == 'vllm':
        check((argv[:2] == ['serve', '/model']) or
              (argv.count('--model') == 1 and argv[argv.index('--model') + 1] == '/model'),
              'vLLM did not receive the model mount')
    else:
        check(argv[0] == 'serve', 'SGLang fixture entrypoint override lost serve')
        check(argv.count('--model-path') == 1, 'SGLang lost model-path')
        same(argv[argv.index('--model-path') + 1], '/model', 'SGLang actual model path')
    env = inspected['Config']['Env']
    for key, value in expected_body(data)['container_overrides']['env'].items():
        check(env.count(key + '=' + value) == 1, 'spec environment not honored: ' + key)
    return {'container': container, 'image_id': inspected['Image'], 'mount': mounts[0],
            'container_view_files': facts, 'actual_argv': argv, 'docker_command': command,
            'inference_scope': 'real fake-engine process; no real weight consumption or GPU inference claim'}
