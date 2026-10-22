#!/usr/bin/env python3
"""Real local control-plane resource lifecycles, without agent or mount claims.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: python3 test/e2e/network_storage_lifecycle.py [--case network|storage|all]
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import subprocess
import sys
import traceback
from urllib.parse import urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest
from platform_token_read_revoke import button, check, modal_geometry, theme_color


def locale_strings(locale, namespace):
    return json.loads((ROOT / 'ui/src/i18n/locales' / locale / f'{namespace}.json').read_text())


def read(page, base, path, status=200):
    response = page.request.get(base + path)
    check(response.status == status, f'GET {path}: expected {status}, received {response.status}')
    body = response.json()
    check(isinstance(body, dict), f'GET {path}: object envelope required')
    return body


def mutate(page, action, method, path, body, status, evidence):
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == method) as received:
        action()
    response = received.value
    check(response.status == status, f'{method} {path}: expected {status}, received {response.status}')
    check(not urlsplit(response.url).query, f'{method} {path}: unexpected query')
    check(response.request.post_data_json == body if body is not None else response.request.post_data is None,
          f'{method} {path}: request body differs from contract')
    result = response.json() if status != 204 else None
    evidence.append({'layer': 'request', 'method': method, 'path': path, 'body': body,
                     'status': status, 'response': result})
    return result


def persisted(page, base, path, expected, evidence):
    actual = read(page, base, path)
    check(all(actual.get(k) == v for k, v in expected.items()), f'{path}: independent GET differs from saved fields')
    evidence.append({'layer': 'persistence', 'path': path, 'expected': expected, 'response': actual})
    return actual


def mutations_on(page, prefix):
    events = []
    page.on('request', lambda r: events.append({'method': r.method, 'path': urlsplit(r.url).path})
            if urlsplit(r.url).path.startswith(prefix) and r.method in {'POST', 'PUT', 'PATCH', 'DELETE'} else None)
    return events


# Mainline: gpuworks/network/ethernet-resource-lifecycle; 空库 → UI拒绝空名称/角色并取消 → 创建Ethernet serving定义
# → 刷新+独立GET → 编辑名称/CIDR并清空描述/接口 → 刷新回填 → 取消删除不修改 → UI删除、GET404和列表消失。
# 无绑定节点，执行和流量效果N/A；本case不声称网络连通性或agent配置生效。
def test_network_lifecycle(page, server, report, locale):
    t = locale_strings(locale, 'networks')
    evidence = report['evidence']
    prefix = '/api/v1/networks'
    events = mutations_on(page, prefix)
    page.goto(server.base + '/networks')
    expect(page.get_by_role('heading', name=t['title'], exact=True)).to_be_visible()
    baseline = read(page, server.base, prefix)
    check(set(baseline) == {'items', 'roles', 'types', 'type_roles'}, 'network collection envelope differs')
    name = 'e2e-' + report['run_id'] + '-network'
    draft = {'name': name, 'description': 'E2E control-plane definition', 'roles': ['serving'],
             'type': 'host_ethernet', 'cidr': '10.77.0.0/16', 'interface': 'ens*'}
    trigger = button(page, t['actions']['create'])
    trigger.click()
    dialog = page.get_by_role('dialog', name=t['form']['createTitle'], exact=True)
    save = button(dialog, t['actions']['save'])
    modal_geometry(dialog, save)
    name_input = dialog.get_by_label(t['form']['name'], exact=True)
    name_input.click()
    name_input.press('Tab')
    page.keyboard.press('Shift+Tab')
    expect(name_input).to_be_focused()
    theme_color(name_input, 'borderColor', 'primary')
    for _ in range(14):
        page.keyboard.press('Tab')
        check(dialog.evaluate('el => el.contains(document.activeElement)'), 'Tab escaped the modal')
    expect(dialog.get_by_role('radio', name=t['type']['host_ethernet'], exact=True)).to_be_checked()
    for value in ['', '   ']:
        dialog.get_by_label(t['form']['name'], exact=True).fill(value)
        save.click()
        expect(dialog.get_by_text(t['form']['nameRequired'], exact=True)).to_be_visible()
        expect(dialog.get_by_text(t['form']['rolesRequired'], exact=True)).to_be_visible()
        check(events == [], 'invalid network form sent mutation')
    dialog.get_by_label(t['form']['name'], exact=True).fill(name)
    button(dialog, t['actions']['cancel']).click()
    expect(dialog).not_to_be_visible()
    expect(trigger).to_be_focused()
    check(events == [], 'cancel create sent mutation')
    trigger.click()
    for field in ['name', 'description', 'cidr', 'interface']:
        dialog.get_by_label(t['form'][field], exact=True).fill(draft[field])
    dialog.get_by_role('checkbox', name=t['role']['serving'], exact=True).check()
    created = mutate(page, save.click, 'POST', prefix, draft, 201, evidence)
    path = f"{prefix}/{created['id']}"
    expect(page).to_have_url(server.base + f"/networks/{created['id']}")
    expect(page.get_by_role('heading', name=name, exact=True)).to_be_visible()
    persisted(page, server.base, path, draft, evidence)
    page.reload()
    expect(page.get_by_text(draft['description'], exact=True)).to_be_visible()
    expect(page.get_by_text(draft['cidr'], exact=True)).to_be_visible()
    expect(page.get_by_text(t['detail']['noAttachments'], exact=True)).to_be_visible()
    button(page, t['actions']['edit']).click()
    dialog = page.get_by_role('dialog', name=t['form']['editTitle'], exact=True)
    modal_geometry(dialog, button(dialog, t['actions']['save']))
    for field in ['name', 'description', 'cidr', 'interface']:
        expect(dialog.get_by_label(t['form'][field], exact=True)).to_have_value(draft[field])
    dialog.get_by_label(t['form']['name'], exact=True).fill(name + '-cancelled')
    button(dialog, t['actions']['cancel']).click()
    expect(dialog).not_to_be_visible()
    check(len(events) == 1, 'cancel edit sent mutation')
    persisted(page, server.base, path, draft, evidence)
    button(page, t['actions']['edit']).click()
    changed = {**draft, 'name': name + '-edited', 'description': '', 'cidr': '10.78.0.0/16', 'interface': ''}
    for field in ['name', 'description', 'cidr', 'interface']:
        dialog.get_by_label(t['form'][field], exact=True).fill(changed[field])
    mutate(page, button(dialog, t['actions']['save']).click, 'PUT', path, changed, 200, evidence)
    expect(dialog).not_to_be_visible()
    page.reload()
    expect(page.get_by_role('heading', name=changed['name'], exact=True)).to_be_visible()
    persisted(page, server.base, path, changed, evidence)
    button(page, t['actions']['edit']).click()
    for field in ['name', 'description', 'cidr', 'interface']:
        expect(dialog.get_by_label(t['form'][field], exact=True)).to_have_value(changed[field])
    dialog.get_by_label(t['form']['name'], exact=True).press('Escape')
    expect(dialog).not_to_be_visible()
    button(page, t['actions']['delete']).click()
    confirmation = t['actions']['confirmDelete'].replace('{{name}}', changed['name'])
    popup = page.get_by_role('tooltip').filter(has_text=confirmation)
    expect(popup).to_be_in_viewport(ratio=1)
    button(popup, t['actions']['cancel']).click()
    expect(popup).not_to_be_visible()
    check(len(events) == 2, 'cancel delete sent mutation')
    persisted(page, server.base, path, changed, evidence)
    button(page, t['actions']['delete']).click()
    result = mutate(page, button(popup, t['actions']['delete']).click, 'DELETE', path, None, 200, evidence)
    check(result == {'status': 'deleted'}, 'network deletion envelope differs')
    expect(page).to_have_url(server.base + '/networks')
    page.reload()
    expect(page.get_by_role('heading', name=t['title'], exact=True)).to_be_visible()
    expect(page.get_by_role('row').filter(has_text=changed['name'])).to_have_count(0)
    check(read(page, server.base, path, 404)['code'] == 'NETWORK_NOT_FOUND', 'deleted network GET contract differs')
    check(read(page, server.base, prefix) == baseline, 'network deletion did not restore baseline')
    check(events == [{'method': 'POST', 'path': prefix}, {'method': 'PUT', 'path': path}, {'method': 'DELETE', 'path': path}], 'unexpected network mutation')
    evidence.extend([{'layer': 'ui', 'checks': ['empty/whitespace/roles validation', 'dialog viewport', 'keyboard focus border=theme.primary', 'Tab focus trap', 'cancel/create/edit/delete', 'refresh fields', 'clear optional fields']},
                     {'layer': 'cleanup', 'detail_status': 404, 'list_restored': True, 'mutations': events}])


# Mainline: gpuworks/storage/local-resource-lifecycle; 空库 → UI校验名称与绝对路径并取消 → UI登记local/all_nodes
# → GET/刷新回填 → UI编辑名称/目录 → GET/刷新 → UI停用再启用且布尔值持久化 → 取消删除 → UI删除、GET404/列表消失。
# 没有agent；只证明存储定义及启停期望值，不证明节点目录创建、挂载、容量或模型读写。
def test_storage_lifecycle(page, server, report, locale):
    t = locale_strings(locale, 'storage')
    evidence = report['evidence']
    prefix = '/api/v1/storages'
    events = mutations_on(page, prefix)
    page.goto(server.base + '/storage')
    expect(page.get_by_role('heading', name=t['page']['title'], exact=True)).to_be_visible()
    baseline = read(page, server.base, prefix)
    check(set(baseline) == {'items', 'total'} and baseline['total'] == len(baseline['items']), 'storage collection envelope differs')
    name = 'e2e-' + report['run_id'] + '-local'
    draft = {'name': name, 'type': 'local', 'path': str(server.storage_root / name), 'all_nodes': True, 'targets': []}
    # The installation seeds its default storage, so the header action is unique.
    trigger = page.get_by_role('button').filter(has=page.get_by_text(t['page']['add'], exact=True))
    trigger.click()
    dialog = page.get_by_role('dialog', name=t['form']['addTitle'], exact=True)
    save = button(dialog, t['form']['save'])
    modal_geometry(dialog, save)
    name_input = dialog.get_by_label(t['form']['name'], exact=True)
    name_input.click()
    name_input.press('Tab')
    page.keyboard.press('Shift+Tab')
    expect(name_input).to_be_focused()
    theme_color(name_input, 'borderColor', 'primary')
    for _ in range(14):
        page.keyboard.press('Tab')
        check(dialog.evaluate('el => el.contains(document.activeElement)'), 'Tab escaped the modal')
    expect(dialog.get_by_role('radio', name=locale_strings(locale, 'common')['storageType']['local'], exact=True)).to_be_checked()
    expect(dialog.get_by_role('radio', name=t['form']['scope']['all'], exact=True)).to_be_checked()
    for value in ['', '   ', 'INVALID_NAME']:
        dialog.get_by_label(t['form']['name'], exact=True).fill(value)
        save.click()
        expect(dialog.get_by_text(t['form']['error']['name'], exact=True)).to_be_visible()
        check(events == [], 'invalid storage name sent mutation')
    dialog.get_by_label(t['form']['name'], exact=True).fill(name)
    dialog.get_by_label(t['form']['path'], exact=True).fill('relative/path')
    save.click()
    expect(dialog.get_by_text(t['form']['error']['pathAbsolute'], exact=True)).to_be_visible()
    check(events == [], 'relative storage path sent mutation')
    button(dialog, t['form']['cancel']).click()
    expect(dialog).not_to_be_visible()
    expect(trigger).to_be_focused()
    check(events == [], 'cancel storage create sent mutation')
    trigger.click()
    dialog.get_by_label(t['form']['name'], exact=True).fill(' ' + name + ' ')
    dialog.get_by_label(t['form']['path'], exact=True).fill(' ' + draft['path'] + ' ')
    created = mutate(page, save.click, 'POST', prefix, draft, 201, evidence)
    path = f"{prefix}/{created['id']}"
    expect(page).to_have_url(server.base + f"/storage/{created['id']}")
    persisted(page, server.base, path, {**draft, 'enabled': True}, evidence)
    page.reload()
    expect(page.get_by_text(draft['path'], exact=True)).to_be_visible()
    button(page, t['actions']['edit']).click()
    dialog = page.get_by_role('dialog', name=t['form']['editTitle'].replace('{{name}}', name), exact=True)
    modal_geometry(dialog, button(dialog, t['form']['save']))
    expect(dialog.get_by_label(t['form']['name'], exact=True)).to_have_value(name)
    expect(dialog.get_by_label(t['form']['path'], exact=True)).to_have_value(draft['path'])
    dialog.get_by_label(t['form']['path'], exact=True).fill(draft['path'] + '-cancelled')
    page.keyboard.press('Escape')
    expect(dialog).not_to_be_visible()
    expect(button(page, t['actions']['edit'])).to_be_focused()
    check(len(events) == 1, 'cancel storage edit sent mutation')
    persisted(page, server.base, path, draft, evidence)
    button(page, t['actions']['edit']).click()
    changed = {'name': name + '-edited', 'path': draft['path'] + '-edited', 'all_nodes': True, 'targets': []}
    dialog.get_by_label(t['form']['name'], exact=True).fill(changed['name'])
    dialog.get_by_label(t['form']['path'], exact=True).fill(changed['path'])
    # The edit form sends only the fields that differ from what it loaded; scope is unchanged.
    mutate(page, button(dialog, t['form']['save']).click, 'PATCH', path,
           {'name': changed['name'], 'path': changed['path']}, 200, evidence)
    expect(dialog).not_to_be_visible()
    page.reload()
    expect(page.get_by_text(changed['path'], exact=True)).to_be_visible()
    persisted(page, server.base, path, {**changed, 'type': 'local'}, evidence)
    switch = page.get_by_role('switch', name=t['actions']['enabledSwitch'].replace('{{name}}', changed['name']), exact=True)
    for enabled in [False, True]:
        mutate(page, switch.click, 'PATCH', path, {'enabled': enabled}, 200, evidence)
        page.reload()
        expect(switch).to_have_attribute('aria-checked', str(enabled).lower())
        persisted(page, server.base, path, {**changed, 'type': 'local', 'enabled': enabled}, evidence)
    page.goto(server.base + '/storage')
    row = page.get_by_role('row').filter(has_text=changed['name'])
    expect(row).to_contain_text(changed['path'])
    delete = button(row, t['actions']['delete'])
    delete.click()
    confirmation = t['actions']['deleteConfirm'].replace('{{name}}', changed['name'])
    popup = page.get_by_role('tooltip').filter(has_text=confirmation)
    expect(popup).to_be_in_viewport(ratio=1)
    expect(popup.get_by_text(t['actions']['deleteHelp'], exact=True)).to_be_visible()
    button(popup, t['actions']['cancel']).click()
    expect(popup).not_to_be_visible()
    check(len(events) == 4, 'cancel storage delete sent mutation')
    persisted(page, server.base, path, changed, evidence)
    delete.click()
    mutate(page, button(popup, t['actions']['delete']).click, 'DELETE', path, None, 204, evidence)
    expect(row).to_have_count(0)
    page.reload()
    expect(page.get_by_role('heading', name=t['page']['title'], exact=True)).to_be_visible()
    expect(row).to_have_count(0)
    check(read(page, server.base, path, 404) == {'error': 'not found'}, 'deleted storage GET contract differs')
    check(read(page, server.base, prefix) == baseline, 'storage deletion did not restore baseline')
    check(events == [{'method': 'POST', 'path': prefix}] + [{'method': 'PATCH', 'path': path}] * 3 + [{'method': 'DELETE', 'path': path}], 'unexpected storage mutation')
    evidence.extend([{'layer': 'ui', 'checks': ['empty/whitespace/invalid name', 'relative path validation', 'trim', 'dialog viewport', 'keyboard focus border=theme.primary', 'Tab focus trap', 'cancel/create/edit/delete', 'refresh fields', 'enabled false/true']},
                     {'layer': 'cleanup', 'detail_status': 404, 'list_restored': True, 'mutations': events}])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--case', choices=['network', 'storage', 'all'], default='all')
    parser.add_argument('--locale', choices=['en', 'zh'], default='en')
    parser.add_argument('--server-bin', type=Path, default=ROOT / 'bin/gpuworks')
    parser.add_argument('--artifact-dir', type=Path)
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ').lower() + '-' + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / 'secrets/e2e' / run_id)
    server = LocalServer(args.server_bin, artifacts.private)
    manifest_path = ROOT / 'test/e2e/coverage/network-storage-lifecycle.json'
    manifest = json.loads(manifest_path.read_text())
    contracts = {
        'network': ('gpuworks/network/ethernet-resource-lifecycle', test_network_lifecycle),
        'storage': ('gpuworks/storage/local-resource-lifecycle', test_storage_lifecycle),
    }
    selected_keys = list(contracts) if args.case == 'all' else [args.case]
    report = {'run_id': run_id, 'status': 'NOT_RUN', 'source_sha': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip(),
              'working_tree': 'dirty' if subprocess.check_output(['git', 'status', '--porcelain'], cwd=ROOT) else 'clean',
              'lane': 'local-real-server-sqlite', 'identity': 'admin', 'locale': args.locale, 'timezone': 'UTC',
              'viewport': {'width': 1440, 'height': 900}, 'retries': 0, 'cases': [],
              'selection': [contracts[key][0] for key in selected_keys],
              'test_sha256': digest(Path(__file__)), 'fixture_sha256': digest(ROOT / 'test/e2e/local_browser_fixture.py'),
              'helper_sha256': digest(ROOT / 'test/e2e/platform_token_read_revoke.py'), 'manifest_sha256': digest(manifest_path),
              'command': ['python3', 'test/e2e/network_storage_lifecycle.py', *sys.argv[1:]]}
    try:
        check(manifest['test_file'] == 'test/e2e/network_storage_lifecycle.py', 'manifest test file mismatch')
        check(len(manifest['cases']) == len(contracts), 'manifest must contain both resource contracts')
        indexed = {case['case_id']: case for case in manifest['cases']}
        check(set(indexed) == set(contracts), 'manifest cases differ from fixed runner selection')
        for key, (contract, function) in contracts.items():
            check(indexed[key]['contract_id'] == contract and indexed[key]['test_function'] == function.__name__, 'manifest binding differs from fixed runner contract')
            check(set(indexed[key]['required_evidence']) == {'ui', 'request', 'persistence', 'cleanup'}, 'required evidence contract was changed')
        selected = [indexed[key] for key in selected_keys]
        check(bool(selected), 'empty selection is forbidden')
        report['binary_sha256'] = digest(args.server_bin)
        report['health'] = server.start()
        report['base_url'], report['owned_server_pid'] = server.base, server.process.pid
        report['server_environment'] = server.environment
        with sync_playwright() as play:
            browser = play.chromium.launch(channel='chrome', headless=True)
            report['browser_version'] = browser.version
            for case in selected:
                result = {'contract_id': case['contract_id'], 'run_id': run_id, 'status': 'NOT_RUN', 'evidence': []}
                report['cases'].append(result)
                context = browser.new_context(viewport=report['viewport'], locale='zh-CN' if args.locale == 'zh' else 'en-US', timezone_id='UTC')
                page = context.new_page()
                page.set_default_timeout(10000)
                try:
                    auth = locale_strings(args.locale, 'auth')
                    page.goto(server.base + '/dashboard')
                    page.get_by_role('textbox', name=auth['username'], exact=True).fill('admin')
                    page.get_by_label(auth['password'], exact=True).fill('password')
                    with page.expect_response(lambda r: urlsplit(r.url).path == '/api/v1/auth/login' and r.request.method == 'POST') as login:
                        button(page, auth['submit']).click()
                    check(login.value.status == 200, 'browser login failed')
                    for cookie in context.cookies():
                        artifacts.remember(cookie['value'])
                    check(read(page, server.base, '/api/v1/auth/me')['username'] == 'admin', 'session identity differs')
                    report['preflight'] = 'PASS'
                    errors, bad_http, console_errors = [], [], []
                    page.on('pageerror', lambda error: errors.append(str(error)))
                    page.on('response', lambda r: bad_http.append({'path': urlsplit(r.url).path, 'status': r.status}) if r.status >= 400 else None)
                    page.on('console', lambda message: console_errors.append(message.text) if message.type == 'error' else None)
                    artifacts.start_trace(context)
                    contracts[case['case_id']][1](page, server, result, args.locale)
                    check(not errors and not bad_http and not console_errors, f'unexpected browser errors: {errors}, {bad_http}, {console_errors}')
                    check(page.evaluate('document.documentElement.scrollWidth <= innerWidth'), 'page horizontal overflow')
                    check(set(case['required_evidence']) <= {e['layer'] for e in result['evidence']}, 'missing required evidence')
                    result['browser_errors'] = {'page': errors, 'http': bad_http, 'console': console_errors}
                    page.screenshot(path=artifacts.directory / f"{case['case_id']}-complete.png")
                    result['status'] = 'PASS'
                except Exception:
                    result['status'], result['failure'] = 'FAIL_TEST', traceback.format_exc()
                    artifacts.write_json('report.json', report)
                    if not any(secret in page.content() for secret in artifacts.secrets if len(secret) > 8):
                        page.screenshot(path=artifacts.directory / f"{case['case_id']}-failure.png")
                finally:
                    artifacts.stop_trace(context)
                    context.close()
            browser.close()
        report['status'] = 'PASS' if len(report['cases']) == len(selected_keys) and bool(report['cases']) and all(c['status'] == 'PASS' for c in report['cases']) else 'FAIL_TEST'
    except Exception:
        report['status'] = 'FAIL_TEST' if report.get('preflight') == 'PASS' else 'BLOCKED_INFRA'
        report['failure'] = traceback.format_exc()
    finally:
        report['server_stopped'] = server.stop()
        if not report['server_stopped']:
            report['status'] = 'FAIL_TEST'
        log = artifacts.private / 'server.log'
        if log.exists():
            (artifacts.directory / 'server.log').write_text(artifacts.redact(log.read_text()))
        report['private_state'] = 'Isolated SQLite retained for 7 days; no process kept. Remove only this run directory after diagnosis.'
        artifacts.write_json('report.json', report)
        try:
            artifacts.scan()
        except Exception:
            report['status'] = 'FAIL_TEST'
            report['artifact_error'] = 'Credential scan rejected artifacts; do not upload this private run directory'
            artifacts.write_json('report.json', report)
    print(json.dumps({'status': report['status'], 'report': str(artifacts.directory / 'report.json')}))
    return 0 if report['status'] == 'PASS' else 1


if __name__ == '__main__':
    raise SystemExit(main())
