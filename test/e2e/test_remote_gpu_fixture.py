"""Cleanup ownership tests; unit evidence, not real GPU coverage.

E2E standard: test/e2e/browser-e2e-testing-standard.md
"""
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from remote_gpu_fixture import cleanup_gpu


class CleanupSession:
    def __init__(self, baseline_running=True):
        self.deployment = {'id': 17}
        self.container_id = 'owned-container'
        self.gpu_baseline = {'containers': ['unrelated-container']}
        self.config = {'gpu_index': 0, 'model_id': 22}
        self.model_baseline = {'id': 22, 'nodes': []}
        self.model_after = {'id': 22, 'nodes': [{'node_uuid': 'node', 'cache': True, 'path': '/shared/cache/model'}]}
        self.owned = [{'kind': 'deployment', 'id': 17}]
        self.calls = []
        self.evidence_items = []
        self.baseline_running = baseline_running
        self.context = SimpleNamespace(request=SimpleNamespace(get=self.get))
        self.base = 'http://fixture.invalid'

    def api(self, method, path, body=None, expected=200):
        self.calls.append((method, path))
        if (method, path, expected) == ('DELETE', '/api/v1/deployments/17', 202):
            return {'status': 'deleting'}
        if (method, path) == ('GET', '/api/v1/instances'):
            return {'items': []}
        if (method, path) == ('GET', '/api/v1/models/22'):
            return self.model_after
        raise AssertionError('Unexpected API operation (including shared model eviction)')

    def get(self, url):
        assert url == self.base + '/api/v1/deployments/17'
        return SimpleNamespace(status=404)

    def poll(self, fn, predicate, timeout=120):
        result = fn()
        assert predicate(result), 'controlled fixture has not satisfied cleanup condition'
        return result

    def host(self, code):
        def output(args, **kwargs):
            assert kwargs == {'text': True}
            if args in (['docker', 'ps', '-aq', '--no-trunc'], ['docker', 'ps', '-q', '--no-trunc']):
                return 'unrelated-container\n' if self.baseline_running else ''
            if args == ['nvidia-smi', '-i', '0', '--query-gpu=memory.used', '--format=csv,noheader,nounits']:
                return '0\n'
            raise AssertionError('Unexpected host command')
        scope = {}
        with patch('subprocess.check_output', side_effect=output):
            exec(compile(code, '<host-cleanup-inspection>', 'exec'), scope)
        return scope['result']

    def forget(self, kind, identity):
        self.owned.remove({'kind': kind, 'id': identity})

    def evidence(self, layer, detail):
        self.evidence_items.append((layer, detail))


class GPUCleanupTest(unittest.TestCase):
    # Mainline: owned deployment stops and GPU frees → shared cache remains documented, never evicted.
    def test_shared_cache_is_fixture_not_owned_cleanup(self):
        r = CleanupSession()
        cleanup_gpu(r)
        self.assertEqual(r.owned, [])
        self.assertEqual([call for call in r.calls if call[0] == 'DELETE'], [('DELETE', '/api/v1/deployments/17')])
        fixture = next(detail for layer, detail in r.evidence_items if layer == 'fixture')
        self.assertEqual(fixture['node_cache_before'], [])
        self.assertEqual(fixture['node_cache_after'], r.model_after['nodes'])

    # Mainline: owned cleanup observed but an original container disappeared → fail, never report environment restored.
    def test_missing_original_container_rejects_cleanup_claim(self):
        r = CleanupSession(baseline_running=False)
        with self.assertRaisesRegex(AssertionError, 'baseline container stopped'):
            cleanup_gpu(r)
        self.assertTrue(r.owned)
        self.assertFalse(any(layer == 'cleanup' for layer, _ in r.evidence_items))


if __name__ == '__main__':
    unittest.main()
