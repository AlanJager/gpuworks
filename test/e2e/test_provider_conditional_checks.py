"""Conditional-check oracle and privacy failures, without a browser or success stub.

PYTHONPATH=test/e2e python3 -m unittest test_provider_conditional_checks -v
"""
import unittest
from types import SimpleNamespace

from provider_conditional_checks import _field_rows, _provider_write, _switch_plan, exercise_conditional_fields


def field(name, *, secret=False, required=False, extra=False, **other):
    return {'name': name, 'label': {'en': name, 'zh': '标签-' + name}, 'secret': secret,
            'required': required, 'extra': extra, **other}


class ConditionalOracleTest(unittest.TestCase):
    def test_secret_defaults_never_become_expected_visible_values(self):
        rows = _field_rows({'credential_fields': [
            field('api_key', secret=True, required=True, default_value='must-not-be-rendered'),
            field('endpoint', default_value='https://example.invalid/v2'),
            field('regions', extra=True, default_value='cn-a'),
        ]}, 'zh')
        self.assertEqual(rows[0], {'path': ['api_key'], 'label': '标签-api_key', 'secret': True,
                                  'required': True, 'type': 'password', 'default': ''})
        self.assertEqual(rows[1]['default'], 'https://example.invalid/v2')
        self.assertEqual(rows[2]['path'], ['extra', 'regions'])
        self.assertEqual(rows[2]['default'], 'cn-a')
        self.assertNotIn('must-not-be-rendered', str(rows))

    def test_base_and_extra_paths_are_distinct_but_duplicates_fail(self):
        rows = _field_rows({'credential_fields': [field('region'), field('region', extra=True)]}, 'en')
        self.assertEqual([row['path'] for row in rows], [['region'], ['extra', 'region']])
        for duplicate in ([field('api_key'), field('api_key')],
                          [field('ssh_password', extra=True), field('ssh_password', extra=True)]):
            with self.subTest(duplicate=duplicate), self.assertRaises(AssertionError):
                _field_rows({'credential_fields': duplicate}, 'en')

    def test_nonboolean_secret_flags_cannot_silently_weaken_the_privacy_oracle(self):
        for wrong in (1, 'false', None):
            with self.subTest(wrong=wrong), self.assertRaises(AssertionError):
                _field_rows({'credential_fields': [field('api_key', secret=wrong)]}, 'en')

    def test_recording_active_rejects_before_accessing_page_or_entering_drafts(self):
        for state in (SimpleNamespace(tracing=True, trace_number=0),
                      SimpleNamespace(tracing=False, trace_number=1)):
            with self.subTest(state=state), self.assertRaisesRegex(AssertionError, 'before start_trace'):
                exercise_conditional_fields(object(), object(), {}, [], 'en', {}, state)

    def test_transition_plan_covers_both_directions_of_shared_field_families(self):
        names = ['aliyun', 'autodl', 'compshare', 'runpod', 'vast']
        for order in (names, list(reversed(names))):
            plan = _switch_plan(order)
            edges = list(zip(['runpod', *plan[:-1]], plan))
            self.assertEqual(set(edges), {(left, right) for left in names for right in names if left != right})
            self.assertEqual(plan[-1], 'runpod')
            self.assertTrue(all(left != right for left, right in edges))
            self.assertIn(('compshare', 'aliyun'), edges)
            self.assertIn(('aliyun', 'compshare'), edges)
            self.assertIn(('autodl', 'vast'), edges)
            self.assertIn(('vast', 'autodl'), edges)

    def test_mutation_observer_covers_nested_routes_without_matching_other_roots(self):
        for method, path, expected in [
            ('POST', '/api/v1/providers', True), ('PATCH', '/api/v1/providers/3', True),
            ('DELETE', '/api/v1/providers/3', True), ('POST', '/api/v1/providers/3/sync', True),
            ('GET', '/api/v1/providers/kinds', False), ('POST', '/api/v1/providers-other', False),
        ]:
            with self.subTest(method=method, path=path):
                self.assertEqual(_provider_write(SimpleNamespace(method=method, url='http://local' + path)), expected)


if __name__ == '__main__':
    unittest.main()
