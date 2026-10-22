"""Exercise real domain cleanup, including partial failures and observation errors."""
from types import SimpleNamespace
import unittest
from unittest.mock import patch
from model_hub_fixture_setup import cleanup, prepare
from model_hub_fixture import ModelHub


class StatefulBoundary:
    def __init__(self, failures=()):
        self.models = {7: {"id": 7, "source_id": "e2e-unit/weights"}}
        self.rules = {3: {"id": 3, "note": "e2e-unit-metadata"}, 8: {"id": 8, "note": "foreign"}}
        self.template = True
        self.failures = set(failures)
        self.calls = []

    def __call__(self, base, method, path, token=None, body=None):
        self.calls.append((method, path))
        if (method, path) in self.failures:
            raise TimeoutError("owned observation timeout")
        if path.startswith('/api/v1/models?'):
            values = list(self.models.values())
            return 200, {"items": values, "total": len(values)}
        if path.startswith('/api/v1/models/'):
            ident = int(path.rsplit('/', 1)[1])
            if method == 'DELETE':
                self.models.pop(ident, None)
                return 204, None
            return (200, self.models[ident]) if ident in self.models else (404, {"error": "not found"})
        if path == '/api/v1/pull/source-rules':
            return 200, {"items": list(self.rules.values()), "total": len(self.rules)}
        if path.startswith('/api/v1/pull/source-rules/') and method == 'DELETE':
            del self.rules[int(path.rsplit('/', 1)[1])]
            return 204, None
        if path.startswith('/api/v1/inference-templates/spec?') and method == 'DELETE':
            self.template = False
            return 204, None
        if path == '/api/v1/inference-templates/model/e2e-unit/template':
            return (200, {"model_id": "e2e-unit/template"}) if self.template else (404, {"error": "not found"})
        raise AssertionError('unknown boundary request: ' + path)


class ObservedHub:
    def __init__(self, fail=False):
        self.fail, self.stopped, self.base = fail, False, 'http://127.0.0.1:12345'

    def audit(self):
        return []

    def stop(self):
        if self.fail:
            raise TimeoutError('fixture shutdown timeout')
        self.stopped = True
        return {"stopped": True, "listener_stopped": True, "thread_stopped": True, "handlers": 0}


class CleanupContract(unittest.TestCase):
    def resources(self, hub_fail=False):
        report = {"owned_model_repo": "e2e-unit/weights", "owned_source_rule_ids": [3],
            "owned_source_rule_note": "e2e-unit-metadata", "owned_template_model_id": "e2e-unit/template"}
        return report, {"model_hub": ObservedHub(hub_fail)}

    def test_actual_runner_run_id_prepares_consistent_owned_identities(self):
        run_id = '20261021T172233Z-abcdef12'
        report, holder, rules = {"run_id": run_id}, {}, []
        expected_prefix = 'e2e-20261021t172233z-abcdef12'
        def setup_api(base, method, path, token=None, body=None):
            if method == 'POST' and path == '/api/v1/pull/source-rules':
                record = {**body, 'id': len(rules) + 1}
                rules.append(record)
                return 201, record
            if path.startswith('/api/v1/pull/metadata-sources?source='):
                source = path.split('source=')[1]
                index = 0 if source == 'huggingface' else 1
                return 200, {'source': source, 'bases': rules[index]['endpoints']}
            if method == 'POST' and path == '/api/v1/inference-templates/import':
                self.assertEqual(body['models'][0]['model_id'], expected_prefix + '/template')
                return 200, {'models': 1}
            if method == 'GET' and path == '/api/v1/inference-templates/model/' + expected_prefix + '/template':
                return 200, {'specs': [{'name': 'e2e-model-hub'}]}
            raise AssertionError('unexpected preparation request ' + path)
        try:
            with patch('model_hub_fixture_setup.http', setup_api):
                data = prepare(SimpleNamespace(base='owned-server'), 'synthetic-token', run_id,
                               report, holder, None, None)
            self.assertEqual(report['run_id'], run_id)
            self.assertEqual(data['repo'], expected_prefix + '/weights')
            self.assertEqual(report['owned_model_repo'], data['repo'])
            self.assertEqual(report['owned_template_model_id'], expected_prefix + '/template')
            self.assertEqual(report['owned_source_rule_note'], expected_prefix + '-metadata')
            self.assertEqual(data['hub'].repo, data['repo'])
            self.assertEqual(report['owned_source_rule_ids'], [1, 2])
        finally:
            if holder.get('model_hub'):
                self.assertTrue(holder['model_hub'].stop()['stopped'])

    def test_success_observes_absence_and_keeps_foreign_rule(self):
        boundary = StatefulBoundary()
        report, holder = self.resources()
        with patch('model_hub_fixture_setup.http', boundary):
            self.assertTrue(cleanup(SimpleNamespace(base='http://127.0.0.1:8800'), 'token', report, holder))
        self.assertEqual(report['remaining_resources'], [])
        self.assertIsNone(report['owned_model_repo'])
        self.assertIsNone(report['owned_template_model_id'])
        self.assertEqual(report['owned_source_rule_ids'], [])
        self.assertEqual(list(boundary.rules), [8])
        self.assertTrue(holder['model_hub'].stopped)
        self.assertIn(('GET', '/api/v1/models/7'), boundary.calls)

    def test_model_observation_failure_retains_ownership_and_still_cleans_other_domains(self):
        boundary = StatefulBoundary([('GET', '/api/v1/models/7')])
        report, holder = self.resources()
        with patch('model_hub_fixture_setup.http', boundary):
            self.assertFalse(cleanup(SimpleNamespace(base='unused'), 'token', report, holder))
        self.assertEqual(report['owned_model_repo'], 'e2e-unit/weights')
        self.assertEqual(report['remaining_resources'][0]['kind'], 'model-repo')
        self.assertIn('reclaim', report['remaining_resources'][0])
        self.assertFalse(boundary.template)
        self.assertEqual(list(boundary.rules), [8])
        self.assertTrue(holder['model_hub'].stopped)

    def test_prepare_failure_only_known_names_and_fixture_still_stop(self):
        boundary = StatefulBoundary([('GET', '/api/v1/pull/source-rules')])
        report, holder = self.resources()
        report['owned_template_model_id'] = None
        with patch('model_hub_fixture_setup.http', boundary):
            self.assertFalse(cleanup(SimpleNamespace(base='unused'), None, report, holder))
        self.assertEqual(report['owned_source_rule_note'], 'e2e-unit-metadata')
        self.assertEqual(report['remaining_resources'][0]['kind'], 'source-rules')
        self.assertTrue(holder['model_hub'].stopped)

    def test_fixture_close_exception_prevents_pass_after_api_cleanup(self):
        boundary = StatefulBoundary()
        report, holder = self.resources(hub_fail=True)
        with patch('model_hub_fixture_setup.http', boundary):
            self.assertFalse(cleanup(SimpleNamespace(base='unused'), 'token', report, holder))
        self.assertEqual(report['remaining_resources'][0]['kind'], 'model-hub-listener')
        self.assertEqual(boundary.models, {})
        self.assertFalse(boundary.template)


if __name__ == '__main__':
    unittest.main()
