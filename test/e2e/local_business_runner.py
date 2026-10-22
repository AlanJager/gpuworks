"""Fail-closed lifecycle for local business browser contracts.

Callbacks own domain resources; this runner owns browser/server/agent shutdown.
Each invocation is independent and never retries. See test/e2e/AGENTS.md.
"""
import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import traceback
import uuid

from local_browser_fixture import Artifacts, LocalServer, ROOT, digest, http
from deployment_replicas import require_dedicated_daemon


def case_parser(description, cases=None):
    parser = argparse.ArgumentParser(description=description)
    parser.add_argument('--server-bin', type=Path, default=ROOT / 'bin/gpuworks')
    parser.add_argument('--agent-bin', type=Path, default=ROOT / 'bin/gpuworks-agent')
    parser.add_argument('--artifact-dir', type=Path)
    parser.add_argument('--locale', choices=('en', 'zh'), default='en')
    parser.add_argument('--mutation', help='Label an actual product mutation; not a failure injector')
    if cases:
        parser.add_argument('--case', choices=cases, required=True)
    return parser


def checked(condition, message):
    if not condition:
        raise AssertionError(message)


def unattributed_console_errors(console_errors, attributed_responses):
    # Chrome also logs an error response as a console resource error ("Failed
    # to load resource: the server responded with a status of NNN"); an echo
    # whose path and status match an already attributed expected response is
    # the same event, not a new error. Matching happens at verdict time on the
    # complete records, so the asynchronous console/response arrival order
    # does not matter. A response that is itself unexpected still fails the
    # case through the HTTP channel, so its echo is never masked here.
    remaining = []
    for error in console_errors:
        match = re.search(r'status of (\d+)', error.get('text', ''))
        if match and any(response['path'] == error.get('path') and response['status'] == int(match[1])
                         for response in attributed_responses):
            continue
        remaining.append(error)
    return remaining


def source_path(path):
    path = (ROOT / path).resolve()
    checked(path.is_relative_to(ROOT) and path.is_file(), 'source path must be a repository file')
    return path


def source_identity(manifest_path, driver, exercise, helpers):
    manifest_path, driver = source_path(manifest_path), source_path(driver)
    manifest = json.loads(manifest_path.read_text())
    browser_file = source_path(manifest['test_file'])
    checked(Path(exercise.__code__.co_filename).resolve() == browser_file
            and exercise.__name__ == manifest['test_function'], 'manifest browser binding differs')
    checked(manifest['driver_file'] == str(driver.relative_to(ROOT)), 'manifest driver binding differs')
    paths = [manifest_path, driver, browser_file, Path(__file__),
             ROOT / 'test/e2e/local_browser_fixture.py',
             ROOT / 'test/e2e/deployment_replicas.py', *map(source_path, helpers)]
    hashes = {str(path.relative_to(ROOT)): digest(path) for path in paths}
    git = lambda *args: subprocess.check_output(['git', *args], cwd=ROOT, text=True).strip()
    return manifest, {'source_sha': git('rev-parse', 'HEAD'),
        'target_sha': git('rev-parse', 'wei/master'),
        'working_tree': 'dirty' if git('status', '--porcelain') else 'clean',
        'coverage_manifest_sha256': digest(manifest_path), 'test_sha256': digest(browser_file),
        'source_hashes': hashes}


def docker_preflight(report, image):
    require_dedicated_daemon(os.environ.get('DOCKER_HOST'), os.environ.get('DOCKER_CONTEXT', ''))
    def command(*args):
        result = subprocess.run(['docker', *args], capture_output=True, text=True, timeout=30)
        checked(result.returncode == 0, f'docker {args[0]} failed: {result.stderr.strip()}')
        return result.stdout
    report['docker_host'] = os.environ['DOCKER_HOST']
    report['docker_daemon'] = json.loads(command('info', '--format',
        '{"ID":{{json .ID}},"Name":{{json .Name}},"Driver":{{json .Driver}}}'))
    report['fake_engine_image_id'] = command('image', 'inspect', '--format', '{{.Id}}', image).strip()
    checked(re.fullmatch(r'sha256:[0-9a-f]{64}', report['fake_engine_image_id']), 'invalid engine image identity')


def final_status(report, required):
    layers = {e.get('layer') for e in report['evidence'] if isinstance(e, dict)}
    passed = (report.get('case_completed') is True and report.get('environment_cleanup_ok') is True
              and report.get('browser_close_ok') is True and report.get('artifact_scan') == 'PASS'
              and report.get('remaining_resources') == [] and set(required) <= layers
              and all(isinstance(s, dict) and s.get('ok') is True for s in report['cleanup_steps'])
              and not report.get('failure') and not report.get('artifact_errors')
              and report['status'] not in ('FAIL_TEST', 'FAIL_PRODUCT', 'BLOCKED_INFRA'))
    if passed:
        return 'PASS'
    return report['status'] if report['status'] in ('FAIL_TEST', 'FAIL_PRODUCT', 'BLOCKED_INFRA') else 'FAIL_TEST'


def run_case(args, *, contract_id, manifest, test_file, prepare, exercise, cleanup,
             helpers=(), docker_required=True, image='gpuworks/fake-engine:test'):
    """test_file names the CLI driver; manifest test_file binds exercise itself.

    prepare(server, token, run_id, report, holder, agent_bin, artifacts) -> data
    exercise(page, context, server, artifacts, report, locale, data, token, holder)
    cleanup(server, token, report, holder) -> bool, recording observed residuals.
    """
    os.umask(0o077)
    manifest, identity = source_identity(manifest, test_file, exercise, helpers)
    checked(manifest['contract_id'] == contract_id, 'manifest contract differs')
    required = manifest['required_evidence']
    checked(isinstance(required, list) and required and 'cleanup' in required, 'missing required evidence contract')
    run_id = datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ') + '-' + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / 'secrets/e2e' / run_id)
    report = {**identity, 'contract': contract_id, 'case': manifest['name'], 'run_id': run_id,
        'status': 'NOT_RUN', 'locale': args.locale, 'timezone': 'UTC',
        'viewport': {'width': 1440, 'height': 900}, 'selection': [contract_id], 'retries': 0,
        'product_mutation': args.mutation, 'step': 'preflight', 'last_step': None,
        'lane': manifest['lane'], 'evidence': [], 'remaining_resources': [], 'cleanup_steps': [],
        'not_covered': manifest.get('missing', []),
        'command': [sys.executable, str(test_file), *sys.argv[1:]],
        'browser_close_ok': False, 'environment_cleanup_ok': False}
    server = LocalServer(args.server_bin, artifacts.private)
    holder = {'agent': None, 'log': None}
    token = None

    def failed(label):
        report['status'] = 'FAIL_TEST'
        message = artifacts.redact(traceback.format_exc())
        report.setdefault('failure', f'{label}: {message}')
        report.setdefault('artifact_errors', []).append({'stage': label, 'error': message})

    def stage(label, action):
        try:
            ok = action() is True
        except Exception:
            failed(label)
            ok = False
        report['cleanup_steps'].append({'stage': label, 'ok': ok})
        return ok

    def save():
        try:
            artifacts.write_json('report.json', report)
            return True
        except Exception:
            failed('report serialization')
            return False

    try:
        report['binary_sha256'] = digest(args.server_bin)
        if docker_required:
            report['agent_binary_sha256'] = digest(args.agent_bin)
            docker_preflight(report, image)
        report['health'] = server.start()
        report['base_url'], report['server_pid'] = server.base, server.process.pid
        code, login = http(server.base, 'POST', '/api/v1/auth/login', body={'username': 'admin', 'password': 'password'})
        checked(code == 200 and isinstance(login.get('token'), str) and login['token'], 'fixture admin login failed')
        token = login['token']
        artifacts.remember(token)
        report['preflight'] = 'PASS'
        report['step'] = 'prepare owned fixture'
        data = prepare(server, token, run_id, report, holder, args.agent_bin, artifacts)
        report['last_step'] = 'fixture ready'
        report['step'] = 'browser contract'
        save()
        from playwright.sync_api import sync_playwright
        with sync_playwright() as play:
            browser = context = page = None
            try:
                browser = play.chromium.launch(channel='chrome', headless=True)
                context = browser.new_context(viewport=report['viewport'], timezone_id='UTC',
                    locale='zh-CN' if args.locale == 'zh' else 'en-US')
                report['browser_version'] = browser.version
                page = context.new_page()
                page.set_default_timeout(10000)
                exercise(page, context, server, artifacts, report, args.locale, data, token, holder)
                checked(set(required) - {'cleanup'} <= {e['layer'] for e in report['evidence']},
                        'browser did not record required evidence layers')
                report['case_completed'] = True
            except Exception:
                report['status'] = 'FAIL_TEST'
                report.setdefault('failed_step', report['step'])
                report.setdefault('failure', artifacts.redact(traceback.format_exc()))
                if page is not None:
                    try:
                        report['failure_screenshot'] = artifacts.capture_failure(page, bool(report.get('sensitive_phase')))
                    except Exception:
                        failed('failure screenshot')
            finally:
                close_ok = True
                if context is not None and artifacts.tracing:
                    close_ok = stage('trace close', lambda: (artifacts.stop_trace(context), True)[1])
                if context is not None:
                    close_ok = stage('context close', lambda: (context.close(), True)[1]) and close_ok
                if browser is not None:
                    close_ok = stage('browser close', lambda: (browser.close(), True)[1]) and close_ok
                report['browser_close_ok'] = close_ok and browser is not None and context is not None
    except Exception:
        report['status'] = 'FAIL_TEST' if report.get('preflight') == 'PASS' else 'BLOCKED_INFRA'
        report.setdefault('failed_step', report['step'])
        report.setdefault('failure', artifacts.redact(traceback.format_exc()))
    finally:
        report['step'] = 'cleanup'
        domain_ok = stage('domain resources', lambda: cleanup(server, token, report, holder))
        if not domain_ok and not report['remaining_resources']:
            report['remaining_resources'].append({'kind': 'domain-cleanup', 'id': run_id,
                'state': 'not confirmed reclaimed', 'reclaim': 'inspect owned_* IDs in this report; reclaim only this run',
                'owned': {key: value for key, value in report.items() if key.startswith('owned_')}})
        agent = holder.get('agent')
        def stop_agent():
            if agent is not None and agent.poll() is None:
                agent.terminate()
                try:
                    agent.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    agent.kill()
                    agent.wait(timeout=5)
            return agent is None or agent.poll() is not None
        report['agent_stopped'] = stage('agent stop', stop_agent)
        log = holder.get('log')
        log_ok = stage('agent log close', lambda: (log.close(), True)[1] if log is not None and not log.closed else True)
        report['server_stopped'] = stage('server stop', lambda: server.stop() is True)
        for kind, ok, process in [('agent', report['agent_stopped'], agent), ('server', report['server_stopped'], server.process)]:
            if not ok:
                report['remaining_resources'].append({'kind': kind, 'id': getattr(process, 'pid', 'unknown'),
                    'state': 'not confirmed stopped', 'reclaim': 'stop only the recorded owned process'})
        image_tag = report.get('owned_image_tag')
        def remove_image():
            result = subprocess.run(['docker', 'image', 'rm', image_tag], capture_output=True, text=True, timeout=30)
            checked(result.returncode == 0, 'run image removal failed: ' + result.stderr)
            after = subprocess.run(['docker', 'image', 'inspect', image_tag], capture_output=True, text=True, timeout=20)
            checked(after.returncode != 0 and re.fullmatch(r'(?:Error response from daemon: |Error: )?No such (?:image|object): '
                + re.escape(image_tag), after.stderr.strip(), re.IGNORECASE), 'image absence not confirmed: ' + after.stderr)
            report['owned_image_tag'] = None
            return True
        image_ok = stage('owned image tag', remove_image) if image_tag else True
        if not image_ok:
            report['remaining_resources'].append({'kind': 'image-tag', 'id': image_tag,
                'state': 'not confirmed absent', 'reclaim': 'docker image rm ' + image_tag})
        report['environment_cleanup_ok'] = (domain_ok and report['agent_stopped'] and log_ok
            and report['server_stopped'] and image_ok and report['browser_close_ok'] and not report['remaining_resources'])
        if report['environment_cleanup_ok']:
            report['evidence'].append({'layer': 'cleanup', 'steps': list(report['cleanup_steps'])})
        for label in ('server.log', 'agent.log'):
            path = artifacts.private / label
            if path.exists():
                try:
                    (artifacts.directory / label).write_text(artifacts.redact(path.read_text()))
                except Exception:
                    failed(label)
        report['private_state'] = ('private/ retains isolated DB and logs for seven days; '
            + ('residuals are listed with scoped reclaim entries' if report['remaining_resources'] else 'owned processes confirmed stopped'))
        save()
        try:
            artifacts.scan()
            report['artifact_scan'] = 'PASS'
        except Exception:
            report['artifact_scan'] = 'FAIL'
            failed('artifact scan')
        report['status'] = final_status(report, required)
        if report['status'] != 'PASS':
            report.setdefault('failed_step', report['step'])
            report.setdefault('failure', 'Completion rejected: cleanup, browser close, scan or required evidence did not all pass')
        if not save():
            report['status'] = 'FAIL_TEST'
    print(json.dumps({'status': report['status'], 'contract': contract_id, 'run_id': run_id,
        'report': str(artifacts.directory / 'report.json')}, ensure_ascii=False))
    return 0 if report['status'] == 'PASS' else 1
