"""One-shot real server/100metadata preflight; no browser, agent or Docker."""
import argparse
from collections import Counter
import hashlib
import json
from pathlib import Path
from urllib.parse import urlsplit
import time

from local_browser_fixture import LocalServer, http
from model_metadata_fixture import CONFIG, INDEX, README, SHA, WEIGHT_BYTES, MetadataTableHub
from model_metadata_table_owner import MetadataTableOwner, MODELS, RULES, require

BINARY_SHA = '1db9e6ab539cad1cb5f6292ac2efffa036a7f4c7d7ec8cb5a089a14274cfcb81'


def sha(data):
    return hashlib.sha256(data).hexdigest()


def write(path, value):
    path.write_text(json.dumps(value, indent=2) + '\n')


def expected_external(rows):
    """Immutable declared bytes/paths, independent of all product responses."""
    out = {}
    encode = lambda v: json.dumps(v, separators=(',', ':')).encode()
    for row in rows:
        if row.source == 'huggingface':
            info = '/hf/api/models/' + row.repo
            body = encode({'sha': SHA, 'siblings': [{'rfilename': 'model.safetensors', 'size': WEIGHT_BYTES}],
                           'createdAt': '2025-01-02T03:04:05Z', 'lastModified': '2025-01-03T04:05:06Z'})
            prefix = '/hf/' + row.repo + '/resolve/main/'
        else:
            info = '/ms/api/v1/models/' + row.repo
            body = encode({'Code': 200, 'Data': {'CreatedTime': 1735787045, 'LastUpdatedTime': 1735877106}})
            prefix = '/ms/models/' + row.repo + '/resolve/master/'
        for target, value in [(info, body), *[(prefix + f, data.encode()) for f, data in
                [('config.json', CONFIG), ('README.md', README), ('model.safetensors.index.json', INDEX)]]]:
            require(target not in out, 'external oracle target duplicated')
            out[target] = {'repo': row.repo, 'source': row.source, 'sha256': sha(value), 'bytes': len(value)}
    return out


def check_external(rows, audit):
    expected = expected_external(rows)
    counts = Counter()
    for request in audit:
        target = request['target']
        require(target in expected, 'foreign/unexpected metadata target: ' + target)
        fact = expected[target]
        require(request['method'] == 'GET' and request['status'] == 200 and request['origin'] == '',
                'metadata method/status/origin differs: ' + target)
        require(request['sha256'] == fact['sha256'] and request['bytes'] == fact['bytes'],
                'metadata response bytes/hash differs: ' + target)
        counts[target] += 1
    require(set(counts) == set(expected) and all(c == 1 for c in counts.values()),
            'metadata exact target coverage/count differs')
    return {'status': 'PASS', 'repositories': len(rows), 'requests': len(audit), 'foreign': 0,
            'per_repository': [{'repo': row.repo, 'source': row.source,
                'targets': [target for target, fact in expected.items() if fact['repo'] == row.repo]}
                for row in rows]}



def check_server_address(base):
    address = urlsplit(base)
    require(address.scheme == 'http' and address.hostname == '127.0.0.1'
            and address.port in range(8800, 8900, 2) and not address.path
            and not address.query and not address.fragment, 'server address escapes LocalServer port policy')
    return address.port


def main():
    parser = argparse.ArgumentParser(__doc__)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--freeze', type=Path, required=True)
    args = parser.parse_args()
    require(not args.output.exists(), 'one-shot output already exists; do not rerun/overwrite')
    freeze = json.loads(args.freeze.read_text())
    for path, digest in freeze['files'].items():
        require(sha(Path(path).read_bytes()) == digest, 'frozen input drift: ' + path)
    require(sha(args.binary.read_bytes()) == BINARY_SHA, 'normal1db binary differs')
    args.output.mkdir(mode=0o700)
    private = args.output / 'private'
    private.mkdir(mode=0o700)
    run_id = args.output.name.lower()
    hub = MetadataTableHub('e2e-' + run_id)
    server = LocalServer(args.binary, private, auto_check_updates=False,
                         env_overrides={'GPUWORKS_USAGE_REPORT': 'off', 'GOMAXPROCS': '2'})
    report = {'status': 'RUNNING', 'run_id': run_id, 'binary_sha256': BINARY_SHA,
              'browser': False, 'agent': False, 'Docker': False, 'coverage_credit': 0,
              'source_rules_created': [], 'cleanup_errors': [], 'primary_error': None,
              'auto_check_updates': False, 'child_usage_report': 'off'}
    token, before, candidates = '', None, {}
    started = time.monotonic()
    def request(method, path, body=None):
        return http(server.base, method, path, token, body)
    def rule_list():
        status, document = request('GET', RULES)
        require(status == 200 and isinstance(document.get('items'), list), 'rule list failed')
        return document['items']
    notes = {source: 'e2e-' + run_id + '-' + source
             for source in ('huggingface', 'modelscope')}
    try:
        server.start()
        report['server_base'] = server.base
        actual_port = check_server_address(server.base)
        require(server.port_lock is not None, 'LocalServer did not retain its port lock')
        report['port_policy'] = {'start': 8800, 'end': 8898, 'step': 2, 'actual': actual_port,
                                 'lock_file': str(server.port_lock.name), 'selection': 'LocalServer sequential lock+bind'}
        code, login = http(server.base, 'POST', '/api/v1/auth/login', body={'username': 'admin', 'password': 'gpuworks-test-password-2026'})
        require(code == 200 and isinstance(login.get('token'), str) and login['token'], 'admin login failed')
        token = login['token']
        before = rule_list()
        require(not any(r.get('note') in notes.values() for r in before), 'run-owned rule note already present')
        code, initial_models = request('GET', MODELS + '?page=1&page_size=100')
        require(code == 200 and initial_models.get('total') == 0,
                'isolated server already has registered models or initial list failed')
        for source, origin in [('huggingface', 'https://huggingface.co'), ('modelscope', 'https://modelscope.cn')]:
            body = {'match': origin, 'endpoints': ['http://127.0.0.1:1/' + run_id + '/' + source],
                    'allow_upstream': False, 'control_plane': True, 'note': notes[source], 'node_requirements': {}}
            status, value = request('POST', RULES, body)
            if isinstance(value, dict) and type(value.get('id')) is int:
                candidates[value['id']] = source
                report['source_rules_created'].append({'id': value['id'], 'source': source, 'body': body})
            require(status == 201 and isinstance(value, dict) and type(value.get('id')) is int,
                    'owned source-rule create failed')
            require(all(value.get(k) == v for k, v in body.items()), 'owned source-rule create wire differs')
        rule_ids = {source: ident for ident, source in candidates.items()}
        report['owner'] = {}
        owner = MetadataTableOwner(request, hub, rule_ids, report['owner'])
        result = owner.run()
        write(args.output / 'owner-report.json', result)
        report['external_audit'] = hub.audit()
        try:
            report['external_proof'] = check_external(hub.rows, report['external_audit'])
        except Exception as error:
            report['external_proof'] = {'status': 'FAIL', 'error': str(error)}
        require(result['status'] == 'PASS', 'owner preflight failed: ' + str(result['primary_error']))
        require(report['external_proof']['status'] == 'PASS', 'metadata external audit failed')
        report['status'] = 'PASS'
    except Exception as error:
        report['status'] = 'FAIL'
        report['primary_error'] = type(error).__name__ + ': ' + str(error)
    finally:
        if token:
            try:
                # Includes POST-timeout commits, but never adopts a foreign note.
                for rule in rule_list():
                    for source, note in notes.items():
                        canonical = 'https://huggingface.co' if source == 'huggingface' else 'https://modelscope.cn'
                        if rule.get('note') == note and rule.get('match') == canonical:
                            candidates[rule['id']] = source
                for ident, source in candidates.items():
                    try:
                        current = {r['id']: r for r in rule_list()}
                        if ident in current:
                            require(current[ident].get('note') == notes[source], 'rule cleanup ownership guard refused')
                            require(current[ident]['match'] == ('https://huggingface.co' if source == 'huggingface' else 'https://modelscope.cn'), 'rule cleanup match differs')
                            require(request('DELETE', RULES + '/' + str(ident))[0] == 204, 'rule DELETE failed')
                        require(ident not in {r['id'] for r in rule_list()}, 'owned source-rule still present')
                    except Exception as error:
                        report['cleanup_errors'].append('rule ' + str(ident) + ': ' + str(error))
                final_rules = rule_list()
                report['final_rules'] = final_rules
                require(before is None or final_rules == before, 'pre-existing/default rules changed')
                status, models = request('GET', MODELS + '?page=1&page_size=100')
                report['final_models_total'] = models.get('total')
                require(status == 200 and models.get('total') == 0, 'models remain after scoped cleanup')
            except Exception as error:
                report['cleanup_errors'].append(str(error))
        try:
            report['hub_stop'] = hub.stop()
            require(report['hub_stop'] == {'listener_stopped': True, 'thread_stopped': True, 'handlers': 0, 'stopped': True}, 'outer hub cleanup incomplete')
        except Exception as error:
            report['cleanup_errors'].append('hub stop: ' + str(error))
        report['server_stopped'] = server.stop()
        report['elapsed_seconds'] = time.monotonic() - started
        report['frozen_sources_unchanged'] = all(sha(Path(p).read_bytes()) == digest for p, digest in freeze['files'].items())
        if report['cleanup_errors'] or not report['server_stopped'] or not report['frozen_sources_unchanged']:
            report['status'] = 'FAIL'
        write(args.output / 'report.json', report)
        print(json.dumps({'status': report['status'], 'output': str(args.output), 'server_base': server.base,
                          'elapsed_seconds': report['elapsed_seconds'], 'primary_error': report['primary_error']}))
    return 0 if report['status'] == 'PASS' else 1


if __name__ == '__main__':
    raise SystemExit(main())
