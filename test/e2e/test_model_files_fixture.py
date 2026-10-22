"""Domain cleanup failure/prepare-partial ownership and independent hash tests."""
import copy
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import model_files_fixture as fixture


class Hub:
    base = 'http://127.0.0.1:12345'
    def __init__(self, stopped=True):
        self.stopped, self.released, self.closed = stopped, False, False
    def release(self):
        self.released = True
    def stop(self):
        self.closed = True
        return {'stopped': self.stopped, 'handlers': 0 if self.stopped else 1,
                'thread_stopped': True, 'listener_stopped': True}
    def audit(self):
        return []


class CleanupFixtureTests(unittest.TestCase):
    def setUp(self):
        self.server = type('Server', (), {'base': 'http://127.0.0.1:8800'})()
        self.root = tempfile.TemporaryDirectory()
        self.addCleanup(self.root.cleanup)
        self.storage = {'id': 8, 'name': 'e2e-owned-storage', 'root': self.root.name,
                        'payload_path': str(Path(self.root.name) / 'owned-model')}
        self.model = {'id': 9, 'name': 'e2e-owned-model', 'source_id': 'e2e-owned/model'}
        self.node = {'id': 'owned-node-uuid', 'name': 'e2e-owned-node'}
        self.report = {'model_files_owned': {'storages': [self.storage], 'model': self.model, 'node': self.node}}
        self.hub = Hub()
        self.calls, self.deleted = [], set()

    def request(self, base, method, path, token, body=None):
        self.calls.append((method, path))
        if method == 'DELETE':
            self.deleted.add(path.split('?')[0])
            if path.startswith(fixture.NODES + '/'):
                return 200, {'status': 'deleted', 'agent_uninstalled': False}
            return (200, {}) if '/nodes/' in path and path.startswith(fixture.MODELS) else (204, None)
        if path.startswith(fixture.MODELS + '?'):
            return 200, {'items': [] if fixture.MODELS+'/9' in self.deleted else [self.model],
                         'total': 0 if fixture.MODELS+'/9' in self.deleted else 1}
        if path in self.deleted:
            return 404, {'error': 'not found'}
        raise AssertionError('unexpected fixture HTTP call: ' + method + ' ' + path)

    def run_cleanup(self, request=None):
        with patch.object(fixture, 'http', request or self.request):
            return fixture.cleanup(self.server, 'private-token', self.report, {'model_download': self.hub})

    def test_query_failure_keeps_model_identity_and_other_cleanup_runs(self):
        def failed(base, method, path, token, body=None):
            if path.startswith(fixture.MODELS):
                self.calls.append((method, path))
                raise OSError('management DNS failed')
            return self.request(base, method, path, token, body)
        self.assertFalse(self.run_cleanup(failed))
        self.assertEqual(self.report['model_files_owned']['model']['id'], 9)
        self.assertTrue(self.hub.closed)
        self.assertIn(('DELETE', fixture.STORAGES + '/8'), self.calls)
        self.assertIn(('DELETE', fixture.NODES + '/owned-node-uuid?force=true'), self.calls)
        self.assertTrue(any(r.get('source_id') == 'e2e-owned/model' for r in self.report['remaining_resources']))

    def test_payload_residue_fails_even_if_rpc_claims_success(self):
        Path(self.storage['payload_path']).mkdir()
        self.assertFalse(self.run_cleanup())
        residual = next(r for r in self.report['remaining_resources'] if r['kind'] == 'model files')
        self.assertEqual((residual['model_id'], residual['node_uuid'], residual['payload_path']),
                         (9, 'owned-node-uuid', self.storage['payload_path']))
        self.assertTrue(self.report['model_files_owned']['model']['metadata_absent'])
        self.assertTrue(self.hub.closed)

    def test_listener_close_failure_keeps_residual_non_success(self):
        self.hub.stopped = False
        self.assertFalse(self.run_cleanup())
        self.assertEqual(self.report['remaining_resources'][-1]['kind'], 'repository-listener')
        self.assertEqual(self.report['repository_stopped']['handlers'], 1)

    def test_success_observes_absence_before_releasing_ownership(self):
        self.assertTrue(self.run_cleanup())
        self.assertEqual(self.report['remaining_resources'], [])
        self.assertIsNone(self.report['model_files_owned']['model'])
        self.assertIsNone(self.report['model_files_owned']['node'])
        self.assertTrue(self.storage['reclaimed'])
        self.assertTrue(self.hub.released and self.hub.closed)
        self.assertIn(('GET', fixture.MODELS + '/9'), self.calls)
        self.assertIn(('GET', fixture.STORAGES + '/8'), self.calls)
        self.assertIn(('GET', fixture.NODES + '/owned-node-uuid'), self.calls)

    def test_partially_prepared_names_are_discovered_before_cleanup(self):
        self.storage['id'] = self.node['id'] = self.model['id'] = None
        def discovered(base, method, path, token, body=None):
            if method == 'GET' and path == fixture.STORAGES:
                return 200, {'items': [{'id': 8, 'name': self.storage['name']}, {'id': 18, 'name': 'foreign'}]}
            if method == 'GET' and path.startswith(fixture.NODES + '?'):
                return 200, {'items': [{'uuid': 'owned-node-uuid', 'name': self.node['name']}]}
            if method == 'GET' and path.startswith(fixture.MODELS + '?'):
                return 200, {'items': [{**self.model, 'id': 9}], 'total': 1}
            return self.request(base, method, path, token, body)
        self.assertTrue(self.run_cleanup(discovered))
        self.assertNotIn(('DELETE', fixture.STORAGES + '/18'), self.calls)
        self.assertTrue(self.hub.closed)

    def test_prepare_create_error_preserves_predeclared_name_and_listener(self):
        server = type('Server', (), {'base': self.server.base})()
        report, holder = {}, {}
        hub = Hub()
        hub.url, hub.files = hub.base + '/owned', {'config.json': b'{}', 'model.safetensors': b'weights'}
        hub.start = lambda: hub
        artifacts = type('Artifacts', (), {'remember': lambda self, value: None})()
        def reject(*_args, **_kwargs):
            raise TimeoutError('after creation reply lost')
        with patch.object(fixture, 'ModelDownload', return_value=hub), patch.object(fixture, 'http', reject):
            with self.assertRaises(TimeoutError):
                fixture.prepare(server, 'private-token', 'OWNED-RUN', report, holder, Path('/unused'), artifacts)
        self.assertEqual(report['model_files_owned']['node']['name'], 'e2e-owned-run-files-node')
        self.assertIs(holder['model_download'], hub)

    def test_wrong_node_delete_success_status_is_rejected_even_with_get404(self):
        def wrong(*args, **kwargs):
            response = self.request(*args, **kwargs)
            if args[1] == 'DELETE' and args[2].startswith(fixture.NODES + '/'):
                return 204, None
            return response
        self.assertFalse(self.run_cleanup(wrong))
        self.assertIsNotNone(self.report['model_files_owned']['node'])
        self.assertTrue(any(r['kind'] == 'node' for r in self.report['remaining_resources']))

    def test_agent_stops_before_owned_node_force_delete(self):
        events = []
        class Process:
            pid = 1234
            stopped = False
            def poll(self):
                return 0 if self.stopped else None
            def terminate(self):
                events.append('terminate-owned-agent')
                self.stopped = True
            def wait(self, timeout):
                return 0
        process = Process()
        def ordered(*args, **kwargs):
            if args[1] == 'DELETE' and args[2].startswith(fixture.NODES):
                self.assertTrue(process.stopped)
                self.assertEqual(args[2], fixture.NODES + '/owned-node-uuid?force=true')
            return self.request(*args, **kwargs)
        with patch.object(fixture, 'http', ordered):
            self.assertTrue(fixture.cleanup(self.server, 'token', self.report,
                {'model_download': self.hub, 'agent': process}))
        self.assertEqual(events, ['terminate-owned-agent'])

    def test_independent_file_hash_refuses_corruption_missing_and_symlink(self):
        root = Path(self.root.name)
        values = {'config.json': b'{"owned":true}', 'model.safetensors': b'expected exact bytes'}
        for name, value in values.items():
            (root/name).write_bytes(value)
        self.assertEqual(fixture.file_facts(root, values), fixture.payload_facts(values))
        (root/'model.safetensors').write_bytes(b'expected exact bytez')
        with self.assertRaisesRegex(AssertionError, 'payload bytes differ'):
            fixture.file_facts(root, values)
        (root/'model.safetensors').unlink()
        with self.assertRaisesRegex(AssertionError, 'missing real payload'):
            fixture.file_facts(root, values)
        (root/'model.safetensors').symlink_to(root/'config.json')
        with self.assertRaisesRegex(AssertionError, 'missing real payload'):
            fixture.file_facts(root, values)


if __name__ == '__main__':
    unittest.main()
