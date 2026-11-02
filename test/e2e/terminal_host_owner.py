"""Scoped real host-agent/user ownership; Linux /proc rather than Docker/fake TTY."""
from datetime import datetime, timezone
import json
import sqlite3
import subprocess
import time
from pathlib import Path

from local_browser_fixture import http
from terminal_host_oracles import identity, require


class Owner:
    def __init__(self, server, token, artifacts, report, first, agent, agent_bin):
        self.server, self.token, self.artifacts, self.report = server, token, artifacts, report
        self.db = server.work / 'server-data/gpuworks.db'
        self.first, self.agent_bin = first, agent_bin
        self.agents = {first['uuid']: agent.pid}
        self.extra = None
        self.log = None
        self.node = None
        self.username = None
        self.contexts, self.pages, self.processes = [], [], []
        self.root = artifacts.private / 'host-gates'
        self.root.mkdir(mode=0o700)
        status, settings = self.call('GET', '/api/v1/settings')
        require(status == 200, 'cannot snapshot terminal settings')
        self.original = settings['terminal']
        report.update(coverage_credit=0, processes=[], cleanup=[], remaining_resources=[], policy_puts=[])

    def call(self, method, path, body=None):
        return http(self.server.base, method, path, self.token, body)

    def prepare(self, run_id):
        self.username = 'e2e-' + run_id + '-host-reader'
        now = datetime.now(timezone.utc).isoformat()
        with sqlite3.connect(self.db) as db:
            cursor = db.execute("INSERT INTO users(username,password_hash,role,created_at,updated_at) SELECT ?,password_hash,'user',?,? FROM users WHERE username='admin'", (self.username, now, now))
            require(cursor.rowcount == 1, 'owned user not created')
        status, value = self.call('POST', '/api/v1/nodes', {'name': 'e2e-' + run_id + '-host-second',
                                                        'server_url': self.server.base})
        if isinstance(value, dict) and isinstance(value.get('node'), dict):
            self.node = value['node']
        require(status == 201 and self.node and value.get('token'), 'second real node creation failed')
        self.artifacts.remember(value['token'])
        state = self.root / 'agent-state'
        state.mkdir()
        self.log = (self.root / 'agent.log').open('w')
        self.extra = subprocess.Popen([str(self.agent_bin), 'run', '--server', self.server.base,
            '--token', value['token'], '--state', str(state), '--driver', 'none'],
            stdout=self.log, stderr=subprocess.STDOUT, text=True)
        self.agents[self.node['uuid']] = self.extra.pid
        from terminal_host import wait_online
        self.node = wait_online(self.server.base, self.token, self.node['uuid'], self.extra)
        deadline = time.monotonic() + 25
        while True:
            status, self.first = self.call('GET', '/api/v1/nodes/' + self.first['uuid'])
            code, second = self.call('GET', '/api/v1/nodes/' + self.node['uuid'])
            require(status == 200 and code == 200, 'owned node read failed')
            self.node = second
            if self.first.get('hostname') and second.get('hostname') and self.active(self.first['uuid'])['active_total'] == 0:
                break
            require(time.monotonic() < deadline, 'host facts or original session release did not settle')
            time.sleep(.1)
        self.report['nodes'] = [{'uuid': n['uuid'], 'hostname': n['hostname'], 'agent_pid': self.agents[n['uuid']]}
                                for n in (self.first, self.node)]

    def audits(self):
        status, value = self.call('GET', '/api/v1/terminal/sessions?limit=200')
        require(status == 200 and type(value.get('items')) is list, 'host audit GET failed')
        return value['items']

    def active(self, node):
        status, value = self.call('GET', '/api/v1/nodes/' + node + '/terminal/settings')
        require(status == 200, 'host preflight GET failed')
        return value

    def db_audit(self, row):
        with sqlite3.connect(self.db) as db:
            db.row_factory = sqlite3.Row
            saved = dict(db.execute('SELECT * FROM terminal_sessions WHERE id=?', (row['id'],)).fetchone())
            admin = db.execute("SELECT id FROM users WHERE username='admin'").fetchone()[0]
        require(saved['user_id'] == admin, 'audit DB principal differs')
        for key in ('id', 'username', 'node_uuid', 'target', 'bytes_in', 'bytes_out', 'client_ip'):
            require(saved[key] == row[key], 'audit DB/wire differs: ' + key)
        require(saved['ref'] == '' and bool(saved['ended_at']) == (not row['active']), 'audit DB result differs')
        stamp = lambda value: datetime.fromisoformat(value.replace('Z', '+00:00'))
        require(stamp(saved['started_at']) == stamp(row['started_at']), 'audit DB start timestamp differs')
        if not row['active']:
            require(saved['close_reason'] == row['close_reason'] and stamp(saved['ended_at']) == stamp(row['ended_at']), 'DB close reason/timestamp differs')
        require(set(saved) == {'id', 'user_id', 'username', 'node_uuid', 'target', 'ref', 'client_ip',
                              'started_at', 'ended_at', 'bytes_in', 'bytes_out', 'close_reason'},
                'audit DB contains unexpected content fields')

    def observe_process(self, stem, node):
        observed = identity((stem.with_suffix('.stat')).read_text())
        pid, _ = observed
        require(self.current(observed) == observed, 'real host PTY shell not alive')
        ancestor, visited = pid, []
        while ancestor > 1 and len(visited) < 32:
            visited.append(ancestor)
            raw = Path('/proc/' + str(ancestor) + '/stat').read_text()
            ancestor = int(raw[raw.rfind(')') + 2:].split()[1])
        require(self.agents[node] in visited, 'host shell does not descend from the selected real agent')
        self.processes.append(observed)
        self.report['processes'].append({'pid': observed[0], 'start_ticks': observed[1], 'node_uuid': node})
        return observed

    @staticmethod
    def current(observed):
        try:
            return identity(Path('/proc/' + str(observed[0]) + '/stat').read_text())
        except FileNotFoundError:
            return None

    def cleanup(self):
        errors = []
        def stage(name, fn):
            try:
                require(fn() is True, name + ' not confirmed')
                self.report['cleanup'].append({'stage': name, 'ok': True})
            except Exception as error:
                errors.append(name + ': ' + str(error))
                self.report['cleanup'].append({'stage': name, 'ok': False})
        for page in self.pages:
            stage('owned page close', lambda page=page: (page.close(), page.is_closed())[1] if not page.is_closed() else True)
        for observed in self.contexts:
            def close_context(observed=observed):
                if not observed['closed']:
                    observed['context'].close()
                return observed['closed'] is True
            stage('owned context close', close_context)
        def sessions():
            deadline = time.monotonic() + 20
            while time.monotonic() < deadline:
                if all(not r['active'] for r in self.audits()) and all(self.current(p) != p for p in self.processes):
                    return True
                time.sleep(.1)
            return False
        stage('host sessions and observed shells released', sessions)
        def policy():
            status, _ = self.call('PUT', '/api/v1/settings/terminal', self.original)
            code, settings = self.call('GET', '/api/v1/settings')
            with sqlite3.connect(self.db) as db:
                stored = db.execute("SELECT value FROM settings WHERE key='terminal'").fetchone()
            return status == 200 and code == 200 and settings['terminal'] == self.original and stored is not None and json.loads(stored[0]) == self.original
        stage('original terminal policy restore', policy)
        def stop():
            if self.extra and self.extra.poll() is None:
                self.extra.terminate()
                try:
                    self.extra.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    self.extra.kill(); self.extra.wait(timeout=5)
            return self.extra is None or self.extra.poll() is not None
        stage('second real agent stopped', stop)
        if self.log:
            stage('second agent log closed', lambda: (self.log.close(), self.log.closed)[1])
        if self.node:
            def delete_node():
                path = '/api/v1/nodes/' + self.node['uuid']
                status, body = self.call('DELETE', path + '?force=true')
                if status != 200 or body != {'status': 'deleted', 'agent_uninstalled': False}:
                    return False
                return self.call('GET', path)[0] == 404
            stage('second owned node gone', delete_node)
        if self.username:
            def delete_user():
                with sqlite3.connect(self.db) as db:
                    db.execute("DELETE FROM users WHERE username=? AND role='user'", (self.username,))
                    return db.execute('SELECT count(*) FROM users WHERE username=?', (self.username,)).fetchone()[0] == 0
            stage('owned permission user gone', delete_user)
        if errors:
            self.report['remaining_resources'] = [{'kind': 'host-gates', 'node_uuid': self.node and self.node['uuid'],
                'username': self.username, 'processes': self.report['processes'],
                'state': errors, 'reclaim': 'inspect only the above IDs and PID/start_ticks; original agent belongs to terminal_host runner'}]
        self.report['cleanup_errors'] = errors
        return not errors
