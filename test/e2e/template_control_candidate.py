#!/usr/bin/env python3
"""UNAPPLIED candidate. Never registered as canonical checklist coverage.

After review/apply/build, explicit invocation (one locale per invocation):
python3 test/e2e/template_control_candidate.py --server-bin /path/to/server \
    --upstream-model <verified-built-in-model-id> --locale en
No Docker/agent/GPU required. Does not call legacy management API interception.
"""
import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import subprocess
import traceback
from urllib.parse import urlencode, quote
import uuid

from local_browser_fixture import Artifacts, LocalServer, ROOT, digest, http
from template_catalog_fixture import catalog_package
from template_control_oracles import archive_members, cleanup_owned, finish_stage, require

API = '/api/v1/inference-templates'


def run(args):
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ') + '-' + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / 'secrets/e2e' / run_id)
    report = {'status': 'NOT_RUN', 'candidate_only': True, 'run_id': run_id,
        'locale': args.locale, 'product_mutation': args.mutation, 'owned_models': [],
        'cleanup_steps': [], 'remaining_resources': [], 'evidence': [],
        'canonical_authority': 'test/e2e/coverage/template-edit.json',
        'not_claimed': ['15.2', '12.8', '15.6', 'runtime', 'coverage publication'],
        'step': 'preflight', 'retries': 0}
    source_files = ['template_control_candidate.py', 'template_control_checks.py',
        'template_control_oracles.py', 'template_edit.py', 'template_catalog_fixture.py',
        'local_browser_fixture.py', 'platform_token_read_revoke.py', 'dialog_checks.py']
    paths = [ROOT / 'test/e2e' / path for path in source_files]
    paths += [ROOT / path for path in ['ui/src/pages/InferenceTemplateDetailPage.tsx',
        'ui/src/pages/yamlEditor.tsx', 'test/e2e/coverage/template-edit.json',
        'ui/src/i18n/locales/en/inferenceTemplates.json', 'ui/src/i18n/locales/zh/inferenceTemplates.json']]
    server = LocalServer(args.server_bin, artifacts.private, auto_check_updates=False)
    browser = context = page = None
    token = None
    try:
        report['source_sha256'] = {str(path.relative_to(ROOT)): digest(path) for path in paths}
        report['source_sha'] = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip()
        report['working_tree'] = subprocess.check_output(['git', 'status', '--porcelain'], cwd=ROOT, text=True)
        report['binary_sha256'] = digest(args.server_bin)
        report['health'] = server.start()
        code, login = http(server.base, 'POST', '/api/v1/auth/login', body={'username': 'admin', 'password': 'password'})
        require(code == 200 and isinstance(login.get('token'), str) and login['token'], 'fixture login failed')
        token = login['token']
        artifacts.remember(token)
        report['step'] = 'import deterministic owned package'
        identity = 'e2e-' + run_id.lower() + '/template'
        title, spec = 'e2e-' + run_id, 'e2e-' + run_id + '-spec'
        report.update(fixture_model=identity, fixture_title=title)
        report['owned_models'].append(identity)
        artifacts.write_json('report.json', report)
        payload = catalog_package(identity, spec, title)
        # Real management POST. Package bytes are deterministic, no external
        # repository is contacted; weight_bytes is explicit and never deployed.
        from urllib.request import Request, build_opener, ProxyHandler
        req = Request(server.base + API + '/import', data=payload, method='POST',
            headers={'Authorization': 'Bearer ' + token, 'Content-Type': 'application/gzip'})
        opener = build_opener(ProxyHandler({}))
        with opener.open(req, timeout=20) as response:
            envelope = json.load(response)
            require(response.status == 200 and envelope['models'] == 1, 'fixture package import failed')
        package_digest = envelope['package_digest']
        require(isinstance(package_digest, str) and len(package_digest) == 64
                and all(c in '0123456789abcdef' for c in package_digest), 'invalid imported package digest')
        report['fixture_package_digest'] = package_digest
        report['fixture_package_sha256'] = hashlib.sha256(payload).hexdigest()
        exported = Request(server.base + API + '/export/' + package_digest,
            headers={'Authorization': 'Bearer ' + token})
        with opener.open(exported, timeout=20) as response:
            exported_bytes = response.read()
            require(response.status == 200 and response.headers['ETag'] == '"' + package_digest + '"', 'package export contract drift')
        require(hashlib.sha256(exported_bytes).hexdigest() == package_digest, 'canonical export digest mismatch')
        require(archive_members(exported_bytes) == archive_members(payload), 'canonical export changed fixture file bytes')
        report['step'] = 'browser control contract'
        from playwright.sync_api import sync_playwright
        from template_control_checks import exercise
        with sync_playwright() as play:
            try:
                browser = play.chromium.launch(channel='chrome', headless=True)
                context = browser.new_context(viewport={'width': 1440, 'height': 900}, timezone_id='UTC',
                    locale='zh-CN' if args.locale == 'zh' else 'en-US')
                report['browser_version'] = browser.version
                page = context.new_page()
                page.set_default_timeout(10000)
                exercise(page, context, server, artifacts, report, args.upstream_model)
                require(all(digest(ROOT / path) == sha for path, sha in report['source_sha256'].items()), 'source changed during candidate execution')
                require(digest(args.server_bin) == report['binary_sha256'], 'server binary changed during candidate execution')
                report['case_completed'] = True
            except Exception:
                report.setdefault('failure', artifacts.redact(traceback.format_exc()))
                if page is not None:
                    finish_stage(report, 'failure screenshot',
                        lambda: bool(artifacts.capture_failure(page, not report.get('authenticated', False))))
            finally:
                if context is not None and artifacts.tracing:
                    finish_stage(report, 'trace close', lambda: (artifacts.stop_trace(context), True)[1])
                if context is not None:
                    finish_stage(report, 'context close', lambda: (context.close(), True)[1])
                if browser is not None:
                    finish_stage(report, 'browser close', lambda: (browser.close(), True)[1])
    except Exception:
        report.setdefault('failure', artifacts.redact(traceback.format_exc()))
    finally:
        report['step'] = 'cleanup'
        def fetch(identity):
            return http(server.base, 'GET', API + '/model/' + quote(identity, safe='/'), token=token)
        def remove(identity, spec):
            return http(server.base, 'DELETE', API + '/spec?' + urlencode({'model_id': identity, 'spec': spec}), token=token)[0]
        finish_stage(report, 'owned models', lambda: cleanup_owned(report['owned_models'], fetch, remove,
            report['cleanup_steps'].append))
        report['remaining_resources'] = [{'kind': 'inference-template', 'id': identity,
            'state': 'absence not confirmed', 'reclaim': 'inspect retained isolated SQLite and delete only this registered user model'}
            for identity in report['owned_models']]
        finish_stage(report, 'server stop', lambda: server.stop() is True)
        if server.process is not None and server.process.poll() is None:
            report['remaining_resources'].append({'kind': 'server', 'id': server.process.pid,
                'state': 'running', 'reclaim': 'stop only this recorded candidate server PID'})
        log = artifacts.private / 'server.log'
        if log.exists():
            finish_stage(report, 'server log', lambda: ((artifacts.directory / 'server.log').write_text(artifacts.redact(log.read_text())), True)[1])
        report['private_state'] = 'isolated SQLite retained; observed cleanup steps and residuals above; no shared resources'
        finish_stage(report, 'artifact scan', lambda: (artifacts.scan(), True)[1])
        report['status'] = ('CANDIDATE_PASS' if report.get('case_completed') and not report.get('failure')
            and not report['remaining_resources'] and all(s['ok'] for s in report['cleanup_steps']) else 'FAIL_TEST')
        artifacts.write_json('report.json', report)
    print(json.dumps({'status': report['status'], 'report': str(artifacts.directory / 'report.json')}))
    return report['status'] == 'CANDIDATE_PASS'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--server-bin', type=Path, required=True)
    parser.add_argument('--upstream-model', required=True, help='verified real built-in model, never a user-import relabel')
    parser.add_argument('--locale', choices=('en', 'zh'), required=True)
    parser.add_argument('--artifact-dir', type=Path)
    parser.add_argument('--mutation', help='label only; must identify an actual separately built product mutant')
    return 0 if run(parser.parse_args()) else 1


if __name__ == '__main__':
    raise SystemExit(main())
