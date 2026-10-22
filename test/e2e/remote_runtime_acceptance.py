#!/usr/bin/env python3
"""Explicit remote runtime acceptance; credentials and target IDs from private JSON.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: python3 test/e2e/remote_runtime_acceptance.py --config <private-json> --case storage
Cases: storage | inference | network | two-node-network | all.
"all" selects the three single-node cases; two-node-network is explicit only.
Fixture tests: python3 -m unittest discover -s test/e2e -p 'test_*fixture.py'

Required config (gitignored secrets/, chmod 0600; placeholders are not defaults):
  {"base_url": "http://<explicit-server>:<port>", "username": "<admin>",
   "password": "<private-password>", "node_uuid": "<explicit-online-node>",
   "storage_source_id": 0, "model_id": 0,
   "model_source_id": "<cached-chat-model-repository>", "model_revision": "<exact-revision>",
   "image": "<locally-cached-vllm-image>", "gpu_index": 0}
Replace IDs with the selected environment's IDs. storage needs the first five fields;
two-node-network needs base_url, username, password, node_uuid and second_node_uuid.
No default endpoint, retries, mock server or automatic deletion on failure.

Prerequisites: explicit online agent with host terminal and Python 3; storage also
requires root NFS mounting. GPU cases require NVIDIA GPU, Docker, cached image/model
and an idle selected card. The fixture uses single-card vLLM, 2048 context and 15%
memory, not arbitrary model sizes or runtimes. network requires no other deployment
on the selected control-plane node. Two-node traffic is unthrottled and can compete
with existing management traffic; it is not a performance qualification.

For local UI against a remote backend (does not upgrade the remote installation):
  node test/e2e/remote_ui_proxy.mjs <private-json> 8802
Keep the proxy running with ui/node_modules installed. Add explicit ui_base_url and
ui_source_sha to a separate private config; keep base_url pointing at the backend.
Exclude loopback from shell proxies via NO_PROXY/no_proxy. Chrome treats only this
run's explicit HTTP origin as secure for real clipboard interaction. Record this
lane separately from the remote embedded UI; requests retain Host/Origin for CSRF.

Temporary second-agent preparation/removal is outside this browser runner. Use the
server's /dl/ version in a runId directory, driver none, and cordon its node before
connecting. Check storage desired state, isolate state paths and the Docker socket,
and record GPU-process/mount baselines; the default Docker driver is unsafe for a
shared host. Afterward stop only that temporary service, remove its node/directory,
and attach baseline/absence evidence to this run. Failed runs preserve logs and the
owned ledger for diagnosis; there is no global cleanup. Product network test history
has no DELETE API and remains recorded as retained_product_history.

Shared models, images, storage and node caches are not owned fixtures. Starting a
model on nfs_cache can leave a persistent cache; record before/after inventory, never
evict the shared cache or delete the authoritative model to make cleanup look empty.
Specific Mainline/steps/assertions live in remote_{storage,inference,network}_runtime.py
and remote_two_node_network.py; coverage manifests retain missing dimensions.
"""
import argparse
import json
from pathlib import Path
import traceback

from playwright.sync_api import sync_playwright
from local_browser_fixture import ROOT, digest
from remote_runtime_fixture import RemoteSession
from remote_gpu_fixture import prepare_gpu, public_chat, cleanup_gpu, verify_gpu


def run(browser, config, case):
    r = RemoteSession(browser, config)
    if case == 'storage' and not config.get('ui_base_url'):
        r.report['lane'] = 'remote-real-agent-nfs'
    if case == 'two-node-network' and not config.get('ui_base_url'):
        r.report['lane'] = 'remote-real-agent-two-node-tcp'
    print('Artifacts:', r.artifacts.directory, flush=True)
    try:
        r.login()
        r.artifacts.start_trace(r.context)
        if case == 'storage':
            from remote_storage_runtime import run_storage
            run_storage(r)
        elif case == 'two-node-network':
            from remote_two_node_network import run_two_node_network
            run_two_node_network(r)
        else:
            prepare_gpu(r)
            r.public_chat = lambda key, stream=False: public_chat(r, key, stream)
            if case == 'inference':
                from remote_inference_runtime import run_inference
                run_inference(r)
            else:
                from remote_network_runtime import run_network
                r.artifacts.stop_trace(r.context)
                guard = r.api('POST', '/api/v1/apikeys', {'name': 'e2e-' + r.run_id + '-network'}, expected=201)
                key = guard['key']
                r.artifacts.remember(key)
                prefix = key[:12]
                r.own('inference_key', prefix)
                r.chat = lambda: public_chat(r, key)
                r.artifacts.start_trace(r.context)
                run_network(r)
                result = r.api('DELETE', '/api/v1/apikeys/' + prefix)
                assert result == {'status': 'deleted'}
                r.forget('inference_key', prefix)
            verify_gpu(r)
            cleanup_gpu(r)
        assert not r.errors, 'browser page errors: ' + str(r.errors)
        assert not r.owned, 'owned resources remain'
        manifest_name = 'remote-two-node-network.json' if case == 'two-node-network' else f'remote-{case}-runtime.json'
        manifest_path = ROOT / 'test/e2e/coverage' / manifest_name
        manifest = json.loads(manifest_path.read_text())
        expected = set(manifest['cases'][0]['required_evidence'])
        assert expected <= {e['layer'] for e in r.evidences}, 'required evidence missing'
        assert r.report['source_files'] == {p.name: digest(p) for p in (ROOT / 'test/e2e').glob('remote_*.py')}, 'test source changed during execution'
        r.report.update(status='VERIFYING_ARTIFACTS', case=case, manifest_sha256=digest(manifest_path),
            test_sha256=digest(ROOT / manifest['test_file']))
        r.report['cases'] = [{'contract_id': manifest['cases'][0]['contract_id'], 'status': 'VERIFYING_ARTIFACTS'}]
    except Exception:
        error = r.artifacts.redact(traceback.format_exc())
        r.report.update(status='BLOCKED_INFRA' if 'BLOCKED_INFRA:' in error else 'FAIL', case=case, error=error)
        # Preserve owned inventory for diagnosis. No screenshot while secret may be displayed.
        print(r.report['error'], flush=True)
        raise
    finally:
        try:
            r.close()
            r.artifacts.scan()
        except Exception:
            r.report.update(status='FAIL_ARTIFACT', error=r.artifacts.redact(traceback.format_exc()))
            r.report['cases'] = []
            r.flush()
            raise
    r.report['status'] = 'PASS'
    r.report['cases'][0]['status'] = 'PASS'
    r.flush()
    print(case + ': PASS', flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--config', type=Path, required=True)
    parser.add_argument('--case', choices=['storage', 'inference', 'network', 'two-node-network', 'all'], required=True)
    args = parser.parse_args()
    config = json.loads(args.config.read_text())
    for key in ('base_url', 'username', 'password', 'node_uuid'):
        assert config.get(key), 'missing explicit config: ' + key
    if args.case in ('storage', 'inference', 'network', 'all'):
        assert config.get('storage_source_id'), 'missing explicit storage_source_id'
    if args.case == 'two-node-network':
        assert config.get('second_node_uuid'), 'missing explicit second_node_uuid'
        assert config['second_node_uuid'] != config['node_uuid'], 'two distinct real nodes required'
    if args.case in ('inference', 'network', 'all'):
        for key in ('model_id', 'model_source_id', 'model_revision', 'image', 'gpu_index'):
            assert key in config, 'missing explicit GPU config: ' + key
    with sync_playwright() as play:
        browser = play.chromium.launch(channel='chrome', headless=True,
            args=['--unsafely-treat-insecure-origin-as-secure=' + config['base_url']])
        try:
            for case in (['storage', 'inference', 'network'] if args.case == 'all' else [args.case]):
                run(browser, config, case)
        finally:
            browser.close()


if __name__ == '__main__':
    main()
