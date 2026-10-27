"""Strict hostile-provider boundary and original-log safety regression tests.

PYTHONPATH=test/e2e python3 -B -m unittest test_provider_secret_error_fixture -v
No product/browser/build/cloud calls; actual local HTTP and original bytes.
"""
import hashlib
import json
from pathlib import Path
from types import SimpleNamespace
import tempfile
import unittest
from unittest import mock
import urllib.error
import urllib.request

from local_browser_fixture import Artifacts
from provider_secret_error_fixture import SecretErrorRunPodFixture
from provider_secret_error_checks import WARN_MESSAGE, WARN_REASON, wait_raw_ledger
from provider_url_checks import close_fixture
from test_provider_endpoint_fixture import KEY_A, KEY_B

SECRET = 'synthetic-only-secret-error-key'
MARKER = 'synthetic-error-boundary'


class SecretErrorBoundaryTest(unittest.TestCase):
    def setUp(self):
        self.fixture = SecretErrorRunPodFixture(SECRET, MARKER, keys=[KEY_A])
        self.fixture.__enter__()
        self.client = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def tearDown(self):
        report = {'status': 'NOT_RUN'}
        close_fixture(self.fixture, report, 'unit-secret-error')
        self.assertEqual(report['provider_url_fixture_cleanup']['unit-secret-error'],
                         {'loop_stopped': True, 'listener_closed': True, 'failures': []})

    def request(self, path='/proxy-a/v2/account/ssh-keys', method='GET', body=None,
                authorization='Bearer ' + SECRET, raw=None):
        headers = {'Content-Type': 'application/json'}
        if authorization is not None:
            headers['Authorization'] = authorization
        request = urllib.request.Request(self.fixture.base + path, method=method, headers=headers,
            data=raw if raw is not None else json.dumps(body).encode() if body is not None else None)
        try:
            response = self.client.open(request, timeout=5)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            wire = response.read()
            return response.status, response.headers.get_content_type(), json.loads(wire), wire

    def test_only_declared_authenticated_errors_emit_exact_secret_and_http_ledger(self):
        status, kind, body, wire = self.request()
        self.assertEqual((status, kind), (400, 'application/problem+json'))
        self.assertEqual(body, {'title': 'Bad Request', 'status': 400,
                               'detail': MARKER + ': upstream echoed Bearer ' + SECRET})
        emitted = self.fixture.emitted_errors[0]
        self.assertEqual(emitted['authorization'], 'Bearer ' + SECRET)
        self.assertEqual(emitted['body_sha256'], hashlib.sha256(wire).hexdigest())
        self.assertEqual(emitted['bytes'], len(wire))
        self.fixture.pods_error = True
        self.assertEqual(self.request('/proxy-a/v2/pods')[0:2], (403, 'application/problem+json'))
        self.fixture.pods_error = False
        self.assertEqual(self.request('/proxy-a/v2/pods')[2],
                         {'pods': [], 'pagination': {'nextCursor': None, 'hasNextPage': False}})
        self.assertEqual([(r['method'], r['path'], r['status'], r['authorized']) for r in self.fixture.requests],
                         [('GET', '/proxy-a/v2/account/ssh-keys', 400, True),
                          ('GET', '/proxy-a/v2/pods', 403, True), ('GET', '/proxy-a/v2/pods', 200, True)])
        self.assertEqual([row['status'] for row in self.fixture.emitted_errors], [400, 403])
        self.assertEqual(self.fixture.keys, [KEY_A])

    def test_missing_malformed_wrong_and_forbidden_auth_do_not_enter_echo_branches(self):
        self.fixture.pods_error = True
        for path in ('/proxy-a/v2/pods', '/proxy-a/v2/account/ssh-keys'):
            for authorization in (None, SECRET, 'Bearer wrong-key'):
                result = self.request(path, authorization=authorization)
                self.assertEqual(result[0:2], (401, 'application/problem+json'))
                self.assertNotIn(SECRET.encode(), result[3])
        self.fixture.forbidden = True
        self.assertEqual(self.request()[2], {'title': 'Forbidden', 'status': 403, 'detail': 'access denied'})
        self.fixture.forbidden = False
        self.fixture.reject = True
        self.assertEqual(self.request()[0], 401)
        self.assertEqual(self.fixture.emitted_errors, [])
        self.assertEqual(self.fixture.keys, [KEY_A])

    def test_unknown_queries_paths_methods_and_invalid_json_stay_fail_closed(self):
        for path in ('/proxy-a/v2/pods?extra=1', '/proxy-a/v2/account/ssh-keys?', '/unknown'):
            self.assertEqual(self.request(path)[0], 404)
        for method in ('POST', 'PATCH', 'DELETE'):
            self.assertEqual(self.request(method=method)[0], 405)
        for body in ({'keys': None}, {'keys': [False]}, {'keys': ['bad-key']}, {'keys': [], 'extra': True}):
            self.assertEqual(self.request(method='PUT', body=body)[0], 422)
        self.assertEqual(self.request(method='PUT', raw=b'{')[0], 400)
        self.assertEqual(self.fixture.keys, [KEY_A])
        self.assertEqual(self.fixture.emitted_errors, [])

    def test_http_only_mode_reuses_actual_base_key_replacement_and_pods_protocol(self):
        self.fixture.key_error = False
        self.assertEqual(self.request()[2], {'keys': [KEY_A]})
        self.assertEqual(self.request(method='PUT', body={'keys': [KEY_A, KEY_B]})[2], {'keys': [KEY_A, KEY_B]})
        self.assertEqual(self.request()[2], {'keys': [KEY_A, KEY_B]})
        self.assertEqual(self.request('/proxy-a/v2/pods')[0], 200)
        self.assertEqual(self.fixture.emitted_errors, [])
        self.fixture.pods_error = True
        self.assertEqual(self.request('/proxy-a/v2/pods')[0], 403)
        self.assertEqual(len(self.fixture.emitted_errors), 1)


class RawProductLogGuardTest(unittest.TestCase):
    def setup_log(self, directory, raw):
        artifacts = Artifacts(Path(directory) / 'run')
        artifacts.remember(SECRET)
        (artifacts.private / 'server.log').write_bytes(raw)
        page = mock.Mock()
        return page, SimpleNamespace(work=artifacts.private), artifacts

    def test_product_leak_is_rejected_even_when_artifact_redaction_would_hide_it(self):
        with tempfile.TemporaryDirectory() as directory:
            raw = ('level=WARN msg="' + WARN_MESSAGE + '" err="' + WARN_REASON + ': ' + SECRET + '"\n'
                   'msg="http request" method=POST path=/api/v1/providers status=201 name=owned\n').encode()
            page, server, artifacts = self.setup_log(directory, raw)
            self.assertNotIn(SECRET, artifacts.redact(raw.decode()))
            with self.assertRaisesRegex(AssertionError, 'raw product log contains a registered secret'):
                wait_raw_ledger(page, server, artifacts, 0, 'POST', '/api/v1/providers', 201,
                                name='owned', warning=True)
            page.evaluate.assert_not_called()

    def test_safe_log_requires_concrete_warn_and_http_ledger_not_just_absence_of_secret(self):
        with tempfile.TemporaryDirectory() as directory:
            raw = ('level=WARN msg="' + WARN_MESSAGE + '" err="' + WARN_REASON + '"\n'
                   'msg="http request" method=POST path=/api/v1/providers status=201 name=owned\n').encode()
            page, server, artifacts = self.setup_log(directory, raw)
            observed = wait_raw_ledger(page, server, artifacts, 0, 'POST', '/api/v1/providers', 201,
                                       name='owned', warning=True)
            self.assertIn(WARN_REASON, observed['raw_warn'])
            self.assertIn('status=201', observed['http_ledger'])
            self.assertEqual(observed['raw_log_snapshot_sha256'], hashlib.sha256(raw).hexdigest())
            page.evaluate.assert_not_called()

    def test_empty_log_cannot_pass_and_wait_is_bounded_by_observation_deadline(self):
        with tempfile.TemporaryDirectory() as directory:
            page, server, artifacts = self.setup_log(directory, b'')
            with mock.patch('provider_secret_error_checks.time.monotonic', side_effect=[0, 0, 13]), \
                    self.assertRaisesRegex(AssertionError, 'empty logs are not secrecy evidence'):
                wait_raw_ledger(page, server, artifacts, 0, 'POST', '/api/v1/providers', 201, warning=True)
            page.evaluate.assert_called_once()


if __name__ == '__main__':
    unittest.main()
