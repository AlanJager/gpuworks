"""Cleanup-ledger and PASS-aggregation tests for the NFS cache browser cases.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: PYTHONPATH=test/e2e python3 -m unittest test_storage_cache_cleanup -v
These are infrastructure tests, not browser coverage.
"""

import os
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

import storage_cache_create as case
import storage_cache_policy as policy
from local_browser_fixture import Artifacts
from storage_cache_policy import cleanup_stage, copy_redacted


class FakeServer:
    def __init__(self, stops=True):
        self.stops = stops
        self.process = mock.Mock(pid=4242)

    def stop(self):
        return self.stops


class CleanupStageTest(unittest.TestCase):
    # Mainline: a raising stage and an incomplete stage are both recorded; the next stage still runs.
    def test_stage_failures_are_recorded_and_isolated(self):
        report, ran = {}, []
        self.assertFalse(cleanup_stage(report, "first", lambda: (_ for _ in ()).throw(RuntimeError("boom"))))
        self.assertFalse(cleanup_stage(report, "second", lambda: False))
        self.assertTrue(cleanup_stage(report, "third", lambda: ran.append(1)))
        self.assertEqual(ran, [1])
        self.assertTrue(report["cleanup_stages"]["first"].startswith("error: RuntimeError: boom"))
        self.assertEqual(report["cleanup_stages"]["second"], "incomplete: False")
        self.assertEqual(report["cleanup_stages"]["third"], "ok")


class StopAgentTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        root = Path(self.directory.name)
        self.owned = {"agent_pidfile": root / "agent.pid", "agent_state": root / "agent-state"}

    def tearDown(self):
        self.directory.cleanup()

    # Mainline: a pid is signalled only while /proc shows it running this run's agent state.
    def test_pid_only_while_it_runs_this_agent(self):
        self.assertIsNone(case.owned_agent_pid(self.owned))  # no pidfile yet
        unrelated = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(30)"])
        mine = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(30)", "run", "--state",
                                 str(self.owned["agent_state"])])
        try:
            self.owned["agent_pidfile"].write_text(f"{unrelated.pid}\n")
            self.assertIsNone(case.owned_agent_pid(self.owned))
            self.owned["agent_pidfile"].write_text(f"{mine.pid}\n")
            self.assertEqual(case.owned_agent_pid(self.owned), str(mine.pid))
        finally:
            for process in (unrelated, mine):
                process.kill()
                process.wait()
        self.assertIsNone(case.owned_agent_pid(self.owned))  # exited: /proc entry gone

    # Mainline: TERM is ignored → KILL follows; the log is closed and the exit observed.
    def test_term_then_kill(self):
        agent = mock.Mock()
        agent.poll.side_effect = [None, 0]
        agent.wait.side_effect = [subprocess.TimeoutExpired("agent", 15), 0]
        log = mock.Mock()
        self.owned.update(agent=agent, agent_log=log)
        signals = []
        with mock.patch.object(case, "owned_agent_pid", return_value="4242"), \
                mock.patch.object(case.subprocess, "run", side_effect=lambda argv, **kw: signals.append(argv[3])):
            self.assertTrue(case.stop_agent(self.owned))
        self.assertEqual(signals, ["-TERM", "-KILL"])
        log.close.assert_called_once()


class CopyRedactedTest(unittest.TestCase):
    # Mainline: missing and empty logs are ok stages; a present log is copied redacted.
    def test_missing_empty_and_present_logs(self):
        with tempfile.TemporaryDirectory() as directory:
            artifacts = Artifacts(Path(directory) / "run")
            report = {}
            self.assertTrue(cleanup_stage(report, "missing", lambda: copy_redacted(artifacts, "server.log")))
            (artifacts.private / "server.log").write_text("")
            self.assertTrue(cleanup_stage(report, "empty", lambda: copy_redacted(artifacts, "server.log")))
            artifacts.remember("synthetic-secret-value")
            (artifacts.private / "server.log").write_text("token=synthetic-secret-value\n")
            self.assertTrue(cleanup_stage(report, "present", lambda: copy_redacted(artifacts, "server.log")))
            copied = (artifacts.directory / "server.log").read_text()
            self.assertNotIn("synthetic-secret-value", copied)
            self.assertEqual(set(report["cleanup_stages"].values()), {"ok"})


class ReleaseOwnedTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        root = Path(self.directory.name)
        self.mount = root / "nfs-mount"
        self.owned = {"agent_pidfile": root / "agent.pid", "agent_state": root / "agent-state", "storage_id": None}
        self.unmounted = []

    def tearDown(self):
        self.directory.cleanup()

    def release(self, report, server, busy, agent_error=None, umount_works=True):
        """busy: whether the final mount is present before cleanup; umount_works: whether umount removes it."""
        stop_agent = mock.Mock(side_effect=agent_error) if agent_error else mock.Mock(return_value=True)
        state = {"mounted": busy}
        umounts = []

        def umount(argv, **kwargs):
            umounts.append(argv)
            if umount_works:
                state["mounted"] = False
                return mock.Mock(returncode=0, stderr="")
            return mock.Mock(returncode=32, stderr="target is busy")
        with mock.patch.object(case, "stop_agent", stop_agent), \
                mock.patch.object(case, "mounted", side_effect=lambda path: ["mountinfo line"] if state["mounted"] else []), \
                mock.patch.object(case, "mounts_under", return_value=[]), \
                mock.patch.object(case, "root_command", side_effect=lambda owned, argv: umount(argv)):
            case.release_owned(report, self.owned, server, self.mount, ())
        return umounts

    # Mainline: everything released → PASS kept, empty ledger.
    def test_clean_release_keeps_pass(self):
        report = {"status": "PASS"}
        self.assertEqual(self.release(report, FakeServer(), busy=False), [])
        self.assertEqual(report["status"], "PASS")
        self.assertEqual(report["remaining_resources"], [])

    # Mainline: agent stop raises, the mount lingers after umount and the server will not stop →
    # every stage still runs, PASS is withdrawn and each residue keeps its scoped reclaim entry.
    def test_failed_stages_withdraw_pass_and_keep_residue(self):
        report = {"status": "PASS"}
        self.owned["storage_id"] = 7
        umounts = self.release(report, FakeServer(stops=False), busy=True, agent_error=PermissionError("pidfile"),
                               umount_works=False)
        self.assertEqual(umounts, [["umount", str(self.mount)]])
        self.assertEqual(report["status"], "FAIL_TEST")
        self.assertFalse(report["agent_stopped"] or report["mount_removed"] or report["server_stopped"])
        self.assertTrue(report["cleanup_stages"]["agent"].startswith("error: PermissionError"))
        kinds = [(item["kind"], item.get("id") or item.get("path") or item.get("pidfile") or item.get("pid"))
                 for item in report["remaining_resources"]]
        self.assertEqual(kinds, [("storage", 7), ("mount", str(self.mount)),
                                 ("process", str(self.owned["agent_pidfile"])), ("process", 4242)])

    # Mainline: an earlier failure is never turned into PASS by a clean release.
    def test_clean_release_does_not_upgrade_failure(self):
        report = {"status": "FAIL_TEST"}
        self.release(report, FakeServer(), busy=False)
        self.assertEqual(report["status"], "FAIL_TEST")

    # Mainline: a lingering mount that the fallback umount removes is released, recorded, and not residue.
    def test_fallback_umount_releases_a_lingering_mount(self):
        report = {"status": "PASS"}
        umounts = self.release(report, FakeServer(), busy=True)
        self.assertEqual(umounts, [["umount", str(self.mount)]])
        self.assertTrue(report["mount_removed"])
        self.assertEqual(report["fallback_unmount"][0]["path"], str(self.mount))
        self.assertEqual(report["remaining_resources"], [])

    # Mainline: a timed-out NFS umount retains the mount and still stops the server.
    def test_umount_timeout_keeps_residue_and_continues(self):
        report = {"status": "PASS"}
        server = FakeServer()
        with mock.patch.object(case, "stop_agent", return_value=True), \
                mock.patch.object(case, "mounted", return_value=["mounted"]), \
                mock.patch.object(case, "mounts_under", return_value=[]), \
                mock.patch.object(case, "root_command", side_effect=subprocess.TimeoutExpired("umount", 15)):
            case.release_owned(report, self.owned, server, self.mount, ())
        self.assertEqual(report["status"], "FAIL_TEST")
        self.assertTrue(report["server_stopped"])
        self.assertEqual(report["remaining_resources"][0]["path"], str(self.mount))

    # Mainline: a file that cannot be deleted remains owned after the rest of cleanup.
    def test_export_marker_failure_is_not_lost(self):
        report = {"status": "FAIL_TEST"}
        marker = self.mount / "e2e-owned.txt"
        self.owned["marker"] = marker
        with mock.patch.object(case, "remove_marker", side_effect=PermissionError("export")), \
                mock.patch.object(case, "stop_agent", return_value=True), \
                mock.patch.object(case, "mounted", return_value=[]), \
                mock.patch.object(case, "mounts_under", return_value=[]):
            case.release_owned(report, self.owned, FakeServer(), self.mount, ())
        self.assertEqual(report["remaining_resources"][0]["kind"], "export-file")
        self.assertEqual(report["remaining_resources"][0]["path"], str(marker))
        self.assertTrue(report["server_stopped"])


class MountinfoTest(unittest.TestCase):
    # Mainline: kernel octal escapes never hide owned mounts with spaces or backslashes.
    def test_escaped_paths_are_observed(self):
        root = Path('/tmp/e2e run\\owned')
        mount = root / 'nfs-mount'
        escaped = str(mount).replace('\\', r'\134').replace(' ', r'\040')
        line = f'1 0 0:1 / {escaped} rw - nfs4 localhost:/export rw,vers=4.2\n'
        with mock.patch.object(Path, 'read_text', return_value=line):
            self.assertEqual(case.mounted(mount), [line.strip()])
            self.assertEqual(case.mounts_under(root), [str(mount)])
        self.assertEqual(case.mountinfo_path(r'/tab\011line\012slash\134040'), '/tab\tline\nslash\\040')


class RootWatchdogTest(unittest.TestCase):
    # Mainline: a reused PID cannot revive an exited command or yield an unrelated PGID.
    def test_reused_pid_is_never_reclaimed(self):
        command = {'pid': 99, 'starttime': '123', 'active': True}
        fields = ['S', '1', '88', *(['0'] * 16), '456']
        with mock.patch.object(Path, 'read_text', return_value='99 (python3) ' + ' '.join(fields)):
            self.assertFalse(case.command_alive(command))
        self.assertNotIn('pgid', command)
        with mock.patch.object(Path, 'read_text', side_effect=AssertionError('must not revisit exited PID')):
            self.assertFalse(case.command_alive(command))

    # Mainline: the real root watchdog kills a sleeping writer, not just sudo;
    # its child is observed exited and cannot recreate a cleaned marker later.
    def test_root_writer_does_not_survive_timeout(self):
        try:
            sudo = subprocess.run(['sudo', '-n', 'true'], capture_output=True, timeout=5)
        except FileNotFoundError:
            sudo = None
        if sudo is None or sudo.returncode != 0:
            if os.environ.get('GPUWORKS_TEST_STRICT') == '1':
                self.fail('sudo -n is required for the root watchdog test')
            self.skipTest('sudo -n is required for the root watchdog test')
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            owned = {'agent_state': root / 'agent-state'}
            marker = root / 'late-marker'
            try:
                with self.assertRaises(subprocess.CalledProcessError):
                    case.root_command(owned, ['python3', '-c',
                        'import time,sys; from pathlib import Path; time.sleep(2); Path(sys.argv[1]).write_text("late")',
                        str(marker)], timeout=.5)
                self.assertIsNotNone(owned['nfs_commands'][0]['pid'])
                self.assertFalse(case.command_alive(owned['nfs_commands'][0]))
                self.assertFalse(marker.exists())
            finally:
                subprocess.run(['sudo', '-n', 'rm', '-f', *[c['pidfile'] for c in owned['nfs_commands']], str(marker)],
                               check=True, timeout=5)


class MutationDiagnosticTest(unittest.TestCase):
    # Mainline: a mismatched wire body is retained with expected/actual and method/path/status.
    def test_body_failure_keeps_observed_request(self):
        page = mock.MagicMock()
        response = mock.Mock(status=200, url='http://localhost/api/v1/storages/7')
        response.request.post_data, response.request.post_data_json = '{"cache_max_bytes":999}', {'cache_max_bytes': 999}
        response.text.return_value = '{}'
        page.expect_response.return_value.__enter__.return_value.value = response
        evidence = []
        with self.assertRaisesRegex(AssertionError, 'expected body.*received'):
            policy.mutate(page, lambda: None, 'PATCH', '/api/v1/storages/7', {'cache_max_bytes': 0}, 200, evidence)
        self.assertEqual(evidence[0]['body'], {'cache_max_bytes': 999})
        self.assertEqual(evidence[0]['expected_body'], {'cache_max_bytes': 0})
        self.assertEqual(evidence[0]['status'], 200)
        page.remove_listener.assert_called_once()


class MutationScalarTypeTest(unittest.TestCase):
    # JSON booleans and fractional tokens must not compare equal to integer capacities.
    def test_integer_wire_is_not_boolean_or_float(self):
        for actual in (False, 0.0):
            with self.subTest(actual=actual, kind=type(actual).__name__):
                page = mock.MagicMock()
                response = mock.Mock(status=200, url='http://localhost/api/v1/storages/7')
                response.request.post_data = json.dumps({'cache_max_bytes': actual})
                response.request.post_data_json = {'cache_max_bytes': actual}
                response.text.return_value = '{}'
                page.expect_response.return_value.__enter__.return_value.value = response
                evidence = []
                with self.assertRaisesRegex(AssertionError, 'expected body.*received'):
                    policy.mutate(page, lambda: None, 'PATCH', '/api/v1/storages/7',
                                  {'cache_max_bytes': 0}, 200, evidence)
                self.assertIs(type(evidence[0]['body']['cache_max_bytes']), type(actual))
                page.remove_listener.assert_called_once()


class EntryCloseFailureTest(unittest.TestCase):
    # Mainline: the actual entry reaches business success, then context/browser close fails;
    # exit is nonzero, the report stays FAIL, and the server is still stopped.
    def test_close_failures_withdraw_pass_in_both_entries(self):
        for module in (policy, case):
            for stage in (None, 'context', 'browser'):
                with self.subTest(module=module.__name__, stage=stage), tempfile.TemporaryDirectory() as directory:
                    root = Path(directory)
                    server = mock.Mock(base='http://127.0.0.1:8800', storage_root=root / 'storage',
                                       environment={}, process=mock.Mock(pid=4242))
                    server.start.return_value = {'ok': True}
                    server.stop.return_value = True
                    browser, context = mock.Mock(version='test-browser'), mock.Mock()
                    browser.new_context.return_value = context
                    context.new_page.return_value.request.patch.return_value.status = 200
                    browser.close.return_value = context.close.return_value = None
                    if stage:
                        getattr(context if stage == 'context' else browser, 'close').side_effect = RuntimeError('injected-close')
                    play = mock.MagicMock()
                    play.__enter__.return_value.chromium.launch.return_value = browser
                    function = module.test_storage_cache_policy if module is policy else module.test_storage_cache_create

                    def business(page, server, artifacts, report, *args):
                        report['last_step'] = 'business-complete'
                        report['evidence'] = [{'layer': layer} for layer in ('ui', 'request', 'persistence', 'execution', 'cleanup')]
                    business.__name__ = function.__name__
                    argv = ['case', '--server-bin', __file__, '--artifact-dir', str(root / 'run')]
                    if module is case:
                        argv += ['--agent-bin', __file__, '--export', 'localhost:/owned']
                    with mock.patch.object(sys, 'argv', argv), \
                            mock.patch.object(module, 'LocalServer', return_value=server), \
                            mock.patch.object(module, 'sync_playwright', return_value=play), \
                            mock.patch.object(module, 'login'), \
                            mock.patch.object(module, function.__name__, business), \
                            mock.patch.object(Artifacts, 'start_trace', return_value=None), \
                            mock.patch.object(Artifacts, 'stop_trace', return_value=None):
                        if module is policy:
                            with mock.patch.object(policy, 'seed', return_value=7), mock.patch.object(policy, 'viewer_read_only'):
                                code = module.main()
                        else:
                            real_run = subprocess.run
                            def run(argv, **kwargs):
                                return mock.Mock(returncode=0, stdout='', stderr='') if argv[:2] == ['sudo', '-n'] else real_run(argv, **kwargs)
                            with mock.patch.object(case, 'export_reachable', return_value={}), \
                                    mock.patch.object(case, 'read', return_value={'items': [], 'reserve_free_bytes': 1024 ** 2}), \
                                    mock.patch.object(case, 'start_agent', return_value='owned-node'), \
                                    mock.patch.object(case.subprocess, 'run', side_effect=run):
                                code = module.main()
                    report = json.loads((root / 'run/report.json').read_text())
                    self.assertEqual(code, 1 if stage else 0)
                    self.assertEqual(report['status'], 'FAIL_TEST' if stage else 'PASS', report.get('failure'))
                    self.assertEqual(report['last_step'], 'business-complete')
                    for name, outcome in report['cleanup_stages'].items():
                        if name == stage:
                            self.assertIn('injected-close', outcome)
                        else:
                            self.assertEqual(outcome, 'ok', name)
                    self.assertTrue(report['server_stopped'])
                    self.assertEqual(report['remaining_resources'], [])


if __name__ == "__main__":
    unittest.main()
