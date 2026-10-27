"""URL helper cleanup failure tests; no product, browser, cloud or build.

Run: PYTHONPATH=test/e2e python3 -B -m unittest test_provider_url_checks -v
"""
import unittest
from unittest import mock

import provider_url_checks as case


class URLFixtureCleanupTest(unittest.TestCase):
    def fake_fixture(self, *, alive=False, listener=-1):
        fixture = mock.MagicMock(base='http://127.0.0.1:9')
        fixture.thread.is_alive.return_value = alive
        fixture.server.socket.fileno.return_value = listener
        return fixture

    def test_body_failure_closes_actual_listener_without_erasing_database_residual(self):
        owned = {'kind': 'provider', 'id': 42, 'scope': 'isolated DB'}
        report = {'status': 'FAIL_TEST', 'remaining_resources': [owned]}
        with self.assertRaisesRegex(RuntimeError, 'injected business failure'):
            with case._fixture('synthetic-url-only-key', report, 'body-failure') as fixture:
                self.assertTrue(fixture.thread.is_alive())
                self.assertNotEqual(fixture.server.socket.fileno(), -1)
                raise RuntimeError('injected business failure')
        self.assertFalse(fixture.thread.is_alive())
        self.assertEqual(fixture.server.socket.fileno(), -1)
        self.assertEqual(report['status'], 'FAIL_TEST')
        self.assertEqual(report['remaining_resources'], [owned])
        self.assertEqual(report['provider_url_fixture_cleanup']['body-failure'],
                         {'loop_stopped': True, 'listener_closed': True, 'failures': []})

    def test_shutdown_error_still_attempts_listener_close_and_loop_join(self):
        fixture = self.fake_fixture()
        fixture.server.shutdown.side_effect = OSError('injected shutdown failure')
        report = {'status': 'NOT_RUN'}
        with self.assertRaisesRegex(AssertionError, 'cleanup failed'):
            case.close_fixture(fixture, report, 'shutdown-failure')
        fixture.server.shutdown.assert_called_once()
        fixture.server.server_close.assert_called_once()
        fixture.thread.join.assert_called_once_with(timeout=5)
        self.assertEqual(report['status'], 'FAIL_TEST')
        self.assertEqual(report['provider_url_fixture_cleanup']['shutdown-failure']['failures'][0]['stage'], 'shutdown')

    def test_successful_close_calls_do_not_hide_a_live_fixture_or_drop_owned_provider(self):
        fixture = self.fake_fixture(alive=True, listener=19)
        owned = {'kind': 'provider', 'id': 42}
        report = {'status': 'NOT_RUN', 'remaining_resources': [owned]}
        with self.assertRaisesRegex(AssertionError, 'cleanup failed'):
            case.close_fixture(fixture, report, 'still-live')
        self.assertEqual(report['status'], 'FAIL_TEST')
        self.assertEqual(report['remaining_resources'][0], owned)
        self.assertEqual(report['remaining_resources'][1]['kind'], 'fixture')
        self.assertEqual(report['remaining_resources'][1]['endpoint'], fixture.base)
        self.assertFalse(report['provider_url_fixture_cleanup']['still-live']['loop_stopped'])
        self.assertFalse(report['provider_url_fixture_cleanup']['still-live']['listener_closed'])

    def test_start_failure_closes_bound_socket_without_shutdown_on_unstarted_loop(self):
        fixture = self.fake_fixture()
        fixture.__enter__.side_effect = RuntimeError('injected start failure')
        fixture.requests = []
        report = {'status': 'NOT_RUN'}
        with mock.patch.object(case, 'RunPodFixture', return_value=fixture), \
                self.assertRaisesRegex(RuntimeError, 'injected start failure'):
            with case._fixture('synthetic-url-only-key', report, 'start-failure'):
                self.fail('unstarted fixture entered business')
        fixture.server.server_close.assert_called_once()
        fixture.server.shutdown.assert_not_called()
        fixture.thread.join.assert_not_called()
        self.assertEqual(report['provider_url_fixture_cleanup']['start-failure'],
                         {'loop_stopped': True, 'listener_closed': True, 'failures': []})


if __name__ == '__main__':
    unittest.main()
