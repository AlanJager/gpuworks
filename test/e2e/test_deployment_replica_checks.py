"""Verify strict replica oracles reject the plausible wrong wire/runtime shapes.

Run: PYTHONPATH=test/e2e python3 -m unittest test_deployment_replica_checks
"""
import unittest
from unittest.mock import MagicMock, patch
from types import SimpleNamespace
import deployment_replica_checks as checks
from deployment_replica_checks import integer, bounds, persisted_count, numeric_wire


class ReplicaOracles(unittest.TestCase):
    def test_wire_rejects_coercions_and_records_actual_body(self):
        for wrong in (True, 2.0, '2', None, 0):
            with self.subTest(wrong=wrong), self.assertRaises(AssertionError):
                numeric_wire({}, {'body': {'replicas': wrong}}, 2)
        report = {}
        wire = {'method': 'PUT', 'path': '/api/v1/deployments/7/replicas', 'query': '', 'body': {'replicas': 2}}
        numeric_wire(report, wire, 2)
        self.assertEqual(report['replica_numeric_wire'], [wire])

    def test_persistence_requires_integer_counts_and_distinct_runtime_ids(self):
        base = {'replicas': 2, 'ready_replicas': 2, 'instances': [
            {'replica': {'index': 0}, 'container_id': 'a'},
            {'replica': {'index': 1}, 'container_id': 'b'}]}
        report = {}
        persisted_count(report, base, 2)
        self.assertEqual(report['replica_numeric_persistence'], [{'desired': 2, 'ready': 2, 'containers': ['a', 'b']}])
        for changed in ({**base, 'replicas': 2.0}, {**base, 'ready_replicas': True},
                        {**base, 'instances': base['instances'][:1]},
                        {**base, 'instances': [base['instances'][0], base['instances'][0]]}):
            with self.subTest(changed=changed), self.assertRaises(AssertionError):
                persisted_count({}, changed, 2)

    def test_layout_rejects_absent_zero_and_outside_boxes(self):
        viewport = {'width': 1440, 'height': 900}
        box = {'x': 10, 'y': 10, 'width': 120, 'height': 30}
        bounds(box, viewport, 'input')
        for changed in (None, {**box, 'width': 0}, {**box, 'x': -1},
                        {**box, 'y': 899}, {**box, 'x': 1430}):
            with self.subTest(changed=changed), self.assertRaises(AssertionError):
                bounds(changed, viewport, 'input')

    def test_diagnostic_family_does_not_rewrite_actual_wire(self):
        report = {'check_family': 'notice'}
        wire = {'body': {'replicas': '2'}}
        numeric_wire(report, wire, 2)
        self.assertEqual(wire, {'body': {'replicas': '2'}})
        self.assertNotIn('replica_numeric_wire', report)


class HeldReadLifecycle(unittest.TestCase):
    def run_first_open(self, *, assertion_error=None, continue_error=None, count=1):
        events, report, routes = [], {'check_family': 'first-open'}, []
        page = MagicMock()
        def install(pattern, handler):
            page._handler = handler
        def navigate(url):
            for index in range(count):
                route = SimpleNamespace(request=SimpleNamespace(method='GET', url='http://127.0.0.1/api/v1/deployments/7'))
                route.invalid = False
                def release(current=route, number=index):
                    events.append('continue-' + str(number))
                    if current.invalid:
                        raise RuntimeError('Route is already handled!')
                    if continue_error and number == 0:
                        raise continue_error
                route.continue_ = release
                routes.append(route)
                page._handler(route)
        def remove(pattern, handler):
            events.append('unroute')
            for route in routes:
                route.invalid = True
        page.route.side_effect = install
        page.goto.side_effect = navigate
        page.unroute.side_effect = remove
        assertion = MagicMock()
        if assertion_error:
            assertion.to_have_count.side_effect = assertion_error
        response = SimpleNamespace(status=200, json=lambda: {'id': 7, 'name': 'owned-deployment', 'replicas': 1})
        page.expect_response.return_value.__enter__.return_value = SimpleNamespace(value=response)
        labels = {'detail': {'replicas': 'Replicas'}, 'replica': {'apply': 'Apply'}}
        try:
            with patch.object(checks, 'expect', return_value=assertion), patch.object(checks, 'get_document', return_value={'replicas': 1}):
                checks.first_open(page, SimpleNamespace(base='http://127.0.0.1'), 'secret',
                    {'deployment': {'id': 7}, 'body': {'name': 'owned-deployment'}}, labels, report, [], 1)
        except BaseException as error:
            return events, report, error
        return events, report, None

    def test_success_continues_real_held_read_before_unroute(self):
        events, report, error = self.run_first_open()
        self.assertIsNone(error)
        self.assertEqual(events, ['continue-0', 'unroute'])
        self.assertEqual(report['replica_first_open']['replicas'], 1)

    def test_assertion_failure_releases_before_unroute_and_preserves_failure(self):
        primary = AssertionError('primary loading gate failure')
        events, report, error = self.run_first_open(assertion_error=primary)
        self.assertIs(error, primary)
        self.assertEqual(events, ['continue-0', 'unroute'])
        self.assertNotIn('replica_first_open', report)

    def test_cleanup_attempts_all_reads_and_preserves_primary_error(self):
        primary = AssertionError('primary loading gate failure')
        events, report, error = self.run_first_open(assertion_error=primary,
            continue_error=RuntimeError('transport disconnected'), count=2)
        self.assertIs(error, primary)
        self.assertEqual(events, ['continue-0', 'continue-1', 'unroute'])
        self.assertIn('transport disconnected', str(report['replica_route_cleanup_errors']))
        self.assertNotIn('replica_first_open', report)

    def test_release_failure_without_primary_cannot_pass(self):
        events, report, error = self.run_first_open(continue_error=RuntimeError('transport disconnected'), count=2)
        self.assertIsNotNone(error)
        self.assertEqual(events, ['continue-0', 'continue-1', 'unroute'])
        self.assertNotIn('replica_first_open', report)


if __name__ == '__main__':
    unittest.main()
