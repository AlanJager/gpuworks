"""Explicit remote lane: authenticated browser, real agent commands, owned artifacts.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Credentials come only from a private config file. No local-server fallback.
"""
from __future__ import annotations

import base64
import hashlib
from datetime import datetime, timezone
import json
from pathlib import Path
import re
import shlex
import subprocess
import textwrap
import time
import uuid
from urllib.parse import urlsplit

from playwright.sync_api import expect
from local_browser_fixture import Artifacts, ROOT, digest, http
from platform_token_read_revoke import button


class RemoteSession:
    def __init__(self, browser, config, directory=None):
        self.config = config
        self.backend_base = config['base_url'].rstrip('/')
        assert urlsplit(self.backend_base).hostname not in ('localhost', '127.0.0.1'), 'remote backend URL required'
        self.base = config.get('ui_base_url', self.backend_base).rstrip('/')
        self.run_id = datetime.now(timezone.utc).strftime('%Y%m%dt%H%M%Sz').lower() + '-' + uuid.uuid4().hex[:8]
        self.artifacts = Artifacts(Path(directory) if directory else ROOT / 'secrets/e2e' / self.run_id)
        self.artifacts.remember(config['password'])
        self.context = browser.new_context(viewport={'width': 1440, 'height': 900}, locale='en-US')
        self.context.add_init_script("localStorage.setItem('i18nextLng', 'en')")
        self.page = self.context.new_page()
        self.page.set_default_timeout(30000)
        self.owned = []
        self.errors = []
        self.evidences = []
        self.report = {'run_id': self.run_id, 'lane': 'remote-real-agent-gpu', 'locale': 'en',
            'source_sha': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip(),
            'fixture_sha256': digest(Path(__file__)), 'base_url': self.base,
            'browser_version': browser.version, 'cases': [], 'owned': self.owned}
        self.report['backend_url'] = self.backend_base
        if config.get('ui_base_url'):
            assert config.get('ui_source_sha'), 'explicit local UI source identity required'
            assert config['ui_source_sha'] == self.report['source_sha'], 'local UI must serve this checkout HEAD'
            self.report.update(lane='local-ui-remote-runtime', ui_source_sha=config['ui_source_sha'])
        self.report['source_dirty'] = bool(subprocess.check_output(
            ['git', 'status', '--porcelain', '--untracked-files=normal'], cwd=ROOT, text=True).strip())
        self.report['source_files'] = {p.name: digest(p) for p in (ROOT / 'test/e2e').glob('remote_*.py')}
        self.page.on('pageerror', lambda error: self.errors.append(str(error)))

    def login(self):
        auth = json.loads((ROOT / 'ui/src/i18n/locales/en/auth.json').read_text())
        self.page.goto(self.base + '/apikeys')
        self.page.get_by_role('textbox', name=auth['username'], exact=True).fill(self.config['username'])
        self.page.get_by_label(auth['password'], exact=True).fill(self.config['password'])
        with self.page.expect_response(lambda r: urlsplit(r.url).path == '/api/v1/auth/login' and r.request.method == 'POST') as response:
            button(self.page, auth['submit']).click()
        assert response.value.status == 200, 'browser login failed'
        for cookie in self.context.cookies():
            self.artifacts.remember(cookie['value'])
        title = json.loads((ROOT / 'ui/src/i18n/locales/en/apikeys.json').read_text())['title']
        expect(self.page.get_by_role('heading', name=title, exact=True)).to_be_visible()
        health = self.api('GET', '/api/v1/healthz')
        assert health['status'] == 'ok' and health['version'], 'BLOCKED_INFRA: remote health/version unavailable'
        if self.config.get('ui_base_url'):
            status, direct_health = http(self.backend_base, 'GET', '/healthz')
            assert status == 200 and direct_health['version'] == health['version'], 'BLOCKED_INFRA: proxy backend identity differs'
            self.verify_ui_sources()
        self.node = self.api('GET', '/api/v1/nodes/' + self.config['node_uuid'])
        assert self.node['state'] == 'online', 'BLOCKED_INFRA: selected agent must be online'
        terminal = self.api('GET', '/api/v1/nodes/' + self.node['uuid'] + '/terminal/settings')
        assert terminal['can_open'], 'BLOCKED_INFRA: real host terminal required for runtime evidence'
        self.report['preflight'] = {'health': health, 'node_uuid': self.node['uuid'],
            'agent_version': self.node['agent_version'], 'runtime': self.node['runtime_driver'],
            'gpus': [{'uuid': g['uuid'], 'name': g['name'], 'index': g['index']} for g in self.node['gpus']]}
        self.flush()

    def verify_ui_sources(self):
        """Verify actual Vite modules against this checkout, not a supplied SHA alone."""
        identities = []
        for path in ('src/main.tsx', 'src/pages/APIKeysPage.tsx',
                'src/pages/apikeys/SecretModal.tsx', 'src/pages/networks/NetworkFormModal.tsx',
                'src/pages/networks/NetworkDetailPage.tsx', 'src/pages/networks/NetworkTestPanel.tsx',
                'src/pages/networks/NetworkTestResult.tsx', 'src/pages/storage/StorageFormModal.tsx',
                'src/pages/storage/StorageDetailPage.tsx'):
            response = self.context.request.get(self.base + '/' + path)
            assert response.status == 200, 'BLOCKED_INFRA: UI module unavailable: ' + path
            body = response.body()
            match = re.search(rb'sourceMappingURL=data:application/json;base64,([^\s]+)', body)
            assert match, 'BLOCKED_INFRA: explicit Vite UI source map required'
            source_map = json.loads(base64.b64decode(match[1]))
            source = (ROOT / 'ui' / path).read_text()
            assert source in source_map['sourcesContent'], 'BLOCKED_INFRA: served UI differs from checkout: ' + path
            identities.append({'path': path, 'source_sha256': hashlib.sha256(source.encode()).hexdigest(),
                'response_sha256': hashlib.sha256(body).hexdigest(), 'served_source_matches_checkout': True})
        self.report['ui_asset_identity'] = identities

    def api(self, method, path, body=None, expected=200):
        response = self.context.request.fetch(self.base + path, method=method,
            data=body, timeout=120000)
        assert response.status == expected, f'{method} {path}: {response.status}: {self.artifacts.redact(response.text())[:1500]}'
        return response.json() if response.body() else None

    def poll(self, fn, predicate, timeout=120):
        deadline = time.monotonic() + timeout
        last = None
        while time.monotonic() < deadline:
            last = fn()
            if predicate(last):
                return last
            # The next animation frame yields browser events; no fixed sleep/retry.
            self.page.evaluate('() => new Promise(resolve => requestAnimationFrame(resolve))')
        raise AssertionError('condition deadline exceeded: ' + self.artifacts.redact(json.dumps(last))[:2000])

    def host(self, code, *, node_uuid=None):
        """Run bounded fixture/evidence Python through the actual agent host PTY.

        Not a browser-terminal UX claim. Commands must be read-only or restricted
        to resources explicitly owned by this run. The caller assigns `result`.
        """
        marker = 'e2e_result_' + uuid.uuid4().hex
        wrapped = ('import json,base64,traceback\ntry:\n' + textwrap.indent(code, '    ') +
            '\nexcept Exception:\n    result={"host_error":traceback.format_exc()}\nprint(' + repr(marker) +
            "+base64.b64encode(json.dumps(result).encode()).decode(), flush=True)\n")
        encoded = base64.b64encode(wrapped.encode()).decode()
        command = 'stty -echo; python3 -c ' + shlex.quote("import base64;exec(base64.b64decode(" + repr(encoded) + '))') + '\n'
        output = self.page.evaluate("""({node,command,marker}) => new Promise((resolve,reject) => {
          const url = new URL(`/api/v1/nodes/${node}/terminal?target=host&rows=24&cols=240`, location.origin);
          url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
          const ws = new WebSocket(url); ws.binaryType='arraybuffer';
          const decoder=new TextDecoder(); let output='', sent=false, finished=false;
          const finish=(error,value)=>{ if(finished)return; finished=true; clearTimeout(timer); ws.close(); error?reject(new Error(error)):resolve(value); };
          const timer=setTimeout(()=>finish('agent command deadline exceeded: '+output.slice(-1500)),90000);
          ws.onmessage=e=>{
            if(typeof e.data==='string') {
              const m=JSON.parse(e.data);
              if(m.type==='info'&&!sent){sent=true;ws.send(new TextEncoder().encode(command));}
              if(m.type==='error')finish(m.message);
            } else {
              output+=decoder.decode(e.data,{stream:true});
              const start=output.indexOf(marker);
              if(start>=0){const tail=output.slice(start+marker.length); const end=tail.indexOf('\\n');
                if(end>=0)finish(null,tail.slice(0,end).trim());}
            }
          };
          ws.onerror=()=>finish('agent terminal websocket failed');
          ws.onclose=()=>{if(!finished)finish('agent terminal closed without result: '+output.slice(-1500));};
        })""", {'node': node_uuid or self.node['uuid'], 'command': command, 'marker': marker})
        result = json.loads(base64.b64decode(output))
        assert not isinstance(result, dict) or 'host_error' not in result, self.artifacts.redact(str(result))
        return result

    def evidence(self, layer, detail):
        self.evidences.append({'layer': layer, 'detail': detail})
        self.flush()

    def own(self, kind, identity):
        self.owned.append({'kind': kind, 'id': identity})
        self.flush()

    def forget(self, kind, identity):
        self.owned.remove({'kind': kind, 'id': identity})
        self.flush()

    def flush(self):
        self.report['evidence'] = self.evidences
        self.artifacts.write_json('report.json', self.report)

    def close(self):
        self.artifacts.stop_trace(self.context)
        self.flush()
        self.context.close()
