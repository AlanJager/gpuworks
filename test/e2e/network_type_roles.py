#!/usr/bin/env python3
"""Network type/role and conditional-field contracts on an isolated real server.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: python3 test/e2e/network_type_roles.py --locale en|zh [--case CASE]
Build first with make build. No agent/GPU required; no communication claim.
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import itertools
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
from network_storage_lifecycle import locale_strings, mutate, mutations_on, persisted, read
from platform_token_read_revoke import button, check, modal_geometry

PREFIX = '/api/v1/networks'
ROLES = ['storage_sync', 'compute', 'serving']
TYPES = ['host_ethernet', 'host_rdma', 'accelerator_rdma']
TYPE_ROLES = {'host_ethernet': ROLES, 'host_rdma': ROLES, 'accelerator_rdma': ['compute']}
# Fixed contract denominator: the API dictionary must match, never shrink it dynamically.
COMBINATIONS = {
    kind + '--' + '-'.join(roles): (kind, list(roles))
    for kind in TYPES
    for size in range(1, len(TYPE_ROLES[kind]) + 1)
    for roles in itertools.combinations(TYPE_ROLES[kind], size)
}
TRANSITIONS = {f'to-{kind}-and-back': kind for kind in TYPES[1:]}
CASES = list(COMBINATIONS) + list(TRANSITIONS)


def form(page, t, editing=False):
    return page.get_by_role('dialog', name=t['form']['editTitle' if editing else 'createTitle'], exact=True)


def type_control(dialog, t, kind):
    return dialog.get_by_role('radio', name=t['type'][kind], exact=True)


def choose_type(dialog, t, kind):
    # AntD button radios visually expose their label; native input has no box.
    dialog.get_by_text(t['type'][kind], exact=True).click()
    expect(type_control(dialog, t, kind)).to_be_checked()


def role_control(dialog, t, role):
    return dialog.get_by_role('checkbox', name=t['role'][role], exact=True)


def open_create(page, server, t):
    page.goto(server.base + '/networks')
    expect(page.get_by_role('heading', name=t['title'], exact=True)).to_be_visible()
    button(page, t['actions']['create']).click()
    dialog = form(page, t)
    modal_geometry(dialog, button(dialog, t['actions']['save']))
    return dialog


def delete_owned(page, server, t, path, name, baseline, evidence):
    button(page, t['actions']['delete']).click()
    popup = page.get_by_role('tooltip').filter(has_text=t['actions']['confirmDelete'].replace('{{name}}', name))
    expect(popup).to_be_in_viewport(ratio=1)
    result = mutate(page, button(popup, t['actions']['delete']).click, 'DELETE', path, None, 200, evidence)
    check(result == {'status': 'deleted'}, 'delete envelope differs')
    expect(page).to_have_url(server.base + '/networks')
    page.reload()
    expect(page.get_by_role('heading', name=t['title'], exact=True)).to_be_visible()
    expect(page.get_by_role('row').filter(has_text=name)).to_have_count(0)
    check(read(page, server.base, path, 404)['code'] == 'NETWORK_NOT_FOUND', 'deleted resource remains readable')
    check(read(page, server.base, PREFIX) == baseline, 'scoped deletion did not restore baseline')
    evidence.append({'layer': 'cleanup', 'path': path, 'detail_status': 404, 'list_restored': True})


# Mainline: gpuworks/network/type-role/<type--roles>; 空库字典固定三类型/三角色 → UI逐项选择指定非空角色组合
# → 精确POST+独立GET → 刷新打开编辑确认类型/角色回填 → UI删除+GET404+列表恢复；仅定义，无网络运行时声明。
def test_type_role_combination(page, server, result, locale, key, baseline):
    t, evidence = locale_strings(locale, 'networks'), result['evidence']
    kind, roles = COMBINATIONS[key]
    result['mainline'] = f'空库 → UI选择 {kind} / {roles} 创建 → GET及刷新编辑回填 → UI删除和GET404/列表恢复'
    events = mutations_on(page, PREFIX)
    dialog = open_create(page, server, t)
    expect(type_control(dialog, t, TYPES[0])).to_be_checked()
    expect(dialog.get_by_role('radio')).to_have_count(3)
    expect(dialog.get_by_role('checkbox')).to_have_count(3)
    for role in ROLES:
        expect(role_control(dialog, t, role)).not_to_be_checked()
    choose_type(dialog, t, kind)
    expect(type_control(dialog, t, kind)).to_be_checked()
    for role in ROLES:
        control = role_control(dialog, t, role)
        if role in TYPE_ROLES[kind]:
            expect(control).to_be_enabled()
        else:
            expect(control).to_be_disabled()
        if role in roles:
            control.check()
            expect(control).to_be_checked()
    name = 'e2e-' + result['run_id'] + '-' + str(CASES.index(key))
    dialog.get_by_label(t['form']['name'], exact=True).fill(name)
    if kind != 'host_ethernet':
        expect(dialog.get_by_label(t['form']['interface'], exact=True)).to_have_count(0)
        expect(dialog.get_by_label(t['form']['bootstrapCidr'], exact=True)).not_to_be_visible()
        expect(dialog.get_by_role('button').filter(has_text=t['form']['advanced'])).to_have_attribute('aria-expanded', 'false')
    body = {'name': name, 'description': '', 'roles': roles, 'type': kind, 'cidr': '', 'interface': ''}
    created = mutate(page, button(dialog, t['actions']['save']).click, 'POST', PREFIX, body, 201, evidence)
    path = PREFIX + '/' + str(created['id'])
    expect(page).to_have_url(server.base + '/networks/' + str(created['id']))
    actual = persisted(page, server.base, path, body, evidence)
    check(actual['type_roles'] == TYPE_ROLES, 'detail type_roles differs')
    page.reload()
    expect(page.get_by_role('heading', name=name, exact=True)).to_be_visible()
    button(page, t['actions']['edit']).click()
    dialog = form(page, t, True)
    expect(type_control(dialog, t, kind)).to_be_checked()
    for role in ROLES:
        expect(role_control(dialog, t, role)).to_be_checked(checked=role in roles)
        if role in TYPE_ROLES[kind]:
            expect(role_control(dialog, t, role)).to_be_enabled()
        else:
            expect(role_control(dialog, t, role)).to_be_disabled()
    if kind != 'host_ethernet':
        expect(dialog.get_by_role('button').filter(has_text=t['form']['advanced'])).to_have_attribute('aria-expanded', 'false')
    button(dialog, t['actions']['cancel']).click()
    expect(dialog).not_to_be_visible()
    check(events == [{'method': 'POST', 'path': PREFIX}], 'read/cancel unexpectedly mutated resource')
    evidence.append({'layer': 'ui', 'type': kind, 'selected_roles': roles, 'disabled_roles': [r for r in ROLES if r not in TYPE_ROLES[kind]], 'refresh_edit_backfill': True})
    delete_owned(page, server, t, path, name, baseline, evidence)
    check(events == [{'method': 'POST', 'path': PREFIX}, {'method': 'DELETE', 'path': path}], 'unexpected mutation sequence')


# Mainline: gpuworks/network/type-role/<to-RDMA-and-back>; UI创建含全部角色/CIDR/接口Ethernet → UI切RDMA
# → CIDR保持、interface隐藏、非法roles剔除禁用，切回不复活 → PUT RDMA空interface → GET/刷新advanced自动展开
# → UI切Ethernet确认空interface并保存 → GET/刷新持久化 → UI删除+GET404。无节点/流量效果。
def test_type_transition(page, server, result, locale, key, baseline):
    t, evidence = locale_strings(locale, 'networks'), result['evidence']
    target = TRANSITIONS[key]
    result['mainline'] = f'UI创建Ethernet全部角色/CIDR/接口 → 切换{target}、回切后不复活被剔除角色 → PUT {target} 隐藏接口为空 → GET/刷新advanced自动展开 → PUT Ethernet保留CIDR及角色 → UI删除及GET404'
    events = mutations_on(page, PREFIX)
    name = 'e2e-' + result['run_id'] + '-' + str(CASES.index(key))
    body = {'name': name, 'description': 'type transition', 'type': 'host_ethernet', 'roles': ROLES, 'cidr': '10.77.0.0/16', 'interface': 'ens*'}
    dialog = open_create(page, server, t)
    for field in ['name', 'description', 'cidr', 'interface']:
        dialog.get_by_label(t['form'][field], exact=True).fill(body[field])
    for role in ROLES:
        role_control(dialog, t, role).check()
    created = mutate(page, button(dialog, t['actions']['save']).click, 'POST', PREFIX, body, 201, evidence)
    path = PREFIX + '/' + str(created['id'])
    expect(page).to_have_url(server.base + '/networks/' + str(created['id']))
    persisted(page, server.base, path, body, evidence)
    page.reload()
    button(page, t['actions']['edit']).click()
    dialog = form(page, t, True)
    choose_type(dialog, t, target)
    expect(dialog.get_by_label(t['form']['interface'], exact=True)).to_have_count(0)
    advanced = dialog.get_by_role('button').filter(has_text=t['form']['advanced'])
    expect(advanced).to_have_attribute('aria-expanded', 'true')
    cidr = dialog.get_by_label(t['form']['bootstrapCidr'], exact=True)
    expect(cidr).to_have_value(body['cidr'])
    advanced.click()
    expect(cidr).not_to_be_visible()
    advanced.click()
    expect(cidr).to_be_visible()
    for role in ROLES:
        expect(role_control(dialog, t, role)).to_be_checked(checked=role in TYPE_ROLES[target])
        if role in TYPE_ROLES[target]:
            expect(role_control(dialog, t, role)).to_be_enabled()
        else:
            expect(role_control(dialog, t, role)).to_be_disabled()
    choose_type(dialog, t, 'host_ethernet')
    expect(dialog.get_by_label(t['form']['interface'], exact=True)).to_have_value('ens*')
    expect(dialog.get_by_label(t['form']['cidr'], exact=True)).to_have_value(body['cidr'])
    for role in ROLES:
        expect(role_control(dialog, t, role)).to_be_enabled()
        expect(role_control(dialog, t, role)).to_be_checked(checked=role in TYPE_ROLES[target])
    choose_type(dialog, t, target)
    changed = {**body, 'type': target, 'roles': TYPE_ROLES[target], 'interface': ''}
    mutate(page, button(dialog, t['actions']['save']).click, 'PUT', path, changed, 200, evidence)
    expect(dialog).not_to_be_visible()
    persisted(page, server.base, path, changed, evidence)
    page.reload()
    button(page, t['actions']['edit']).click()
    dialog = form(page, t, True)
    expect(type_control(dialog, t, target)).to_be_checked()
    expect(dialog.get_by_role('button').filter(has_text=t['form']['advanced'])).to_have_attribute('aria-expanded', 'true')
    expect(dialog.get_by_label(t['form']['bootstrapCidr'], exact=True)).to_have_value(body['cidr'])
    for role in ROLES:
        expect(role_control(dialog, t, role)).to_be_checked(checked=role in TYPE_ROLES[target])
    choose_type(dialog, t, 'host_ethernet')
    expect(dialog.get_by_label(t['form']['interface'], exact=True)).to_have_value('')
    back = {**changed, 'type': 'host_ethernet'}
    mutate(page, button(dialog, t['actions']['save']).click, 'PUT', path, back, 200, evidence)
    expect(dialog).not_to_be_visible()
    persisted(page, server.base, path, back, evidence)
    page.reload()
    button(page, t['actions']['edit']).click()
    dialog = form(page, t, True)
    expect(type_control(dialog, t, 'host_ethernet')).to_be_checked()
    expect(dialog.get_by_label(t['form']['interface'], exact=True)).to_have_value('')
    expect(dialog.get_by_label(t['form']['cidr'], exact=True)).to_have_value(body['cidr'])
    for role in ROLES:
        expect(role_control(dialog, t, role)).to_be_checked(checked=role in TYPE_ROLES[target])
    button(dialog, t['actions']['cancel']).click()
    expect(dialog).not_to_be_visible()
    evidence.append({'layer': 'ui', 'transition': ['host_ethernet', target, 'host_ethernet'], 'advanced_toggle_and_saved_expansion': True, 'cidr_retained': body['cidr'], 'interface_draft_retained_but_saved_empty': True, 'roles_not_resurrected': TYPE_ROLES[target]})
    delete_owned(page, server, t, path, name, baseline, evidence)
    check(events == [{'method': 'POST', 'path': PREFIX}] + [{'method': 'PUT', 'path': path}] * 2 + [{'method': 'DELETE', 'path': path}], 'unexpected transition mutations')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--case', choices=CASES + ['all'], default='all')
    parser.add_argument('--locale', choices=['en', 'zh'], default='en')
    parser.add_argument('--server-bin', type=Path, default=ROOT / 'bin/gpuworks')
    parser.add_argument('--artifact-dir', type=Path)
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ').lower() + '-' + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / 'secrets/e2e' / run_id)
    server = LocalServer(args.server_bin, artifacts.private)
    selected = CASES if args.case == 'all' else [args.case]
    manifest_path = ROOT / 'test/e2e/coverage/network-type-roles.json'
    manifest = json.loads(manifest_path.read_text())
    report = {'run_id': run_id, 'status': 'NOT_RUN', 'source_sha': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip(),
              'working_tree': 'dirty' if subprocess.check_output(['git', 'status', '--porcelain'], cwd=ROOT) else 'clean',
              'lane': 'local-real-server-sqlite', 'identity': 'admin', 'locale': args.locale, 'timezone': 'UTC',
              'viewport': {'width': 1440, 'height': 900}, 'retries': 0, 'cases': [], 'selection': selected,
              'source_digests': {str(p.relative_to(ROOT)): digest(p) for p in [Path(__file__).resolve(), manifest_path,
                  ROOT / 'test/e2e/local_browser_fixture.py', ROOT / 'test/e2e/network_storage_lifecycle.py', ROOT / 'test/e2e/platform_token_read_revoke.py', ROOT / 'ui/src/pages/networks/NetworkFormModal.tsx']},
              'command': ['python3', 'test/e2e/network_type_roles.py', *sys.argv[1:]]}
    try:
        check(manifest['test_file'] == 'test/e2e/network_type_roles.py', 'manifest file mismatch')
        indexed = {case['case_id']: case for case in manifest['cases']}
        check(len(manifest['cases']) == len(CASES) and list(indexed) == CASES, 'manifest denominator differs')
        for key in CASES:
            expected_function = 'test_type_role_combination' if key in COMBINATIONS else 'test_type_transition'
            check(indexed[key]['test_function'] == expected_function and indexed[key]['contract_id'] == 'gpuworks/network/type-role/' + key, 'manifest binding differs')
            check(indexed[key]['required_evidence'] == ['ui', 'request', 'persistence', 'cleanup'], 'evidence contract differs')
        report['binary_sha256'] = digest(args.server_bin)
        report['health'] = server.start()
        expected_version = subprocess.check_output(['git', 'describe', '--tags', '--always', '--dirty'], cwd=ROOT, text=True).strip()
        check(report['health']['version'] == expected_version, 'binary health version does not match current source; rebuild first')
        report['base_url'], report['owned_server_pid'] = server.base, server.process.pid
        with sync_playwright() as play:
            browser = play.chromium.launch(channel='chrome', headless=True)
            report['browser_version'] = browser.version
            for key in selected:
                result = {'contract_id': indexed[key]['contract_id'], 'run_id': run_id, 'status': 'NOT_RUN', 'evidence': []}
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
                    baseline = read(page, server.base, PREFIX)
                    check(set(baseline) == {'items', 'roles', 'types', 'type_roles'}, 'collection envelope differs')
                    check(baseline['roles'] == ROLES and baseline['types'] == TYPES and baseline['type_roles'] == TYPE_ROLES, 'type/role dictionary changed; explicitly review denominator')
                    check(all(item['builtin'] for item in baseline['items']), 'previous case left owned resources')
                    errors, bad_http, console_errors = [], [], []
                    page.on('pageerror', lambda error: errors.append(str(error)))
                    page.on('response', lambda r: bad_http.append({'path': urlsplit(r.url).path, 'status': r.status}) if r.status >= 400 else None)
                    page.on('console', lambda message: console_errors.append(message.text) if message.type == 'error' else None)
                    artifacts.start_trace(context)
                    function = test_type_role_combination if key in COMBINATIONS else test_type_transition
                    function(page, server, result, args.locale, key, baseline)
                    check(not errors and not bad_http and not console_errors, f'browser errors: {errors}, {bad_http}, {console_errors}')
                    check(page.evaluate('document.documentElement.scrollWidth <= innerWidth'), 'page horizontal overflow')
                    check(set(indexed[key]['required_evidence']) <= {e['layer'] for e in result['evidence']}, 'missing required evidence')
                    result['browser_errors'] = {'page': errors, 'http': bad_http, 'console': console_errors}
                    page.screenshot(path=artifacts.directory / f'{key}-complete.png')
                    result['status'] = 'VERIFIED_PENDING_ARTIFACT_SCAN'
                except Exception:
                    result['status'], result['failure'] = 'FAIL_TEST', traceback.format_exc()
                    if not any(secret in page.content() for secret in artifacts.secrets if len(secret) > 8):
                        page.screenshot(path=artifacts.directory / f'{key}-failure.png')
                finally:
                    artifacts.stop_trace(context)
                    context.close()
                if result['status'] == 'FAIL_TEST':
                    # Freeze this isolated database; do not contaminate subsequent cases.
                    break
            browser.close()
    except Exception:
        report['status'] = 'FAIL_TEST' if report.get('preflight') == 'PASS' else 'BLOCKED_INFRA'
        report['failure'] = traceback.format_exc()
    finally:
        report['server_stopped'] = server.stop()
        executed = {c['contract_id'] for c in report['cases']}
        for key in selected:
            contract_id = 'gpuworks/network/type-role/' + key
            if contract_id not in executed:
                report['cases'].append({'contract_id': contract_id, 'run_id': run_id, 'status': 'NOT_RUN', 'evidence': [], 'reason': 'Earlier failure froze this run'})
        log = artifacts.private / 'server.log'
        if log.exists():
            (artifacts.directory / 'server.log').write_text(artifacts.redact(log.read_text()))
        report['private_state'] = 'Isolated SQLite retained for 7 days; no process kept. Remove only this run directory after diagnosis.'
        artifacts.write_json('report.json', report)
        try:
            check(report['source_sha'] == subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip(), 'HEAD changed during execution')
            check(all(digest(ROOT / path) == value for path, value in report['source_digests'].items()), 'source changed during execution')
            if 'binary_sha256' in report:
                check(digest(args.server_bin) == report['binary_sha256'], 'binary changed during execution')
            report['source_integrity'] = 'PASS'
        except Exception:
            report['status'] = 'FAIL_TEST'
            report['source_integrity'] = {'status': 'FAIL', 'detail': traceback.format_exc()}
        try:
            artifacts.scan()
            report['artifact_scan'] = 'PASS'
        except Exception:
            report['status'] = 'FAIL_TEST'
            report['artifact_scan'] = 'FAIL: do not upload this private run directory'
        if report['status'] == 'NOT_RUN':
            verified = len(report['cases']) == len(selected) and all(c['status'] == 'VERIFIED_PENDING_ARTIFACT_SCAN' for c in report['cases'])
            report['status'] = 'PASS' if verified and report['server_stopped'] else 'FAIL_TEST'
            if report['status'] == 'PASS':
                for result in report['cases']:
                    result['status'] = 'PASS'
        if not report['server_stopped']:
            report['status'] = 'FAIL_TEST'
        artifacts.write_json('report.json', report)
    print(json.dumps({'status': report['status'], 'report': str(artifacts.directory / 'report.json')}))
    return 0 if report['status'] == 'PASS' else 1


if __name__ == '__main__':
    raise SystemExit(main())
