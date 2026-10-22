"""Negative prerequisites and observed cleanup; never starts a real runtime.

Run: python3 -B -m unittest discover -s test/e2e -p test_deployment_acquisition_fixture.py
"""
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import MagicMock, patch

import deployment_acquisition_fixture as fixture
from local_browser_fixture import Artifacts


class AcquisitionOwnership(unittest.TestCase):
    def test_lost_node_create_response_keeps_scoped_name(self):
        report, holder = {}, {}
        with tempfile.TemporaryDirectory() as directory:
            artifacts = Artifacts(Path(directory) / 'out')
            server = SimpleNamespace(base='http://local', work=artifacts.private)
            with patch.object(fixture, 'http', side_effect=TimeoutError('lost response')):
                with self.assertRaisesRegex(TimeoutError, 'lost response'):
                    fixture.start_node(server, 'admin', 'e2e-case', 'unfit', 'L4x1,24576',
                                       report, holder, Path('agent'), artifacts)
            self.assertEqual(report['owned_acquisition_nodes'],
                             [{'name': 'e2e-case-unfit', 'uuid': None, 'gpu': 'L4x1,24576'}])

    def test_invalid_credential_does_not_launch_or_poison_secret_registry(self):
        for value in (True, 1, {}, [], None, '', ' '):
            with self.subTest(value=value), tempfile.TemporaryDirectory() as directory:
                artifacts = Artifacts(Path(directory) / 'out')
                server = SimpleNamespace(base='http://local', work=artifacts.private)
                report, holder = {}, {}
                with patch.object(fixture, 'http', return_value=(201, {'node': {'uuid': 'own'}, 'token': value})), \
                     patch.object(fixture.subprocess, 'Popen') as launch:
                    with self.assertRaisesRegex(AssertionError, 'invalid token'):
                        fixture.start_node(server, 'admin', 'e2e-case', 'eligible', 'A100x1,24576',
                                           report, holder, Path('agent'), artifacts)
                self.assertEqual(report['owned_node_uuid'], 'own')
                self.assertEqual(artifacts.secrets, ['password'])
                launch.assert_not_called()

    def test_online_failure_retains_secondary_process_and_log(self):
        with tempfile.TemporaryDirectory() as directory:
            artifacts = Artifacts(Path(directory) / 'out')
            server = SimpleNamespace(base='http://local', work=artifacts.private)
            report, holder, process = {}, {}, MagicMock(pid=123)
            with patch.object(fixture, 'http', return_value=(201, {'node': {'uuid': 'own'}, 'token': 'node-secret'})), \
                 patch.object(fixture.subprocess, 'Popen', return_value=process), \
                 patch.object(fixture, 'wait_online', side_effect=TimeoutError('offline')):
                with self.assertRaisesRegex(TimeoutError, 'offline'):
                    fixture.start_node(server, 'admin', 'e2e-case', 'unfit', 'L4x1,24576',
                                       report, holder, Path('agent'), artifacts)
            entry = holder['acquisition_agents'][0]
            self.assertIs(entry['process'], process)
            self.assertEqual(report['owned_acquisition_nodes'][0]['pid'], 123)
            self.assertIn('node-secret', artifacts.secrets)
            entry['log'].close()

    def test_online_node_waits_for_separate_gpu_inventory(self):
        with tempfile.TemporaryDirectory() as directory:
            artifacts = Artifacts(Path(directory) / 'out')
            server = SimpleNamespace(base='http://local', work=artifacts.private)
            report, holder, process = {}, {}, MagicMock(pid=123)
            process.poll.return_value = None
            replies = [(201, {'node': {'uuid': 'own'}, 'token': 'node-secret'}),
                       (200, {'uuid': 'own', 'state': 'online', 'gpus': []}),
                       (200, {'uuid': 'own', 'state': 'online', 'gpus': [{'index': 0, 'name': 'A100'}]})]
            with patch.object(fixture, 'http', side_effect=replies) as request, \
                 patch.object(fixture.subprocess, 'Popen', return_value=process), \
                 patch.object(fixture, 'wait_online'):
                node = fixture.start_node(server, 'admin', 'e2e-case', 'eligible', 'A100x1,24576',
                                          report, holder, Path('agent'), artifacts)
            self.assertEqual(node['gpus'], [{'index': 0, 'name': 'A100'}])
            self.assertEqual(request.call_count, 3)
            self.assertEqual(report['owned_acquisition_nodes'][0]['last_gpu_inventory'], node['gpus'])
            holder['log'].close()

    def cleanup_fixture(self, *, node_error=False, stop_error=False, source_error=False, primary_error=False):
        report = {'owned_acquisition_nodes': [{'name': 'e2e-case-unfit', 'uuid': 'node', 'pid': 123}],
                  'remaining_resources': [], 'cleanup_steps': []}
        process, log, hub = MagicMock(pid=123), MagicMock(closed=False), MagicMock(base='http://own')
        process.poll.return_value = None
        process.terminate.side_effect = (RuntimeError('stop failed') if stop_error else
                                         lambda: setattr(process.poll, 'return_value', 0))
        hub.audit.return_value = [{'complete': False}]
        hub.stop.return_value = {'stopped': not source_error, 'handlers': int(source_error),
                                 'listener_stopped': not source_error}
        holder = {'acquisition_agents': [{'process': process, 'log': log, 'owned': report['owned_acquisition_nodes'][0]}],
                  'acquisition_hub': hub}
        def request(base, method, path, *args, **kwargs):
            self.assertIn(path, ('/api/v1/nodes/node', '/api/v1/nodes/node?force=true'))
            if node_error:
                raise ConnectionError('DNS unavailable')
            return (204 if method == 'DELETE' else 404), {}
        with patch.object(fixture, 'http', side_effect=request), \
             patch.object(fixture.containers, 'cleanup', side_effect=RuntimeError('primary failed') if primary_error else None,
                          return_value=True):
            result = fixture.cleanup(SimpleNamespace(base='http://local'), 'admin', report, holder)
        return result, report, process, log, hub

    def test_success_requires_actual_secondary_stop_and_node_absence(self):
        result, report, process, log, hub = self.cleanup_fixture()
        self.assertTrue(result)
        process.terminate.assert_called_once()
        process.wait.assert_called_once_with(timeout=10)
        log.close.assert_called_once()
        hub.release.assert_called_once()
        self.assertIsNone(report['owned_acquisition_nodes'][0]['uuid'])
        self.assertEqual(report['remaining_resources'], [])

    def test_primary_failure_still_stops_secondary_and_source(self):
        result, report, process, log, hub = self.cleanup_fixture(primary_error=True)
        self.assertFalse(result)
        process.terminate.assert_called_once()
        hub.stop.assert_called_once()
        self.assertEqual([r['kind'] for r in report['remaining_resources']], ['primary-domain'])

    def test_dns_error_keeps_node_identity_and_still_stops_source(self):
        result, report, process, log, hub = self.cleanup_fixture(node_error=True)
        self.assertFalse(result)
        self.assertEqual(report['owned_acquisition_nodes'][0]['uuid'], 'node')
        self.assertIn('DNS unavailable', report['remaining_resources'][0]['state'])
        hub.stop.assert_called_once()

    def test_secondary_stop_failure_closes_log_and_continues(self):
        result, report, process, log, hub = self.cleanup_fixture(stop_error=True)
        self.assertFalse(result)
        self.assertEqual(report['remaining_resources'][0]['kind'], 'acquisition-agent')
        self.assertEqual(report['remaining_resources'][0]['id'], 123)
        log.close.assert_called_once()
        hub.stop.assert_called_once()

    def test_held_source_is_a_failure_not_false_clean_pass(self):
        result, report, process, log, hub = self.cleanup_fixture(source_error=True)
        self.assertFalse(result)
        self.assertEqual(report['remaining_resources'][0]['kind'], 'acquisition-source')
        self.assertEqual(report['fixture_stop']['handlers'], 1)

    def test_import_failure_retains_all_prepared_resources(self):
        from model_download_fixture import tiny_files
        with tempfile.TemporaryDirectory() as directory:
            artifacts = Artifacts(Path(directory) / 'out')
            root = artifacts.private / 'storage'
            root.mkdir()
            server = SimpleNamespace(base='http://local', work=artifacts.private, storage_root=root)
            hub = MagicMock(base='http://127.0.0.1:12345', files=tiny_files())
            nodes = [{'uuid': 'eligible', 'name': 'eligible'}, {'uuid': 'unfit', 'name': 'unfit'}]
            report, holder, bodies = {}, {}, []
            def request(base, method, path, token=None, body=None):
                if path == fixture.RULES:
                    self.assertEqual(body['allow_upstream'], False)
                    self.assertEqual(body['control_plane'], True)
                    bodies.append(body)
                    return 201, {'id': len(bodies)}
                if path.startswith('/api/v1/pull/metadata-sources?source='):
                    source = path.split('=')[-1]
                    return 200, {'source': source, 'bases': [hub.base + ('/hf' if source == 'huggingface' else '/ms')]}
                if path == fixture.STORAGES and method == 'GET':
                    return 200, {'items': [{'id': 1}]}
                if path == fixture.STORAGES + '/1' and method == 'PATCH':
                    self.assertEqual(body, {'enabled': False, 'path': str(root / 'disabled-seeded')})
                    return 200, {'enabled': False}
                if path == fixture.STORAGES and method == 'POST':
                    self.assertEqual(body['targets'], [{'node_uuid': 'eligible'}])
                    bodies.append(body)
                    return 201, {'id': len(bodies), 'name': body['name'], 'path': body['path']}
                if path == fixture.TEMPLATES + '/import':
                    spec = body['models'][0]['specs'][0]
                    self.assertEqual(spec['entrypoint'], ['sglang', 'serve'])
                    self.assertEqual(spec['weights']['sources'][0]['huggingface']['revision'], fixture.REVISION)
                    self.assertEqual(spec['env']['SGLANG_LOGGING_CONFIG_PATH'], '/tmp/e2e-fixture-owned-logging.json')
                    raise TimeoutError('import response lost')
                self.fail(f'unexpected fixture request {method} {path}')
            with patch.object(fixture, 'ModelDownload', return_value=hub), \
                 patch.object(fixture, 'start_node', side_effect=nodes), \
                 patch.object(fixture, 'wait', side_effect=lambda *args: {'id': len(bodies)}), \
                 patch.object(fixture, 'http', side_effect=request):
                with self.assertRaisesRegex(TimeoutError, 'import response lost'):
                    fixture.prepare(server, 'admin', 'case', report, holder, Path('agent'), artifacts,
                                    case='sglang-modelscope')
            self.assertEqual(len(report['owned_source_rule_ids']), 2)
            self.assertEqual(len(report['owned_acquisition_storages']), 2)
            self.assertEqual(report['owned_template_model_id'], 'e2e-case/scheme')
            self.assertIs(holder['acquisition_hub'], hub)

    def test_source_audit_is_observed_after_stop_finalizes_handlers(self):
        records = [{'complete': False, 'bytes_sent': 0}]
        hub = MagicMock(base='http://own')
        hub.audit.side_effect = lambda: [dict(r) for r in records]
        def stopped():
            records[0].update(complete=True, bytes_sent=17, sha256_sent='final-hash')
            return {'stopped': True, 'handlers': 0, 'listener_stopped': True}
        hub.stop.side_effect = stopped
        report = {'remaining_resources': [], 'cleanup_steps': []}
        with patch.object(fixture.containers, 'cleanup', return_value=True):
            self.assertTrue(fixture.cleanup(SimpleNamespace(base='http://local'), 'admin',
                                            report, {'acquisition_hub': hub}))
        self.assertEqual(report['fixture_requests'], records)
        self.assertTrue(report['fixture_requests'][0]['complete'])
        self.assertEqual(report['fixture_requests'][0]['bytes_sent'], 17)

    def test_source_stop_exception_still_keeps_final_audit(self):
        hub = MagicMock(base='http://own')
        hub.stop.side_effect = RuntimeError('stop failed')
        hub.audit.return_value = [{'complete': False, 'bytes_sent': 13}]
        report = {'remaining_resources': [], 'cleanup_steps': []}
        with patch.object(fixture.containers, 'cleanup', return_value=True):
            self.assertFalse(fixture.cleanup(SimpleNamespace(base='http://local'), 'admin',
                                             report, {'acquisition_hub': hub}))
        self.assertEqual(report['fixture_requests'], hub.audit.return_value)
        self.assertEqual(report['remaining_resources'][0]['kind'], 'acquisition-source')

    def test_cleanup_preserves_prior_close_stages_and_primary_partial_observations(self):
        for raises in (False, True):
            with self.subTest(raises=raises):
                prior = [{'stage': 'trace close', 'ok': True},
                         {'stage': 'context close', 'ok': False},
                         {'stage': 'browser close', 'ok': True}]
                report = {'cleanup_steps': list(prior), 'remaining_resources': []}
                def primary(server, token, report, holder, **kwargs):
                    report['cleanup_steps'] = [{'stage': 'container inventory', 'ok': True},
                                               {'stage': 'deployment delete', 'ok': not raises}]
                    if raises:
                        raise RuntimeError('cleanup interrupted after inventory')
                    return True
                with patch.object(fixture.containers, 'cleanup', side_effect=primary):
                    result = fixture.cleanup(SimpleNamespace(base='http://local'), 'admin', report, {})
                self.assertEqual(result, not raises)
                self.assertEqual(report['cleanup_steps'][:3], prior)
                self.assertEqual([s['stage'] for s in report['cleanup_steps']],
                    ['trace close', 'context close', 'browser close', 'container inventory',
                     'deployment delete', 'primary-domain'])
                self.assertFalse(report['cleanup_steps'][1]['ok'])
                self.assertEqual(report['cleanup_steps'][4]['ok'], not raises)
                self.assertEqual(report['cleanup_steps'][5]['ok'], not raises)

    def test_unknown_case_rejected_before_external_setup(self):
        with patch.object(fixture, 'ModelDownload') as source:
            with self.assertRaisesRegex(AssertionError, 'unknown acquisition'):
                fixture.prepare(None, None, 'case', {}, {}, None, None, case='callable-url')
            source.assert_not_called()


if __name__ == '__main__':
    unittest.main()
