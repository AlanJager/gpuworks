"""Real host PTY checkpoints: bytes, SIGWINCH, audit and process release."""
import hashlib
import json
import os
import pwd
import shlex
import time
from urllib.parse import parse_qs, urlsplit
from playwright.sync_api import expect
from terminal_host_oracles import Frames, TEXT, audit_contract, require, socket_contract


def poll(page, read, label, timeout=25):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = read()
        if value:
            return value
        page.wait_for_timeout(100)
    raise AssertionError('host gate timeout: ' + label)


class Sessions:
    def __init__(self, owner, context, locale):
        self.owner, self.context, self.locale = owner, context, locale
        self.text = TEXT[locale]
        self.records = []
        self.last_page = None
        self.expected_http = set()
        self.http_errors, self.page_errors, self.console_errors = [], [], []

    def observe(self, page, node, wait_for_document=False):
        for old in self.records:
            if old['page'] is page and old['socket_discovery_active']:
                # Remove precisely the callback actually registered on Page.
                # The old WebSocket's own frame listeners remain attached.
                page.remove_listener('websocket', old['discover_socket'])
                old['socket_discovery_active'] = False
                if not old['document_ready']:
                    page.remove_listener('framenavigated', old['document_commit'])
        self.owner.pages.append(page)
        self.last_page = page
        record = {'page': page, 'frames': Frames(), 'node': node, 'prior': {r['id'] for r in self.owner.audits()},
                  'preflights': [], 'entry': 'deep' if urlsplit(page.url).query == 'target=host' else 'popup'}
        record['document_ready'] = not wait_for_document
        record['socket_discovery_active'] = True
        def discover_socket(socket):
            if record['document_ready']:
                record['frames'].attach(socket)
        def document_commit(frame):
            if frame is page.main_frame:
                record['document_ready'] = True
                page.remove_listener('framenavigated', record['document_commit'])
        record['discover_socket'], record['document_commit'] = discover_socket, document_commit
        if wait_for_document:
            page.on('framenavigated', document_commit)
        page.on('websocket', discover_socket)
        page.on('pageerror', lambda error: self.page_errors.append(str(error)))
        page.on('console', lambda m: self.console_errors.append({'url': m.location.get('url', ''), 'text': m.text}) if m.type == 'error' else None)
        def response(response):
            path = urlsplit(response.url).path
            if path.endswith('/terminal/settings'):
                record['preflights'].append(response)
            if response.status >= 400:
                self.http_errors.append((path, response.status, response.request.method))
        page.on('response', response)
        self.records.append(record)
        return record

    def deep(self, node, context=None):
        page = (context or self.context).new_page()
        record = self.observe(page, node)
        record['entry'] = 'deep'
        page.goto(self.owner.server.base + '/nodes/' + node['uuid'] + '/terminal?target=host')
        return record

    def popup(self, parent, row, label, node, context=None):
        context = context or self.context
        records = {}
        def created(page):
            records[page] = self.observe(page, node)
        context.on('page', created)
        try:
            with context.expect_page() as opened:
                row.get_by_role('button', name=label, exact=True).click()
            record = records[opened.value]
            opened.value.wait_for_load_state()
            require(opened.value.evaluate('window.opener === null'), 'host terminal popup has an opener')
            record['noopener'] = True
            return record
        finally:
            context.remove_listener('page', created)

    def connected(self, record):
        page, node, frames = record['page'], record['node'], record['frames']
        expect(page.get_by_text(self.text['open'], exact=True)).to_be_visible(timeout=20000)
        expect(page.locator('.xterm')).to_be_visible()
        expect(page.locator('.node-terminal-bar')).to_contain_text(self.text['host'] + ' · ' + node['hostname'])
        expect(page).to_have_title(self.text['title'] + ' · ' + node['hostname'] + ' — GPUWorks')
        require(urlsplit(page.url).path == '/nodes/' + node['uuid'] + '/terminal' and parse_qs(urlsplit(page.url).query, keep_blank_values=True) == ({'target': ['host']} if record['entry'] == 'deep' else {}),
                'host terminal navigation target differs')
        def discovered():
            require(len(frames.urls) <= 1, 'one terminal document opened duplicate host websockets')
            return bool(frames.urls)
        poll(page, discovered, 'one real host websocket')
        record['expected_sockets'] = 1
        record['geometry'] = socket_contract(frames.urls[0], node['uuid'])
        info = poll(page, lambda: next((v for v in frames.controls if v['type'] == 'info'), None), 'real host info frame')
        require(info.get('uid') == os.getuid() and info.get('user') == pwd.getpwuid(os.getuid()).pw_name
                and bool(info.get('shell')) and bool(info.get('cwd')), 'host identity control differs from local agent account')
        poll(page, lambda: len(record['preflights']) == 1, 'one actual preflight')
        pre = record['preflights'][0]
        require(pre.status == 200 and pre.request.method == 'GET' and pre.json()['can_open'] is True,
                'host UI preflight failed')
        def active():
            rows = [r for r in self.owner.audits() if r['id'] not in record['prior']]
            require(len(rows) <= 1, 'one UI action created multiple sessions')
            return rows[0] if rows and rows[0]['active'] else None
        record['audit'] = poll(page, active, 'new active host audit')
        audit_contract(record['audit'], node['uuid'], 'admin', True)
        self.owner.db_audit(record['audit'])
        stem = self.owner.root / ('session-' + str(record['audit']['id']))
        record['stem'] = stem
        command = 'stty -echo; umask 077; cat /proc/$$/stat > ' + shlex.quote(str(stem.with_suffix('.stat'))) + '; stty size > ' + shlex.quote(str(stem.with_suffix('.size'))) + '; : > ' + shlex.quote(str(stem.with_suffix('.ready')))
        self.send(record, command)
        poll(page, lambda: stem.with_suffix('.ready').exists(), 'actual shell identity/geometry files')
        record['process'] = self.owner.observe_process(stem, node['uuid'])
        geometry = frames.resizes[-1] if frames.resizes else record['geometry']
        require(stem.with_suffix('.size').read_text().split() == [str(geometry[k]) for k in ('rows', 'cols')],
                'actual host stty size differs from browser geometry')
        self.owner.report.setdefault('connections', []).append({'path': urlsplit(page.url).path,
            'title': page.title(), 'websocket': frames.urls[0], 'preflight': pre.json(), 'info': info,
            'audit_id': record['audit']['id'], 'noopener': record.get('noopener'), 'entry': record['entry']})
        return record

    @staticmethod
    def send(record, command):
        page = record['page']
        page.locator('.xterm').click()
        page.keyboard.insert_text(command)
        page.keyboard.press('Enter')

    def released(self, record, reason, timeout=25):
        page, observed = record['page'], record['process']
        def released():
            rows = [r for r in self.owner.audits() if r['id'] == record['audit']['id']]
            require(len(rows) == 1, 'owned host audit disappeared')
            row = rows[0]
            return row if row['active'] is False and self.owner.current(observed) != observed else None
        # The event loop pump can be any surviving page when the terminal tab closed.
        driver = page if not page.is_closed() else self.context.pages[0]
        row = poll(driver, released, 'host process and audit release', timeout)
        audit_contract(row, record['node']['uuid'], 'admin', False, reason)
        require(row['bytes_in'] > 0 and row['bytes_out'] > 0, 'host byte metadata must observe real command IO')
        require(row['bytes_in'] == record['frames'].input_bytes, 'audit input bytes differ from real browser binary frames')
        if reason in ('shell_exited', 'idle_timeout'):
            poll(driver, lambda: len(record['frames'].output()) == row['bytes_out'], 'exact audited host output byte count')
        else:
            require(row['bytes_out'] >= len(record['frames'].output()), 'audit output smaller than received WS bytes')
        self.owner.db_audit(row)
        self.owner.report.setdefault('audits', []).append(row)
        return row

    def utf8_resize(self, record):
        page, frames, stem = record['page'], record['frames'], record['stem']
        gate = stem.with_suffix('.release')
        output = stem.with_suffix('.output')
        # The filesystem gate opens only after the browser receives the first byte.
        # This forces a real WS boundary inside 中; no fake socket or output fixture.
        start = len(frames.output())
        waiting = 'i=0; while [ ! -e ' + shlex.quote(str(gate)) + ' ] && [ "$i" -lt 400 ]; do sleep 0.05; i=$((i+1)); done; '
        command = "printf '\\344'; " + waiting + "printf '\\270\\255\\342\\224\\214\\347\\273\\210\\347\\253\\257\\342\\224\\220\\n'; "
        command += 'for i in 1 2 3; do printf "BATCH-%s\\n" "$i"; done; printf "done\\n" > ' + shlex.quote(str(output))
        self.send(record, command)
        poll(page, lambda: frames.output()[start:].endswith(b'\xe4'), 'first incomplete UTF8 byte observed as actual WS data')
        gate.touch()
        payload = '中┌终端┐\r\nBATCH-1\r\nBATCH-2\r\nBATCH-3\r\n'.encode()
        poll(page, lambda: payload in frames.output()[start:] and output.exists() and output.read_text() == 'done\n', 'full split UTF8 and continuous batches')
        frames.split_payload(payload)
        expect(page.locator('.xterm-rows')).to_contain_text('中┌终端┐')
        for number in (1, 2, 3):
            expect(page.locator('.xterm-rows')).to_contain_text('BATCH-' + str(number))
        old = len(frames.resizes)
        winch = stem.with_suffix('.winch')
        ready = stem.with_suffix('.trap-ready')
        self.send(record, "trap " + shlex.quote("stty size >> " + shlex.quote(str(winch))) + " WINCH; : > " + shlex.quote(str(ready)))
        poll(page, ready.exists, 'host SIGWINCH trap installed')
        page.set_viewport_size({'width': 1180, 'height': 740})
        poll(page, lambda: len(frames.resizes) > old, 'real ResizeObserver client frame')
        # One viewport change can emit several fit frames; only the quiescent
        # final frame may be compared to the settled PTY size. Each round sends
        # a fresh stty probe and requires no newer frame during the round trip
        # plus the host WINCH log's last applied size to match that frame.
        deadline = time.monotonic() + 25
        attempt = 0
        while True:
            attempt += 1
            marker = len(frames.resizes)
            changed = frames.resizes[-1]
            require({k: changed[k] for k in ('rows', 'cols')} != record['geometry'], 'resize produced unchanged rows/cols')
            expected = [str(changed[k]) for k in ('rows', 'cols')]
            probe = stem.with_suffix('.probe-' + str(attempt))
            self.send(record, 'stty size > ' + shlex.quote(str(probe)))
            poll(page, lambda: probe.exists() and winch.exists(), 'actual stty/SIGWINCH files',
                 timeout=max(1.0, deadline - time.monotonic()))
            actual_size = probe.read_text().split()
            actual_winch = [line.split() for line in winch.read_text().splitlines()]
            if actual_size == expected and actual_winch and actual_winch[-1] == expected and marker == len(frames.resizes):
                break
            require(time.monotonic() < deadline,
                    'host SIGWINCH/stty never settled to the final resize frame: '
                    f'expected={expected} size={actual_size} winch={actual_winch} resizes={frames.resizes[-3:]}')
        self.owner.report['utf8_resize'] = {'sha256': hashlib.sha256(payload).hexdigest(), 'bytes': len(payload),
            'actual_split_after_byte': 1, 'batches': [1, 2, 3], 'rows': changed['rows'], 'cols': changed['cols'],
            'sigwinch': True, 'real_host_pid': record['process'][0]}

    def denied(self, record, reason, status=200):
        page = record['page']
        expect(page.get_by_text(self.text['error'], exact=True)).to_be_visible(timeout=20000)
        expect(page.get_by_text(reason, exact=False)).to_be_visible()
        poll(page, lambda: len(record['preflights']) == 1, 'denied actual preflight')
        pre = record['preflights'][0]
        require(pre.status == status and pre.request.method == 'GET'
                and urlsplit(pre.url).path == '/api/v1/nodes/' + record['node']['uuid'] + '/terminal/settings'
                and urlsplit(pre.url).query == '', 'denied preflight method/path/query/status differs')
        if status == 200:
            require(pre.json()['can_open'] is False and pre.json()['reason'] == reason, 'denied preflight reason differs')
        else:
            require(pre.json()['code'] == 'ADMINISTRATOR_REQUIRED', 'permission error code differs')
        record['expected_sockets'] = 0
        require(record['frames'].urls == [], 'denied UI opened a websocket')
        require({r['id'] for r in self.owner.audits()} == record['prior'], 'denied UI created audit/session')

    def upgrade_denied(self, context, node, status, reason=None):
        path = '/api/v1/nodes/' + node + '/terminal'
        response = context.request.get(self.owner.server.base + path + '?target=host&rows=24&cols=80', headers={
            'Connection': 'Upgrade', 'Upgrade': 'websocket', 'Sec-WebSocket-Version': '13',
            'Sec-WebSocket-Key': 'dGhlIHNhbXBsZSBub25jZQ==', 'Origin': self.owner.server.base})
        require(response.status == status, 'direct real WS admission status differs')
        value = response.json()
        require(value.get('error') == reason if reason else value.get('code') == 'ADMINISTRATOR_REQUIRED',
                'direct WS denial reason/code differs')
        self.owner.report.setdefault('upgrade_denials', []).append({'path': path, 'status': status, 'code': value.get('code'), 'reason': value.get('error')})

    def verify_socket_counts(self):
        for record in self.records:
            if 'expected_sockets' in record:
                require(len(record['frames'].urls) == record['expected_sockets'],
                        'terminal document websocket count changed after admission')

    def verify_errors(self):
        self.verify_socket_counts()
        require(not self.page_errors, 'host gate pageerror')
        require(all((path, status) in self.expected_http and method == 'GET' for path, status, method in self.http_errors), 'unexpected host gate HTTP error')
        for error in self.console_errors:
            path = urlsplit(error['url']).path
            require(any(p == path and str(code) in error['text'] for p, code in self.expected_http),
                    'unattributed host gate console error')
