"""Negative oracle and manifest/source binding tests; no product process starts."""
import copy
import hashlib
import json
from pathlib import Path
from types import SimpleNamespace
import unittest

from local_browser_fixture import ROOT
from local_business_runner import final_status, source_identity, unattributed_console_errors
from model_files_browser import finish, watch, wire_proof
from model_files_lifecycle import CASES, HELPERS


class FakePage:
    def __init__(self):
        self.handlers = {}
    def on(self, event, handler):
        self.handlers.setdefault(event, []).append(handler)


class AuditHub:
    root = '/e2e-unit/tiny-model'
    files = {'config.json': b'{"owned":true}', 'model.safetensors': b'abcdefghij'}
    def __init__(self):
        self.idle = True
        self.records = []
        for name, body in self.files.items():
            self.records.append({'target': self.root + '/' + name, 'method': 'GET',
                'status': 200, 'range': None, 'bytes_sent': len(body),
                'sha256_sent': hashlib.sha256(body).hexdigest(), 'complete': True})
    def wait_idle(self):
        return self.idle
    def audit(self):
        return copy.deepcopy(self.records)


class ModelFilesRunnerTests(unittest.TestCase):
    def test_wire_oracle_refuses_planned_length_without_actual_bytes(self):
        hub = AuditHub()
        weight = hub.records[-1]
        weight.update(planned_bytes=10, bytes_sent=0, sha256_sent=hashlib.sha256(b'').hexdigest())
        with self.assertRaisesRegex(AssertionError, 'wire did not deliver complete'):
            wire_proof({'hub': hub}, 0)

    def test_wire_oracle_refuses_missing_range_and_wrong_slice_hash(self):
        hub = AuditHub()
        hub.records[-1].update(range='bytes=2-9', bytes_sent=8,
                              sha256_sent=hashlib.sha256(b'cdefghij').hexdigest())
        with self.assertRaisesRegex(AssertionError, 'wire coverage hole'):
            wire_proof({'hub': hub}, 0)
        hub = AuditHub()
        hub.records[-1]['sha256_sent'] = hashlib.sha256(b'wrongbytes').hexdigest()
        with self.assertRaisesRegex(AssertionError, 'actual wire slice SHA256'):
            wire_proof({'hub': hub}, 0)

    def test_wire_oracle_refuses_held_handler_despite_complete_records(self):
        hub = AuditHub()
        hub.idle = False
        with self.assertRaisesRegex(AssertionError, 'handlers remain'):
            wire_proof({'hub': hub}, 0)

    def test_partial_sent_and_exact_range_suffix_union_proves_final_payload(self):
        hub = AuditHub()
        hub.records.pop()
        for start, end, complete in ((0, 4, False), (4, 10, True)):
            hub.records.append({'target': hub.root + '/model.safetensors', 'method': 'GET',
                'status': 206, 'range': f'bytes={start}-9', 'bytes_sent': end-start,
                'sha256_sent': hashlib.sha256(hub.files['model.safetensors'][start:end]).hexdigest(),
                'complete': complete, 'interrupted': not complete})
        proof = wire_proof({'hub': hub}, 0)
        self.assertEqual(proof['files']['model.safetensors']['covered_bytes'], 10)
        self.assertEqual(proof['files']['config.json']['bytes'], 14)

    def test_each_case_binds_exact_browser_function_and_all_oracle_source(self):
        expected = {'download-delete': 'exercise_download_delete',
                    'failure-retry': 'exercise_failure_retry',
                    'interrupted-recovery': 'exercise_interrupted_recovery'}
        self.assertEqual({key: func.__name__ for key, func in CASES.items()}, expected)
        for case, exercise in CASES.items():
            manifest, identity = source_identity('test/e2e/coverage/model-files-' + case + '.json',
                ROOT/'test/e2e/model_files_lifecycle.py', exercise, HELPERS)
            self.assertEqual(manifest['contract_id'], 'gpuworks/models/files-' + case)
            self.assertEqual(set(manifest['required_evidence']), {'ui', 'request', 'persistence', 'execution', 'cleanup'})
            for path in ('test/e2e/model_files_fixture.py', 'test/e2e/model_download_fixture.py',
                         'test/e2e/model_files_browser.py', 'test/e2e/test_model_files_fixture.py'):
                self.assertIn(path, identity['source_hashes'])
            with self.assertRaisesRegex(AssertionError, 'browser binding differs'):
                source_identity('test/e2e/coverage/model-files-' + case + '.json',
                    ROOT/'test/e2e/model_files_lifecycle.py', next(f for f in CASES.values() if f != exercise), HELPERS)

    def test_unattributed_console_and_http_errors_fail_finish(self):
        page, report = FakePage(), {}
        watch(page, report)
        page.handlers['console'][0](SimpleNamespace(type='error', text='TypeError: boom',
            location={'url': 'http://local/models'}))
        page.handlers['console'][0](SimpleNamespace(type='warning', text='not an error',
            location={'url': 'http://local/models'}))
        page.handlers['response'][0](SimpleNamespace(url='http://local/api/v1/models', status=500,
            request=SimpleNamespace(method='GET')))
        self.assertEqual(report['console_errors'], [{'path': '/models', 'text': 'TypeError: boom'}])
        self.assertEqual(report['http_errors'], [{'method': 'GET', 'path': '/api/v1/models', 'status': 500}])
        with self.assertRaisesRegex(AssertionError, 'browser console errors'):
            finish(None, None, report, None, None)
        report['console_errors'] = []
        with self.assertRaisesRegex(AssertionError, 'unexpected browser HTTP errors'):
            finish(None, None, report, None, None)

    def test_anonymous_session_probe_is_attributed_until_login(self):
        page, report = FakePage(), {}
        session = watch(page, report)
        message = SimpleNamespace(type='error', text='Failed to load resource: status of 401',
            location={'url': 'http://local/api/v1/auth/me'})
        probe = SimpleNamespace(url='http://local/api/v1/auth/me', status=401,
            request=SimpleNamespace(method='GET'))
        page.handlers['console'][0](message)
        page.handlers['response'][0](probe)
        self.assertEqual(report['console_errors'], [])
        self.assertEqual(report['http_errors'], [])
        session['authenticated'] = True
        page.handlers['console'][0](message)
        page.handlers['response'][0](probe)
        self.assertEqual(len(report['console_errors']), 1)
        self.assertEqual(report['http_errors'], [{'method': 'GET', 'path': '/api/v1/auth/me', 'status': 401}])
        with self.assertRaisesRegex(AssertionError, 'browser console errors'):
            finish(None, None, report, None, None)

    def test_allowed_download_refusal_is_not_an_http_error(self):
        page, report = FakePage(), {'allowed_http': [('POST', '/api/v1/models/7/download', 409)]}
        session = watch(page, report)
        session['authenticated'] = True
        page.handlers['response'][0](SimpleNamespace(
            url='http://local/api/v1/models/7/download', status=409,
            request=SimpleNamespace(method='POST')))
        page.handlers['console'][0](SimpleNamespace(type='error',
            text='Failed to load resource: the server responded with a status of 409 (Conflict)',
            location={'url': 'http://local/api/v1/models/7/download'}))
        self.assertEqual(report['http_errors'], [])
        self.assertEqual(report['attributed_responses'],
                         [{'path': '/api/v1/models/7/download', 'status': 409}])
        self.assertEqual(unattributed_console_errors(
            report['console_errors'], report['attributed_responses']), [])

    def test_console_resource_echo_needs_an_attributed_response(self):
        # This contract declares no expected >=400, so a resource echo can never
        # be cross-attributed here; the mechanism is the same as conclude's.
        page, report = FakePage(), {}
        session = watch(page, report)
        session['authenticated'] = True
        page.handlers['console'][0](SimpleNamespace(type='error',
            text='Failed to load resource: the server responded with a status of 400 (Bad Request)',
            location={'url': 'http://local/api/v1/models/7/download'}))
        with self.assertRaisesRegex(AssertionError, 'browser console errors'):
            finish(None, None, report, None, None)

    def test_cleanup_or_execution_missing_cannot_turn_case_pass(self):
        report = {'case_completed': True, 'environment_cleanup_ok': True, 'browser_close_ok': True,
                  'artifact_scan': 'PASS', 'remaining_resources': [], 'status': 'NOT_RUN',
                  'cleanup_steps': [{'stage': 'owned resources', 'ok': True}],
                  'evidence': [{'layer': v} for v in ('ui', 'request', 'persistence', 'execution', 'cleanup')]}
        required = ['ui', 'request', 'persistence', 'execution', 'cleanup']
        self.assertEqual(final_status(report, required), 'PASS')
        for changed in ({'environment_cleanup_ok': False}, {'browser_close_ok': False},
                        {'remaining_resources': [{'id': 'owned-failed-close'}]},
                        {'cleanup_steps': [{'stage': 'repository-listener', 'ok': False}]},
                        {'evidence': report['evidence'][:-2] + [{'layer': 'cleanup'}]},
                        {'status': 'FAIL_PRODUCT'}, {'failure': 'prior operation failed'}):
            with self.subTest(changed=changed):
                self.assertNotEqual(final_status({**report, **changed}, required), 'PASS')


if __name__ == '__main__':
    unittest.main()
