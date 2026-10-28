"""Post-mainline host-only gates. No container authority or coverage declaration."""
import copy
import json
import re
import sqlite3
from urllib.parse import urlsplit
from playwright.sync_api import expect
from local_browser_fixture import ROOT
from platform_token_read_revoke import button
from terminal_host_oracles import POLICY, TEXT, NODE_BUSY, GLOBAL_BUSY, OFFLINE, require, policy_contract
from terminal_host_owner import Owner
from terminal_host_sessions import Sessions, poll


def labels(locale, namespace):
    return json.loads((ROOT / 'ui/src/i18n/locales' / locale / (namespace + '.json')).read_text())


def context_for(owner, parent, locale, authenticated=True):
    options = {'locale': 'zh-CN' if locale == 'zh' else 'en-US', 'timezone_id': 'UTC',
               'viewport': {'width': 1440, 'height': 900}}
    if authenticated:
        options['storage_state'] = parent.storage_state()
    result = parent.browser.new_context(**options)
    observed = {'context': result, 'closed': False}
    result.on('close', lambda _: observed.update(closed=True))
    owner.contexts.append(observed)
    return result


def set_policy(owner, page, locale, value):
    policy_contract(value)
    text = labels(locale, 'settings')
    page.goto(owner.server.base + '/settings')
    page.get_by_role('tab', name=text['page']['tabs']['access'], exact=True).click()
    card = page.locator('.ant-card').filter(has_text=text['terminal']['title']).first
    expect(card.get_by_role('switch')).to_be_checked()
    for key, title in [('idle_timeout_min', 'idle'), ('grace_sec', 'grace'),
                       ('max_per_node', 'nodeLimit'), ('max_global', 'globalLimit')]:
        field = card.locator('.ant-form-item').filter(has=page.get_by_text(text['terminal'][title], exact=True))
        field.get_by_role('spinbutton').fill(str(value[key]))
        field.get_by_role('spinbutton').press('Tab')
    with page.expect_response(lambda r: r.request.method == 'PUT'
            and urlsplit(r.url).path == '/api/v1/settings/terminal') as changed:
        button(card, text['common']['save']).click()
    require(changed.value.status == 200 and changed.value.request.post_data_json == value,
            'full UI terminal policy PUT differs')
    status, saved = owner.call('GET', '/api/v1/settings')
    require(status == 200 and saved['terminal'] == value, 'policy independent GET differs')
    with sqlite3.connect(owner.db) as db:
        stored = db.execute("SELECT value FROM settings WHERE key='terminal'").fetchone()
    require(stored and json.loads(stored[0]) == value, 'policy SQLite differs from independent declaration')
    owner.report['policy_puts'].append(copy.deepcopy(value))


def node_row(page, owner, node, locale):
    text = labels(locale, 'nodes')
    page.goto(owner.server.base + '/nodes')
    nav = page.locator('.desktop-sider .app-nav').get_by_role('link', name=labels(locale, 'common')['navigation']['nodes'], exact=True)
    expect(nav).to_have_attribute('href', '/nodes')
    nav.click()
    expect(page).to_have_url(owner.server.base + '/nodes')
    expect(nav.locator('xpath=ancestor::*[@role="menuitem"][1]')).to_have_class(re.compile(r'\bant-menu-item-selected\b'))
    row = page.get_by_role('row').filter(has_text=node['name'])
    expect(row).to_have_count(1)
    control = row.get_by_role('button', name=text['action']['terminal'], exact=True)
    expect(control).to_be_visible()
    expect(control).to_be_enabled()
    return row, text


def permissions(owner, sessions, admin_context, parent, locale):
    context = context_for(owner, parent, locale, False)
    page = context.new_page()
    owner.pages.append(page)
    auth = labels(locale, 'auth')
    owner.sensitive = True
    page.goto(owner.server.base + '/nodes')
    page.get_by_role('textbox', name=auth['username'], exact=True).fill(owner.username)
    page.get_by_label(auth['password'], exact=True).fill('password')
    with page.expect_response(lambda r: urlsplit(r.url).path == '/api/v1/auth/login'
            and r.request.method == 'POST') as login:
        button(page, auth['submit']).click()
    require(login.value.status == 200, 'real user login failed')
    for cookie in context.cookies():
        owner.artifacts.remember(cookie['value'])
    owner.sensitive = False
    row, text = node_row(page, owner, owner.first, locale)
    # Current product deliberately renders the row entry for readers; access is
    # denied at the admin API boundary. Do not invent a hidden-button contract.
    path = '/api/v1/nodes/' + owner.first['uuid'] + '/terminal/settings'
    sessions.expected_http.add((path, 403))
    before = {r['id'] for r in owner.audits()}
    record = sessions.popup(page, row, text['action']['terminal'], owner.first, context)
    sessions.denied(record, TEXT[locale]['permission'], 403)
    record['page'].close()
    record = sessions.deep(owner.first, context)
    sessions.denied(record, TEXT[locale]['permission'], 403)
    sessions.upgrade_denied(context, owner.first['uuid'], 403)
    require({r['id'] for r in owner.audits()} == before and owner.active(owner.first['uuid'])['active_total'] == 0,
            'nonadmin denial consumed an audit or slot')
    record['page'].close()
    owner.report['permissions'] = {'role': 'user', 'row_entry': 'visible/enabled; actual API denies',
        'popup_preflight': 403, 'deep_preflight': 403, 'upgrade': 403,
        'code': 'ADMINISTRATOR_REQUIRED', 'created_audits': 0}


def exercise(parent, server, artifacts, report, locale, first, agent, agent_bin, token):
    result = report['host_gates'] = {'status': 'NOT_RUN', 'candidate_items': ['19.3', '19.5', '19.6', '22.1', '22.3', '22.5', '22.7'],
        'scope': 'real Linux host PTY and navigation only; no container/GPU/runtime claim', 'coverage_credit': 0,
        'gaps': ['22.5: server preflight reasons remain English in zh; no generic translated empty-target screen',
                 'each invocation covers only its selected locale; final en+zh and scoped product mutants required']}
    owner = Owner(server, token, artifacts, result, first, agent, agent_bin)
    sessions, primary = None, None
    try:
        owner.prepare(report['run_id'])
        context = context_for(owner, parent, locale)
        page = context.new_page()
        owner.pages.append(page)
        sessions = Sessions(owner, context, locale)
        set_policy(owner, page, locale, dict(POLICY))
        row, text = node_row(page, owner, owner.first, locale)
        popup = sessions.connected(sessions.popup(page, row, text['action']['terminal'], owner.first))
        sessions.utf8_resize(popup)
        sessions.send(popup, 'exit')
        expect(popup['page'].get_by_text(TEXT[locale]['exited'], exact=True)).to_be_visible()
        expect(popup['page'].locator('.xterm-rows')).to_contain_text(TEXT[locale]['ended'])
        sessions.released(popup, 'shell_exited')
        popup['page'].close()
        # Deep-link and actual reload must establish distinct audited PTYs on
        # the same declared target. Reload is never treated as a SPA200 success.
        deep = sessions.connected(sessions.deep(owner.first))
        # Bind the new document's events before reload, retaining the old audit.
        refreshed = sessions.observe(deep['page'], owner.first, wait_for_document=True)
        deep['page'].reload()
        sessions.released(deep, 'client_closed')
        sessions.connected(refreshed)
        refreshed['page'].close()
        sessions.released(refreshed, 'client_closed')
        disconnected_context = context_for(owner, parent, locale)
        disconnected = sessions.connected(sessions.deep(owner.first, disconnected_context))
        disconnected_context.close()
        sessions.released(disconnected, 'client_closed')
        result['context_disconnect_released'] = True
        # One slot per node, two total: each guard gets a distinct real denial.
        a = sessions.connected(sessions.deep(owner.first))
        denied = sessions.deep(owner.first)
        sessions.denied(denied, NODE_BUSY)
        sessions.upgrade_denied(context, owner.first['uuid'], 429, NODE_BUSY)
        denied['page'].close()
        b = sessions.connected(sessions.deep(owner.node))
        denied = sessions.deep(owner.node)
        sessions.denied(denied, GLOBAL_BUSY)
        sessions.upgrade_denied(context, owner.node['uuid'], 429, GLOBAL_BUSY)
        denied['page'].close()
        a['page'].close(); sessions.released(a, 'client_closed')
        c = sessions.connected(sessions.deep(owner.first))
        c['page'].close(); sessions.released(c, 'client_closed')
        b['page'].close(); sessions.released(b, 'client_closed')
        idle = sessions.connected(sessions.deep(owner.first))
        expect(idle['page'].get_by_text(TEXT[locale]['exited'], exact=True)).to_be_visible(timeout=95000)
        sessions.released(idle, 'idle_timeout', timeout=15)
        exits = [x for x in idle['frames'].controls if x['type'] == 'exit']
        require(len(exits) == 1 and exits[0]['reason'] == 'idle timeout' and exits[0]['code'] == -1,
                'real idle exit frame differs')
        require(owner.active(owner.first['uuid'])['active_total'] == 0, 'idle left hidden host sessions')
        require(owner.report['audits'][-1].get('duration_sec', 0) >= 60, 'idle ended before the configured real minute')
        idle['page'].close()
        revived = sessions.connected(sessions.deep(owner.first))
        sessions.send(revived, 'exit')
        sessions.released(revived, 'shell_exited')
        revived['page'].close()
        permissions(owner, sessions, context, parent, locale)
        missing = {'uuid': 'e2e-' + report['run_id'] + '-missing', 'hostname': 'unused'}
        sessions.expected_http.add(('/api/v1/nodes/' + missing['uuid'], 404))
        unknown = sessions.deep(missing)
        sessions.denied(unknown, OFFLINE)
        unknown['page'].close()
        result['unknown_target'] = {'node_detail': 404, 'preflight': 200, 'can_open': False,
                                    'reason': OFFLINE, 'websocket_count': 0}
        sessions.verify_errors()
        result['status'] = 'PASS'
        report['last_step'] = 'host-gates-real-PTY-policy-audit-navigation-complete'
    except Exception as error:
        primary = error
        result['status'] = 'FAIL'
        result['primary_error'] = artifacts.redact(type(error).__name__ + ': ' + str(error))
        if sessions and sessions.last_page and not sessions.last_page.is_closed():
            try:
                saved = artifacts.capture_failure(sessions.last_page, getattr(owner, 'sensitive', False))
                if saved == 'failure.png':
                    (artifacts.directory / saved).replace(artifacts.directory / 'host-gates-failure.png')
                (artifacts.directory / 'host-gates-failure.aria.txt').write_text(artifacts.redact(sessions.last_page.locator('body').aria_snapshot()))
            except Exception as capture_error:
                result['capture_error'] = artifacts.redact(str(capture_error))
        raise
    finally:
        clean = owner.cleanup()
        if not clean:
            result['status'] = 'FAIL'
            if primary is None:
                raise AssertionError('host gate cleanup did not complete; scoped identities retained')
