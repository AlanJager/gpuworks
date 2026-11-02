"""Focused whitespace/rotation helper failures; never stand in for browser PASS.

PYTHONPATH=test/e2e python3 -B -m unittest test_provider_text_value_checks -v
"""
from types import SimpleNamespace
import unittest
from unittest import mock

import provider_text_value_checks as case
import provider_key_rotation_checks as rotation


class ValueOracleTests(unittest.TestCase):
    def test_diagnostic_families_skip_before_accessing_browser_or_tls(self):
        report = {'check_family': 'key-rotation'}
        self.assertEqual(case.exercise_text_values(None, None, None, None, report, None, None, None, []), [])
        report = {'check_family': 'text-values'}
        self.assertEqual(rotation.exercise_key_rotation(None, None, None, None, report, None, None), [])

    def test_registration_preserves_all_existing_keys_and_appends_only_if_missing(self):
        existing = ['ssh-ed25519 AAAAforeign foreign@example']
        platform = 'ssh-ed25519 AAAAplatform server@example'
        calls, after = case.registration_calls(existing, [platform])
        self.assertEqual(existing, ['ssh-ed25519 AAAAforeign foreign@example'])
        self.assertEqual(after, [*existing, platform])
        self.assertEqual([call['method'] for call in calls], ['GET', 'GET', 'PUT'])
        self.assertEqual(calls[-1]['body'], {'keys': after})
        comments_differ = [*existing, 'ssh-ed25519 AAAAplatform another-comment']
        calls, after = case.registration_calls(comments_differ, [platform], '/v2')
        self.assertEqual(after, comments_differ)
        self.assertEqual([call['path'] for call in calls], ['/v2/pods', '/v2/account/ssh-keys'])
        with self.assertRaises(AssertionError):
            case.registration_calls(existing, [])

    def test_trace_stop_failure_cannot_enter_or_register_a_secret(self):
        control = mock.Mock()
        artifacts = mock.Mock()
        artifacts.stop_trace.side_effect = RuntimeError('injected trace stop failure')
        with self.assertRaisesRegex(RuntimeError, 'injected trace stop'):
            case.type_secret(control, 'synthetic-credential', object(), artifacts)
        artifacts.remember.assert_not_called()
        control.fill.assert_not_called()

    def test_secret_registration_failure_cannot_type_the_value(self):
        control = mock.Mock()
        artifacts = mock.Mock()
        artifacts.remember.side_effect = RuntimeError('injected registration failure')
        with self.assertRaisesRegex(RuntimeError, 'injected registration'):
            case.type_secret(control, 'synthetic-credential', object(), artifacts)
        artifacts.stop_trace.assert_called_once()
        control.fill.assert_not_called()

    def test_ambiguous_create_id_is_owned_before_any_projection_assertions(self):
        flow = object.__new__(case.AccountFlow)
        flow.scope = 'test-owned-account'
        flow.report = {'run_id': 'owned-run'}
        response = SimpleNamespace(status=201, url='http://local/api/v1/providers',
                                   request=SimpleNamespace(method='POST', url='http://local/api/v1/providers'),
                                   json=lambda: {'name': 'unexpected projection'})
        flow.observe_create(response)
        self.assertEqual(flow.report['remaining_resources'][0]['id'], None)
        with self.assertRaisesRegex(AssertionError, 'not independently reclaimed'):
            flow.assert_no_owned_accounts()
        response.json = lambda: {'id': 43}
        flow.observe_create(response)
        flow.observe_create(response)
        self.assertEqual(len(flow.report['remaining_resources']), 2)

    def test_update_without_account_cannot_accidentally_post_another_provider(self):
        flow = object.__new__(case.AccountFlow)
        with self.assertRaisesRegex(AssertionError, 'oracle mode mismatch'):
            flow.submit({'name': 'new', 'endpoint': 'http://local'}, stage='invalid-helper-call')

    def test_complete_external_ledger_detects_calls_between_checked_actions(self):
        flow = object.__new__(case.AccountFlow)
        flow.scope, flow.external_expected = 'ledger-test', {}
        fixture = SimpleNamespace(requests=[])
        flow.watch_external(fixture)
        expected = case.pods_call()
        fixture.requests.append(expected)
        flow.verify_external(fixture, 0, [expected], 'one action')
        fixture.requests.append(case.pods_call(authorized=False))
        with self.assertRaisesRegex(AssertionError, 'complete external call ledger'):
            flow.verify_all_external()

    def test_business_failure_closes_real_external_listener_and_keeps_provider_id(self):
        owned = {'kind': 'provider', 'id': 9, 'check_scope': 'case'}
        report = {'status': 'FAIL_TEST', 'remaining_resources': [owned]}
        with self.assertRaisesRegex(RuntimeError, 'injected business failure'):
            with case.external_fixture('synthetic-fixture-key', report, 'failure-test', []) as fixture:
                self.assertTrue(fixture.thread.is_alive())
                raise RuntimeError('injected business failure')
        self.assertFalse(fixture.thread.is_alive())
        self.assertEqual(fixture.server.socket.fileno(), -1)
        self.assertEqual(report['remaining_resources'], [owned])
        self.assertEqual(report['status'], 'FAIL_TEST')


if __name__ == '__main__':
    unittest.main()
