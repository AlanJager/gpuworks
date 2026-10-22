#!/usr/bin/env python3
"""Storage / four-step NFS cache creation through real browser/server/agent/mount.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-storage-nfs-cache-create E2E_NFS_EXPORT=host:/path [E2E_LOCALE=zh]
After build: python3 test/e2e/storage_cache_create.py --export host:/path --locale en
Requires Python Playwright, system Chrome, sudo -n, mount.nfs4 and an explicitly owned,
reachable NFSv4.2 export that allows root writes. Does not create or stop the
export. Owns only its isolated server, agent, mount and cache paths. Uses
driver=none; makes no GPU/container execution claim.
Artifacts: secrets/e2e/<runId>; agent-created cache directories under private/
are root-owned, so remove only this run directory with sudo after diagnosis.
"""
from __future__ import annotations
import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import socket
import subprocess
import sys
import threading
import time
import traceback
from urllib.parse import urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest
from network_storage_lifecycle import locale_strings, read
from platform_token_read_revoke import button, check
from storage_cache_policy import capture, check_browser, cleanup_stage, copy_redacted, login, mutate, observe_browser, GIB

CONTRACT = 'gpuworks/storage/nfs-cache-create'
CASE = '存储 / NFS Cache 四步创建 / 节点例外、真实试挂、创建后挂载与缓存预算收敛、刷新回读'
VIEWPORT = {'width': 1440, 'height': 900}


def until(call, accept, description, timeout=40):
    deadline = time.monotonic() + timeout
    while True:
        value = call()
        if accept(value):
            return value
        if time.monotonic() >= deadline:
            raise AssertionError(f'timed out: {description}; last observation: {value!r}')
        threading.Event().wait(.2)


def mounted(path):
    return [line for line in Path('/proc/self/mountinfo').read_text().splitlines()
            if mountinfo_path(line.split()[4]) == str(path)]


def mountinfo_path(value):
    return re.sub(r'\\(040|011|012|134)', lambda match: chr(int(match[1], 8)), value)


def command_alive(command):
    """An uninterruptible NFS syscall may outlive SIGKILL; retain that PID."""
    if command.get('active') is False or not command.get('pid'):
        return False
    try:
        fields = Path(f'/proc/{command["pid"]}/stat').read_text().rsplit(') ', 1)[1].split()
        if fields[19] != command['starttime']:
            command['active'] = False
            return False  # The original command exited and its PID was reused.
        command['pgid'] = int(fields[2])
        command['active'] = fields[0] not in ('Z', 'X')
        return command['active']
    except FileNotFoundError:
        command['active'] = False
        return False


def root_command(owned, argv, timeout=15):
    """The root watchdog kills its own process group, including the root child.

    A timeout around sudo alone only kills sudo and can leave a late NFS writer.
    GNU timeout must run without --foreground so its process group stays scoped.
    """
    pidfile = owned['agent_state'].parent / ('nfs-command-' + uuid.uuid4().hex + '.pid')
    command = {'pidfile': str(pidfile), 'argv': argv, 'pid': None}
    owned.setdefault('nfs_commands', []).append(command)
    try:
        return subprocess.run(['sudo', '-n', 'timeout', '--signal=KILL', f'{timeout}s',
            'sh', '-c', 'umask 022; awk \'{print $1, $22}\' /proc/$$/stat > "$1" || exit; shift; exec "$@"',
            'owned-nfs-command', str(pidfile), *argv], check=True, capture_output=True, text=True, timeout=timeout + 5)
    finally:
        if pidfile.exists():
            pid, command['starttime'] = pidfile.read_text().split()
            command['pid'] = int(pid)
        command['active'] = command_alive(command)


def test_storage_cache_create(page, server, artifacts, report, owned, export, node_uuid, mount, cache, node_cache, locale):
    t = locale_strings(locale, 'storage')
    common = locale_strings(locale, 'common')
    f, d, cp = t['form'], t['cacheDraft'], t['cachePolicy']
    evidence = report['evidence']
    name = f"e2e-{report['run_id'].lower()}-cache"
    report['storage_name'] = name
    # Register the storage as owned the moment the server creates it, before any assertion runs.
    def created_seen(response):
        # Keep the response; its body is read in this greenlet, never inside the event handler.
        if urlsplit(response.url).path == '/api/v1/storages' and response.request.method == 'POST' and response.status == 201:
            owned['created_response'] = response
    page.on('response', created_seen)
    # Mainline: empty NFS list -> four-step wizard -> node cache override -> actual trial mount
    # -> create -> independent GET + actual kernel mount -> refresh/edit -> remove -> actual unmount.
    page.goto(server.base + '/storage')
    page.get_by_role('button').filter(has=page.get_by_text(t['page']['add'], exact=True)).click()
    dialog = page.get_by_role('dialog', name=f['addTitle'], exact=True)
    dialog.get_by_text(common['storageType']['nfs_cache'], exact=True).click()
    expect(dialog.get_by_role('radio', name=common['storageType']['nfs_cache'], exact=True)).to_be_checked()
    dialog.get_by_label(f['name'], exact=True).fill(name)
    dialog.get_by_label(f['export'], exact=True).fill(export)
    dialog.get_by_label(f['mountPoint'], exact=True).fill(str(mount))
    dialog.get_by_label(f['mountOptions'], exact=True).fill('vers=4.2,proto=tcp')
    button(dialog, d['next']).click()
    expect(dialog.get_by_role('radio', name=f['scope']['all'], exact=True)).to_be_checked()
    button(dialog, d['next']).click()
    dialog.get_by_role('textbox', name=f['cachePath'], exact=True).fill(str(cache))
    dialog.get_by_role('radio', name=cp['custom'], exact=True).check()
    dialog.get_by_role('spinbutton', name=f['cacheMax'], exact=True).fill('8')
    node_row = dialog.get_by_role('row').filter(has_text='Cache live node')
    expect(node_row).to_be_visible()
    button(node_row, d['configure']).click()
    override = page.get_by_role('dialog', name=d['editTitle'].replace('{{count}}', '1'), exact=True)
    override.get_by_role('radiogroup', name=d['pathMode'], exact=True).get_by_role('radio', name=cp['custom'], exact=True).check()
    override.get_by_role('textbox', name=f['cachePath'], exact=True).fill(str(node_cache))
    override.get_by_role('radiogroup', name=cp['limit'], exact=True).get_by_role('radio', name=cp['unlimited'], exact=True).check()
    button(override, d['apply']).click()
    expect(override).not_to_be_visible()
    expect(node_row).to_contain_text(cp['unlimited'])
    page.screenshot(path=artifacts.directory / '01-create-default-and-node-exception.png')
    button(dialog, d['next']).click()
    expect(button(dialog, f['save'])).to_be_disabled()
    page.screenshot(path=artifacts.directory / '02-create-confirm-before-probe.png')
    report['last_step'] = 'wizard-filled'
    probe_body = {'type': 'nfs_cache', 'path': str(mount), 'export': export, 'mount_options': 'vers=4.2,proto=tcp'}
    probe = mutate(page, button(dialog, f['probe']).click, 'POST', '/api/v1/storages/probe', probe_body, 200, evidence)
    check(probe['ok'] and probe['writable'] and probe['readable'], f'actual trial mount not readable+writable: {probe}')
    check(probe['node_uuid'] == node_uuid and probe.get('version') == '4.2', f'probe node/NFS version differs: {probe}')
    check(not mounted(mount), 'trial mount leaked final mount before save')
    leaked = mounts_under(owned['agent_state'])
    check(not leaked, f'the trial mount was left mounted: {leaked}')
    body = {'name': name, 'type': 'nfs_cache', 'all_nodes': True,
            'targets': [{'node_uuid': node_uuid, 'override_only': True, 'cache_path': str(node_cache), 'cache_max_bytes': 0}],
            'path': str(mount), 'export': export, 'mount_options': 'vers=4.2,proto=tcp',
            'cache_path': str(cache), 'cache_max_bytes': 8 * GIB}
    report['last_step'] = 'probe-ok'
    try:
        created = mutate(page, button(dialog, f['save']).click, 'POST', '/api/v1/storages', body, 201, evidence)
    finally:
        # A 201 is owned even when mutate's body or status checks then fail.
        if owned.get('created_response'):
            owned['storage_id'] = owned['created_response'].json()['id']
    storage_id = created['id']
    check(owned['storage_id'] == storage_id, 'created storage was not registered as owned')
    path = f'/api/v1/storages/{storage_id}'
    current = until(lambda: read(page, server.base, path + '?all_nodes=1'), lambda v: v['nodes']['ready'] == 1, 'NFS agent reports ready', 60)
    saved_fields = {key: current.get(key) for key in ('name', 'type', 'all_nodes', 'path', 'export', 'mount_options', 'cache_path', 'cache_max_bytes')}
    sent_fields = {key: body[key] for key in saved_fields}
    check(saved_fields == sent_fields, f'independent GET differs from the create body: {saved_fields} != {sent_fields}')
    check(len(current['targets']) == 1 and current['targets'][0]['cache_max_bytes'] == 0 and current['targets'][0]['override_only'], 'explicit unlimited node exception lost')
    check(current['targets'][0]['cache_path'] == str(node_cache), 'node cache path lost')
    check(current['nodes']['all'][0]['cache_path'] == str(node_cache), 'agent did not use override directory')
    evidence.append({'layer': 'persistence', 'storage': current})
    report['last_step'] = 'created-and-ready'
    actual_mount = mounted(mount)
    check(len(actual_mount) == 1, f'expected exactly one mount at {mount}: {actual_mount}')
    fstype, source, superopts = actual_mount[0].split(' - ', 1)[1].split()[:3]
    check(fstype == 'nfs4' and mountinfo_path(source) == export and 'vers=4.2' in superopts.split(','),
          f'mount is not the requested NFSv4.2 export: {fstype} {source} {superopts}')
    # The probe's capacity is the export's real filesystem size, read independently here.
    statfs = root_command(owned, ['python3', '-c',
        'import os,sys; s=os.statvfs(sys.argv[1]); print(s.f_blocks*s.f_frsize)', str(mount)])
    mount_total = int(statfs.stdout.strip())
    check(probe['total_bytes'] == mount_total,
          f"probe total {probe['total_bytes']} differs from the mounted export's {mount_total}")
    marker = mount / ('e2e-' + report['run_id'].lower() + '.txt')
    owned['marker'] = marker
    content = 'owned cache browser acceptance\n'
    root_command(owned, ['python3', '-c', 'from pathlib import Path; import sys; p=Path(sys.argv[1]); f=p.open("x"); f.write(sys.argv[2]); f.close(); assert p.read_text()==sys.argv[2]', str(marker), content])
    check(remove_marker(owned, mount), 'owned export marker was not removed')
    evidence.append({'layer': 'execution', 'mountinfo': actual_mount, 'file_roundtrip': True, 'cache_path': str(node_cache), 'runtime_driver': 'none'})
    report['last_step'] = 'mount-verified'
    page.reload()
    button(page, t['actions']['edit']).click()
    edit = page.get_by_role('dialog', name=f['editTitle'].replace('{{name}}', name), exact=True)
    expect(edit.get_by_role('spinbutton', name=f['cacheMax'], exact=True)).to_have_value('8')
    expect(edit.get_by_role('row').filter(has_text='Cache live node')).to_contain_text(cp['unlimited'])
    page.screenshot(path=artifacts.directory / '03-edit-persisted-node-exception.png')
    button(edit, f['cancel']).click()
    page.get_by_role('tab', name=t['detail']['tab']['nodes'].replace('{{count}}', '1'), exact=True).click()
    expect(page.get_by_role('link', name='Cache live node', exact=True)).to_be_visible()
    live_row = page.get_by_role('row').filter(has_text='Cache live node')
    page.screenshot(path=artifacts.directory / '04-live-node-ready.png')
    report['last_step'] = 'edit-prefill-verified'
    # A ready mount precedes the agent's first cache-directory statfs report.
    # Wait for the real controller to consume it and settle the cache budget.
    settled = until(lambda: read(page, server.base, path + '?all_nodes=1'),
        lambda value: any(node['node_uuid'] == node_uuid and node.get('cache', {}).get('state') == 'satisfied'
                          for node in value['nodes'].get('all', [])),
        'real cache capacity and budget converge to satisfied', 90)
    settled_node = next(node for node in settled['nodes']['all'] if node['node_uuid'] == node_uuid)
    check(settled_node['cache_path'] == str(node_cache), 'capacity report belongs to another cache directory')
    # The reported directory capacity is the node cache directory's own filesystem, not the mount's.
    check(node_cache.is_dir() and not cache.exists(), 'the agent did not create only the node override directory')
    local = os.statvfs(node_cache)
    report['cache_capacity_source_distinct'] = mount_total != (local.f_blocks * local.f_frsize)
    check(settled_node.get('cache_total_bytes') == local.f_blocks * local.f_frsize,
          f"cache total {settled_node.get('cache_total_bytes')} differs from {node_cache}'s {local.f_blocks * local.f_frsize}")
    drift = abs(settled_node.get('cache_available_bytes', -1) - local.f_bavail * local.f_frsize)
    check(drift <= 256 * 1024 ** 2, f'cache available bytes drift {drift} from statvfs exceeds 256 MiB')
    check(settled_node['cache']['limit_bytes'] == 0 and settled_node['cache']['used_bytes'] == 0,
          'settled budget differs from the empty unlimited cache')
    evidence.append({'layer': 'execution', 'cache_budget_converged': True, 'node': settled_node})
    page.reload()
    page.get_by_role('tab', name=t['detail']['tab']['nodes'].replace('{{count}}', '1'), exact=True).click()
    live_row = page.get_by_role('row').filter(has_text='Cache live node')
    expect(live_row).to_contain_text(t['cacheManage']['state']['satisfied'])
    expect(live_row).not_to_contain_text(t['cacheManage']['unverified'])
    # The cache-disk bar's figure carries the node cache directory's real total (statvfs above).
    cache_bar = live_row.get_by_role('progressbar', name=t['nodes']['cacheAria'].replace('{{node}}', 'Cache live node'), exact=True)
    expect(cache_bar).to_be_visible()
    expect(cache_bar.locator('xpath=..')).to_contain_text(format_bytes(local.f_blocks * local.f_frsize))
    page.goto(server.base + '/storage')
    listed = page.get_by_role('row').filter(has=page.get_by_text(name, exact=True))
    expect(listed).to_have_count(1)
    expect(listed).to_contain_text(common['storageType']['nfs_cache'])
    page.screenshot(path=artifacts.directory / '05-live-cache-budget-satisfied.png')
    report['last_step'] = 'cache-budget-satisfied'
    # Cleanup through real API, intentionally not claimed as UI deletion coverage.
    response = page.request.delete(server.base + path)
    check(response.status == 204, 'owned storage cleanup rejected')
    until(lambda: mounted(mount), lambda rows: not rows, 'agent unmount after deletion', 40)
    check(page.request.get(server.base + path).status == 404, 'storage still registered after deletion')
    owned['storage_id'] = None
    evidence.append({'layer': 'ui', 'checks': ['nfs-cache-type-selected', 'four-step-navigation', 'list-row-after-create', 'node-cache-exception-applied', 'save-disabled-before-probe', 'edit-roundtrip-after-refresh', 'node-tab-ready', 'cache-state-satisfied']})
    evidence.append({'layer': 'cleanup', 'storage_deleted': True, 'mount_removed': True})


def format_bytes(value):
    """ui/src/format.ts formatBytes with default options: one decimal from KiB up."""
    units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
    if not value:
        return '0 B'
    unit, number = 0, float(value)
    while number >= 1024 and unit < len(units) - 1:
        number /= 1024
        unit += 1
    return f"{number:.0f} B" if unit == 0 else f"{number:.1f} {units[unit]}"


def mounts_under(root):
    """Mountpoints at or below root, from /proc/self/mountinfo."""
    prefix = str(root).rstrip('/') + '/'
    paths = [mountinfo_path(line.split()[4]) for line in Path('/proc/self/mountinfo').read_text().splitlines()]
    return [path for path in paths if path == str(root) or path.startswith(prefix)]


def remove_marker(owned, mount):
    marker = owned.get('marker')
    if marker is None:
        return True
    if any(command_alive(command) for command in owned.get('nfs_commands', [])):
        return False  # Never race an earlier writer that may still finish its syscall.
    if not mounted(mount):
        return False  # The remote file cannot be checked through an unmounted directory.
    root_command(owned, ['python3', '-c',
                    'from pathlib import Path; import sys; p=Path(sys.argv[1]); p.unlink(missing_ok=True); assert not p.exists()',
                    str(marker)])
    owned['marker'] = None
    return True


def export_reachable(export):
    """Infrastructure gate: an NFS client and a TCP listener on the export host's port 2049."""
    check(':/' in export, f'export must be host:/path, received {export!r}')
    host = export.split(':/', 1)[0]  # mount.nfs splits at the first ':/'
    if ':' in host:
        check(host.startswith('[') and host.endswith(']'), f'an IPv6 export host must be bracketed: {export!r}')
    host = host.strip('[]')
    check(bool(host), f'export has no host: {export!r}')
    check(any(Path(d, 'mount.nfs4').exists() for d in ('/sbin', '/usr/sbin')), 'mount.nfs4 (NFS client) is not installed')
    with socket.create_connection((host, 2049), timeout=5):
        pass
    return {'host': host, 'port': 2049}


def start_agent(page, server, artifacts, report, owned, agent_bin):
    """Enroll and start one real agent whose token travels only over private stdin."""
    response = page.request.post(server.base + '/api/v1/nodes', data={'name': 'Cache live node', 'server_url': server.base})
    check(response.status == 201, 'real agent enrollment failed')
    enrollment = response.json()
    token, node_uuid = enrollment['token'], enrollment['node']['uuid']
    artifacts.remember(token)
    owned['node_uuid'] = report['node_uuid'] = node_uuid
    owned['agent_log'] = (artifacts.private / 'agent.log').open('w')
    owned['agent'] = agent = subprocess.Popen(['sudo', '-n', 'sh', '-c',
        'IFS= read -r GPUWORKS_TOKEN; export GPUWORKS_TOKEN; printf "%s\\n" "$$" > "$1"; chmod 0644 "$1"; shift; exec "$@"',
        'owned-cache-agent', str(owned['agent_pidfile']), str(agent_bin.resolve()), 'run', '--server', server.base,
        '--state', str(owned['agent_state']), '--driver', 'none'],
        stdin=subprocess.PIPE, stdout=owned['agent_log'], stderr=subprocess.STDOUT, text=True)
    agent.stdin.write(token + '\n')
    agent.stdin.close()
    current = until(lambda: read(page, server.base, '/api/v1/nodes/' + node_uuid), lambda n: n.get('state') == 'online', 'real agent connection')
    check(not current.get('runtime_driver'), 'agent must not acquire a shared container runtime')
    return node_uuid


def owned_agent_pid(owned):
    """The pid the agent wrapper recorded, only while it still runs this run's agent."""
    pidfile = owned['agent_pidfile']
    if not pidfile.exists():
        return None
    pid = pidfile.read_text().strip()
    try:
        argv = Path(f'/proc/{pid}/cmdline').read_bytes().split(b'\0')
    except OSError:
        return None
    state = str(owned['agent_state']).encode()
    return pid if b'run' in argv and state in argv else None


def stop_agent(owned):
    """Stop the owned agent (TERM, then KILL) and close its log; returns whether it exited."""
    agent = owned.get('agent')
    if agent and agent.poll() is None:
        for signal, wait in (('-TERM', 15), ('-KILL', 5)):
            pid = owned_agent_pid(owned)
            if pid:
                subprocess.run(['sudo', '-n', 'kill', signal, pid], capture_output=True)
            try:
                agent.wait(timeout=wait)
                break
            except subprocess.TimeoutExpired:
                continue
    if owned.get('agent_log'):
        owned['agent_log'].close()
    return agent is None or agent.poll() is not None


def release_owned(report, owned, server, mount, root_owned):
    """Release every owned resource in reverse dependency order; each stage runs even if
    an earlier one fails, and PASS survives only when all are observed released."""
    cleanup_stage(report, 'export-marker', lambda: remove_marker(owned, mount))
    report['agent_stopped'] = cleanup_stage(report, 'agent', lambda: stop_agent(owned))

    def unmount():
        # The final mount and any trial mount the agent left under its state directory.
        for target in [str(mount)] * bool(mounted(mount)) + mounts_under(owned['agent_state']):
            result = root_command(owned, ['umount', target])
            report.setdefault('fallback_unmount', []).append({'path': target, 'returncode': result.returncode, 'stderr': result.stderr.strip()})
        return not mounted(mount) and not mounts_under(owned['agent_state'])
    report['mount_removed'] = cleanup_stage(report, 'mount', unmount)
    report['server_stopped'] = cleanup_stage(report, 'server', server.stop)
    remaining = []
    for command in owned.get('nfs_commands', []):
        if command_alive(command):
            remaining.append({'kind': 'nfs-command', **command,
                              'reclaim': f'sudo kill -KILL -- -{command["pgid"]}'})
    if owned.get('marker') is not None:
        remaining.append({'kind': 'export-file', 'path': str(owned['marker']),
                          'reclaim': 'remove this exact filename from the caller-owned NFS export: ' + owned['marker'].name})
    if owned['storage_id'] is not None:
        remaining.append({'kind': 'storage', 'id': owned['storage_id'], 'scope': 'isolated server database in private/',
                          'reclaim': 'discarded with this run directory'})
    for target in [str(mount)] * bool(mounted(mount)) + mounts_under(owned['agent_state']):
        remaining.append({'kind': 'mount', 'path': target, 'reclaim': 'sudo umount ' + target})
    if not report['agent_stopped']:
        remaining.append({'kind': 'process', 'pidfile': str(owned['agent_pidfile']),
                          'reclaim': f"sudo kill $(cat {owned['agent_pidfile']})"})
    if not report['server_stopped']:
        remaining.append({'kind': 'process', 'pid': server.process.pid if server.process else None,
                          'reclaim': f"kill {server.process.pid}" if server.process else 'none: never started'})
    report['remaining_resources'] = remaining
    # The agent writes its state (including its node token) and the cache directories as root.
    report['root_owned_paths'] = [str(p) for p in (owned['agent_state'], *root_owned) if p.exists()]
    report['private_state'] = ('private/ retains the isolated DB, logs and root-owned agent state/cache directories; '
                               'remove only this run directory with sudo after diagnosis (7 days)')
    if report.get('status') == 'PASS' and (remaining or any(v != 'ok' for v in report['cleanup_stages'].values())):
        report['status'] = 'FAIL_TEST'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--server-bin', type=Path, default=ROOT / 'bin/gpuworks')
    parser.add_argument('--agent-bin', type=Path, default=ROOT / 'bin/gpuworks-agent')
    parser.add_argument('--export', required=True, help='owned NFS export, host:/path')
    parser.add_argument('--artifact-dir', type=Path)
    parser.add_argument('--locale', choices=['en', 'zh'], default='en')
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ') + '-' + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / 'secrets/e2e' / run_id)
    manifest_path = ROOT / 'test/e2e/coverage/storage-nfs-cache-create.json'
    manifest = json.loads(manifest_path.read_text())
    bound = manifest['contract_id'] == CONTRACT and manifest['test_function'] == test_storage_cache_create.__name__
    report = {'contract': CONTRACT, 'case': CASE, 'run_id': run_id, 'status': 'NOT_RUN',
              'source_sha': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip(),
              'working_tree': 'dirty' if subprocess.check_output(['git', 'status', '--porcelain'], cwd=ROOT) else 'clean',
              'target_branch': 'wei/master',
              'target_sha': subprocess.check_output(['git', 'rev-parse', 'wei/master'], cwd=ROOT, text=True).strip(),
              'lane': 'local-real-server-agent-nfs', 'identity': 'admin', 'locale': args.locale,
              'timezone': 'UTC', 'viewport': VIEWPORT, 'selection': [CONTRACT], 'retries': 0, 'evidence': [],
              'coverage_manifest_sha256': digest(manifest_path), 'test_sha256': digest(Path(__file__)),
              'helper_sha256': digest(ROOT / 'test/e2e/local_browser_fixture.py'),
              'runtime': 'N/A: driver=none; no container, GPU or engine',
              'limits': ['Single local node, no container or GPU runtime.', 'NFS export lifecycle owned by caller.'],
              'command': ['python3', 'test/e2e/storage_cache_create.py', *sys.argv[1:]]}
    server = LocalServer(args.server_bin, artifacts.private)
    owned = {'agent_pidfile': artifacts.private / 'agent.pid', 'agent_state': artifacts.private / 'agent-state', 'storage_id': None}
    mount = server.storage_root / 'nfs-mount'
    cache = server.storage_root / 'cache'
    node_cache = server.storage_root / 'node-cache'
    if not bound:
        report['status'], report['failure'] = 'FAIL_TEST', 'coverage manifest does not bind this case'
        artifacts.write_json('report.json', report)
        print(json.dumps({'status': report['status'], 'contract': CONTRACT, 'run_id': run_id}, ensure_ascii=False))
        return 1
    try:
        report['binary_sha256'] = digest(args.server_bin)
        report['agent_sha256'] = digest(args.agent_bin)
        sudo = subprocess.run(['sudo', '-n', 'true'], capture_output=True, text=True)
        check(sudo.returncode == 0, 'sudo -n is required to run the agent and mount NFS: ' + sudo.stderr.strip())
        report['export_reachable'] = export_reachable(args.export)
        report['health'] = server.start()
        # sudo, the NFS client, the export and server health are infrastructure; from
        # here on login, storage setup, enrollment and the agent are product behavior.
        report['preflight'] = 'PASS'
        report['base_url'] = server.base
        report['server_environment'] = server.environment
        with sync_playwright() as play:
            browser = play.chromium.launch(channel='chrome', headless=True)
            report['browser_version'] = browser.version
            context = browser.new_context(viewport=VIEWPORT, locale='zh-CN' if args.locale == 'zh' else 'en-US', timezone_id='UTC')
            page = context.new_page()
            page.set_default_timeout(20000)
            try:
                login(page, server, artifacts, args.locale)
                errors = observe_browser(page, report)
                # Fixture setup uses real management APIs; all storage under test mutations use UI.
                # The empty-cache scenario must not depend on the workstation having
                # the production default's 50 GiB reserve free on its cache disk.
                reserve = 1024 ** 2
                response = page.request.patch(server.base + '/api/v1/storages/settings', data={'reserve_free_bytes': reserve})
                check(response.status == 200, 'could not set the isolated fixture disk reserve')
                check(read(page, server.base, '/api/v1/storages/settings')['reserve_free_bytes'] == reserve,
                      'fixture disk reserve was not persisted')
                report['fixture_reserve_free_bytes'] = reserve
                for item in read(page, server.base, '/api/v1/storages')['items']:
                    check(item['type'] == 'local', 'fresh fixture unexpectedly contains network storage')
                    response = page.request.patch(server.base + f"/api/v1/storages/{item['id']}", data={'path': str(server.storage_root / 'local-models')})
                    check(response.status == 200, 'could not isolate default local storage path')
                node_uuid = start_agent(page, server, artifacts, report, owned, args.agent_bin)
                artifacts.start_trace(context)
                test_storage_cache_create(page, server, artifacts, report, owned, args.export, node_uuid,
                                          mount, cache, node_cache, args.locale)
                check_browser(errors)
                artifacts.stop_trace(context)
                layers = {item['layer'] for item in report['evidence']}
                check(set(manifest['required_evidence']) <= layers, 'required evidence is missing')
                report['status'] = 'PASS'
            except Exception:
                report['status'] = 'FAIL_TEST' if report.get('preflight') == 'PASS' else 'BLOCKED_INFRA'
                report['failure'] = artifacts.redact(traceback.format_exc())
                capture(page, artifacts, 'failure.png')
            finally:
                cleanup_stage(report, 'trace', lambda: artifacts.stop_trace(context))
                cleanup_stage(report, 'context', context.close)
                cleanup_stage(report, 'browser', browser.close)
    except Exception:
        report['status'] = 'FAIL_TEST' if report.get('preflight') == 'PASS' else 'BLOCKED_INFRA'
        report['failure'] = artifacts.redact(traceback.format_exc())
    finally:
        release_owned(report, owned, server, mount, (cache, node_cache))
        for name in ('server.log', 'agent.log'):
            cleanup_stage(report, name, lambda name=name: copy_redacted(artifacts, name))
        if report['status'] == 'PASS' and any(v != 'ok' for v in report['cleanup_stages'].values()):
            report['status'] = 'FAIL_TEST'
        artifacts.write_json('report.json', report)
        try:
            artifacts.scan()
        except Exception:
            report['status'] = 'FAIL_TEST'
            report['artifact_error'] = 'credential scan rejected artifacts; private directory must not be uploaded'
            artifacts.write_json('report.json', report)
    print(json.dumps({'status': report['status'], 'contract': CONTRACT, 'run_id': run_id,
                      'report': str(artifacts.directory / 'report.json')}, ensure_ascii=False))
    return 0 if report['status'] == 'PASS' else 1


if __name__ == '__main__':
    raise SystemExit(main())
