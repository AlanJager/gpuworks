"""python3 -B -m unittest discover -s test/e2e -p test_local_business_runner.py"""
import contextlib
import io
import json
from pathlib import Path
import subprocess
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import local_business_runner as runner


class LifecycleTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.artifact = self.root / 'run'
        self.binary = self.root / 'server'
        self.binary.write_bytes(b'audited binary bytes')
        self.args = SimpleNamespace(server_bin=self.binary, agent_bin=self.binary,
            locale='en', mutation=None, artifact_dir=self.artifact)
        self.manifest = {'contract_id': 'gpuworks/test/runner', 'name': 'runner fixture only',
            'lane': 'local-server-fixture', 'required_evidence': ['ui', 'request', 'cleanup']}
        self.server = Mock(process=SimpleNamespace(pid=123), base='http://127.0.0.1:8800')
        self.server.start.return_value = {'status': 'ok'}
        self.server.stop.return_value = True
        self.browser, self.context, self.page = Mock(), Mock(), Mock()
        self.browser.new_context.return_value = self.context
        self.context.new_page.return_value = self.page
        self.browser.version = 'synthetic test browser'
        self.play = Mock()
        self.play.__enter__ = Mock(return_value=self.play)
        self.play.__exit__ = Mock(return_value=False)
        self.play.chromium.launch.return_value = self.browser
        self.prepare = Mock(return_value={})
        self.cleanup = Mock(return_value=True)

    def exercise(self, page, context, server, artifacts, report, locale, data, token, holder):
        report['evidence'].extend([{'layer': 'ui'}, {'layer': 'request'}])

    def run_case(self, exercise=None):
        with patch.object(runner, 'source_identity', return_value=(self.manifest,
                {'source_sha': 'a' * 40, 'working_tree': 'clean'})), \
             patch.object(runner, 'LocalServer', return_value=self.server), \
             patch.object(runner, 'http', return_value=(200, {'token': 'test-token-secret-long'})), \
             patch('playwright.sync_api.sync_playwright', return_value=self.play), \
             contextlib.redirect_stdout(io.StringIO()):
            code = runner.run_case(self.args, contract_id=self.manifest['contract_id'],
                manifest='not-read-by-this-unit.json', test_file=__file__, prepare=self.prepare,
                exercise=exercise or self.exercise, cleanup=self.cleanup, docker_required=False)
        report = json.loads((self.artifact / 'report.json').read_text())
        return code, report

    def test_success_requires_all_boundaries_and_no_residuals(self):
        code, report = self.run_case()
        self.assertEqual((code, report['status'], report['artifact_scan']), (0, 'PASS', 'PASS'))
        self.assertTrue(report['environment_cleanup_ok'])
        self.assertEqual(report['remaining_resources'], [])
        self.server.stop.assert_called_once()
        self.context.close.assert_called_once()
        self.browser.close.assert_called_once()

    def test_browser_close_exception_stays_failed_and_server_cleanup_runs(self):
        self.context.close.side_effect = RuntimeError('cannot close context')
        code, report = self.run_case()
        self.assertEqual((code, report['status'], report['browser_close_ok']), (1, 'FAIL_TEST', False))
        self.browser.close.assert_called_once()
        self.cleanup.assert_called_once()
        self.server.stop.assert_called_once()

    def test_domain_cleanup_exception_retains_owned_ids_and_closes_processes(self):
        def prepare(server, token, run_id, report, holder, agent_bin, artifacts):
            report['owned_deployment_id'] = 'owned-dep-1'
            return {}
        self.prepare.side_effect = prepare
        self.cleanup.side_effect = TimeoutError('delete timed out')
        code, report = self.run_case()
        self.assertEqual((code, report['status']), (1, 'FAIL_TEST'))
        self.assertEqual(report['remaining_resources'][0]['owned']['owned_deployment_id'], 'owned-dep-1')
        self.context.close.assert_called_once()
        self.server.stop.assert_called_once()

    def test_prepare_partial_failure_cleanup_still_runs_and_never_passes(self):
        self.prepare.side_effect = RuntimeError('partial fixture creation')
        code, report = self.run_case()
        self.assertEqual((code, report['status']), (1, 'FAIL_TEST'))
        self.assertFalse(report['browser_close_ok'])
        self.cleanup.assert_called_once()
        self.server.stop.assert_called_once()
        self.play.chromium.launch.assert_not_called()

    def test_preflight_failure_is_blocked_and_no_browser_or_prepare(self):
        self.args.server_bin = self.root / 'missing'
        code, report = self.run_case()
        self.assertEqual((code, report['status']), (1, 'BLOCKED_INFRA'))
        self.prepare.assert_not_called()
        self.cleanup.assert_called_once()
        self.server.stop.assert_called_once()

    def test_missing_evidence_is_failure_despite_normal_callback_return(self):
        code, report = self.run_case(exercise=lambda *args: None)
        self.assertEqual((code, report['status']), (1, 'FAIL_TEST'))
        self.assertIn('required evidence', report['failure'])

    def test_false_cleanup_or_hidden_failed_stage_cannot_pass(self):
        def cleanup(server, token, report, holder):
            report['cleanup_steps'].append({'stage': 'fixture listener', 'ok': False})
            return True
        self.cleanup.side_effect = cleanup
        code, report = self.run_case()
        self.assertEqual((code, report['status']), (1, 'FAIL_TEST'))

    def test_server_stop_false_preserves_residual_pid(self):
        self.server.stop.return_value = False
        code, report = self.run_case()
        self.assertEqual((code, report['status']), (1, 'FAIL_TEST'))
        self.assertEqual(report['remaining_resources'][0]['id'], 123)

    def test_secret_serialization_and_actual_scanner_reject_leaking_file(self):
        secret = 'this-fixture-credential-must-not-leak'
        def exercise(page, context, server, artifacts, report, *rest):
            artifacts.remember(secret)
            report['evidence'].extend([{'layer': 'ui'}, {'layer': 'request'}])
            report['diagnostic'] = secret
            (artifacts.directory / 'bad.txt').write_text(secret)
        code, report = self.run_case(exercise=exercise)
        self.assertEqual((code, report['status'], report['artifact_scan']), (1, 'FAIL_TEST', 'FAIL'))
        self.assertNotIn(secret, (self.artifact / 'report.json').read_text())
        self.assertEqual(report['diagnostic'], '[REDACTED]')

    def test_existing_failure_is_sticky_even_with_all_evidence(self):
        def exercise(page, context, server, artifacts, report, *rest):
            report['evidence'].extend([{'layer': 'ui'}, {'layer': 'request'}])
            report.update(status='FAIL_TEST', failure='prior assertion failed')
        code, report = self.run_case(exercise=exercise)
        self.assertEqual((code, report['status']), (1, 'FAIL_TEST'))

    def test_agent_stop_timeout_kills_only_owned_agent(self):
        agent = Mock()
        agent.poll.side_effect = [None, -9]
        agent.wait.side_effect = [subprocess.TimeoutExpired('agent', 10), -9]
        def prepare(server, token, run_id, report, holder, agent_bin, artifacts):
            holder['agent'] = agent
            return {}
        self.prepare.side_effect = prepare
        code, report = self.run_case()
        self.assertEqual((code, report['status']), (0, 'PASS'))
        agent.terminate.assert_called_once()
        agent.kill.assert_called_once()


class ProvenanceAndPreflightTests(unittest.TestCase):
    def test_manifest_binds_browser_function_driver_and_all_helpers(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            paths = ['manifest.json', 'browser.py', 'driver.py', 'helper.py',
                     'test/e2e/local_business_runner.py', 'test/e2e/local_browser_fixture.py',
                     'test/e2e/deployment_replicas.py']
            for path in paths:
                (root/path).parent.mkdir(parents=True, exist_ok=True)
                (root/path).write_text('# source ' + path)
            manifest = {'test_file': 'browser.py', 'test_function': 'exercise', 'driver_file': 'driver.py'}
            (root/'manifest.json').write_text(json.dumps(manifest))
            exercise = SimpleNamespace(__code__=SimpleNamespace(co_filename=str(root/'browser.py')),
                                       __name__='exercise')
            def git(args, **kwargs):
                return '' if args[1] == 'status' else 'a' * 40
            with patch.object(runner, 'ROOT', root), patch.object(runner, '__file__', str(root/paths[4])), \
                 patch.object(runner.subprocess, 'check_output', side_effect=git):
                _, identity = runner.source_identity('manifest.json', 'driver.py', exercise, ['helper.py'])
                self.assertEqual(set(identity['source_hashes']), set(paths))
                self.assertEqual(identity['working_tree'], 'clean')
                exercise.__name__ = 'wrong_function'
                with self.assertRaisesRegex(AssertionError, 'browser binding'):
                    runner.source_identity('manifest.json', 'driver.py', exercise, [])

    def test_shared_daemon_and_docker_context_are_refused_before_query(self):
        for environment in [{'DOCKER_HOST': 'unix:///run/docker.sock', 'DOCKER_CONTEXT': ''},
                            {'DOCKER_HOST': 'unix:///tmp/dedicated.sock', 'DOCKER_CONTEXT': 'foreign'}]:
            with self.subTest(environment=environment), patch.dict(runner.os.environ, environment), \
                 patch.object(runner.subprocess, 'run') as query:
                with self.assertRaises(AssertionError):
                    runner.docker_preflight({}, 'fixture:image')
                query.assert_not_called()

    def test_docker_connection_error_is_not_missing_image_success(self):
        with patch.dict(runner.os.environ, {'DOCKER_HOST':'unix:///tmp/dedicated.sock', 'DOCKER_CONTEXT':''}), \
             patch.object(runner.subprocess, 'run', return_value=SimpleNamespace(returncode=1, stdout='', stderr='permission denied')):
            with self.assertRaisesRegex(AssertionError, 'permission denied'):
                runner.docker_preflight({}, 'fixture:image')

    def test_console_resource_echo_matches_only_same_path_and_status(self):
        attributed = [{'method': 'POST', 'path': '/api/v1/deployments', 'status': 400}]
        echo = {'path': '/api/v1/deployments',
                'text': 'Failed to load resource: the server responded with a status of 400 (Bad Request)'}
        self.assertEqual(runner.unattributed_console_errors([echo], attributed), [])
        for defect in ({'path': '/api/v1/models'},  # different path
                       {'text': 'Failed to load resource: the server responded with a status of 403 (Forbidden)'},
                       {'text': 'TypeError: boom'}):  # not a resource echo
            with self.subTest(defect=defect):
                error = {**echo, **defect}
                self.assertEqual(runner.unattributed_console_errors([error], attributed), [error])
        self.assertEqual(runner.unattributed_console_errors([echo], []), [echo])


if __name__ == '__main__':
    unittest.main()
