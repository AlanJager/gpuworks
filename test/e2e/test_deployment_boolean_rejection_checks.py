"""Offline fail-closed oracles; these are not browser/persistence evidence."""
import copy
import unittest
from unittest.mock import patch

import deployment_boolean_rejection_checks as checks


def body(acquisition=False):
    return {'name': 'fixture-owned', 'engine': 'sglang' if acquisition else 'vllm',
            'quick_params': {'enable_cache_report': True, 'dtype': 'auto'},
            'container_overrides': {'env': {'E2E_ACQUISITION' if acquisition else 'E2E_WIZARD': 'owned'},
                                    'extra_mounts': [{'host_path': '/fixture/ro', 'container_path': '/e2e-ro', 'purpose': 'data', 'read_only': True},
                                                     {'host_path': '/fixture/rw', 'container_path': '/e2e-rw', 'purpose': 'data', 'read_only': False}]},
            'model_source': {'provider': 'huggingface', 'storage_id': 3}}


MOUNT = checks.declared_mount('/fixture/weights')

def report(family='all'):
    return {'deployment_check_family': family, 'boolean_rejection': {'pending_mount': copy.deepcopy(MOUNT)}}


class ImmutableDraftOracles(unittest.TestCase):
    def test_each_literal_baseline_restores_without_assuming_auto_or_true(self):
        for cache in (False, True):
            for prefix in ('omitted', False, True):
                value = body()
                value['quick_params']['enable_cache_report'] = cache
                if prefix != 'omitted':
                    value['quick_params']['enable_prefix_caching'] = prefix
                untouched = copy.deepcopy(value)
                expected = {'enable_cache_report': 'on' if cache else 'off',
                            'enable_prefix_caching': {'omitted': 'auto', False: 'off', True: 'on'}[prefix]}
                self.assertEqual(checks.plan(value)['restore'], expected)
                seeded = checks.seeded_body(value, MOUNT)
                self.assertIs(seeded['quick_params']['enable_cache_report'], False)
                self.assertIs(seeded['quick_params']['enable_prefix_caching'], True)
                self.assertEqual(value, untouched)
                self.assertEqual(seeded['container_overrides']['extra_mounts'], untouched['container_overrides']['extra_mounts'] + [MOUNT])

    def test_unrestorable_cache_omission_and_coerced_booleans_are_blockers(self):
        value = body()
        del value['quick_params']['enable_cache_report']
        with self.assertRaisesRegex(AssertionError, 'cannot restore original omission'):
            checks.plan(value)
        for key in (checks.CACHE, checks.PREFIX):
            for wrong in (None, 0, 1, '', 'false'):
                value = body()
                value['quick_params'][key] = wrong
                with self.subTest(key=key, wrong=wrong), self.assertRaises(AssertionError):
                    checks.plan(value)

    def test_only_the_bound_bad_create_changes_not_success_or_delete(self):
        invalid = body()
        invalid['name'] = checks.INVALID_NAME
        original = [{'method': 'POST', 'path': '/api/v1/deployments', 'query': '', 'body': invalid},
                    {'method': 'POST', 'path': '/api/v1/deployments', 'query': '', 'body': body()},
                    {'method': 'DELETE', 'path': '/api/v1/deployments/7', 'query': '', 'body': None}]
        untouched = copy.deepcopy(original)
        changed = checks.expected_mutations(original, report())
        self.assertEqual(changed[0]['body'], checks.seeded_body(invalid, MOUNT))
        self.assertEqual(changed[1:], original[1:])
        self.assertEqual(original, untouched)
        self.assertEqual(checks.expected_mutations(original, {'deployment_check_family': 'launch-contract'}), original)
        for field, wrong in [('method', 'PUT'), ('query', 'unsafe=1'), ('path', '/api/v1/models')]:
            malformed = copy.deepcopy(original)
            malformed[0][field] = wrong
            with self.subTest(field=field), self.assertRaises(AssertionError):
                checks.expected_mutations(malformed, report(checks.FAMILY))

    def test_real_failure_contract_rejects_wire_error_and_status_drift(self):
        for acquisition, kind in ((False, 'name'), (True, 'provider')):
            original = body(acquisition)
            bad, error = checks.refusal(original, kind, MOUNT)
            record = {'method': 'POST', 'path': '/api/v1/deployments', 'query': '',
                      'body': bad, 'status': 400, 'response': error}
            checks.assert_refusal(record, original, kind, MOUNT)
            for changed in ('bool-lost', 'extra-key', 'response', 'status', 'query', 'method'):
                wrong = copy.deepcopy(record)
                if changed == 'bool-lost': del wrong['body']['quick_params'][checks.CACHE]
                elif changed == 'extra-key': wrong['body']['quick_params']['invented'] = True
                elif changed == 'response': wrong['response']['error'] = 'generic error'
                elif changed == 'status': wrong['status'] = 201
                elif changed == 'query': wrong['query'] = 'retry=1'
                else: wrong['method'] = 'PUT'
                with self.subTest(kind=kind, changed=changed), self.assertRaises(AssertionError):
                    checks.assert_refusal(wrong, original, kind, MOUNT)
            with self.assertRaisesRegex(AssertionError, 'fixed fixture save-refusal trigger'):
                checks.refusal(original, 'name' if acquisition else 'provider', MOUNT)

    def test_complete_pending_universe_cannot_shrink_to_observed_controls(self):
        for engine, count in (('vllm', 30), ('sglang', 24)):
            records = [{'id': key, 'offered': True} for key in checks.pending_ids(engine)]
            self.assertEqual(len(records), count)
            checks.complete_pending(records, engine)
            for wrong in (records[:-1], records + [records[0]],
                          [{**row, 'offered': False} for row in records],
                          [{**row, 'offered': 1} for row in records]):
                with self.subTest(engine=engine), self.assertRaises(AssertionError):
                    checks.complete_pending(wrong, engine)

    def test_error_stage_survives_field_assertion_failure(self):
        report = {}
        def failure():
            report['last_step'] = 'old-shared-field-stage'
            raise AssertionError('pending boolean reset')
        with self.assertRaisesRegex(AssertionError, 'pending boolean reset'):
            checks._at(report, 'boolean-rejection-retained-prefix-expert', failure)
        self.assertEqual(report['last_step'], 'boolean-rejection-retained-prefix-expert')

    def test_retained_orchestration_never_calls_explicit_evaluate_while_provider_expired(self):
        # Matrix correctness is separately fail-closed above and requires browser
        # proof. This unit guards the expired-window orchestration boundary only.
        from types import SimpleNamespace
        original = body(True)
        bad, error = checks.refusal(original, 'provider', MOUNT)
        response = SimpleNamespace(url='http://owned/api/v1/deployments', status=400,
            request=SimpleNamespace(method='POST', post_data_json=bad), json=lambda: error)
        state = report(checks.FAMILY)
        common = {'apiError': {'ACQUISITION_CANDIDATE_STALE': 'expired'}}
        labels = {'action': {'createFailed': 'Failed: {{detail}}'}}
        with patch.object(checks, '_matrix') as matrix, patch.object(checks, '_empty') as empty, \
             patch.object(checks, '_common', return_value=common), \
             patch.object(checks, 'rejected_notice'), patch.object(checks, '_wire') as wire:
            checks.retained(object(), labels, original, state, 'token', response, 'provider')
        wire.assert_not_called()
        self.assertEqual(matrix.call_count, 1)
        self.assertEqual(empty.call_count, 2)
        self.assertEqual(state['boolean_rejection']['rejected_wire']['body'], bad)

    def test_declared_pending_mount_is_exact_and_never_learns_from_observation(self):
        literal = {'host_path': '/fixture/weights', 'container_path': '/e2e-pending-ro',
                   'purpose': 'data', 'read_only': True}
        self.assertEqual(checks.declared_mount('/fixture/weights'), literal)
        for key, wrong in [('purpose', None), ('purpose', 'models'), ('read_only', False),
                           ('read_only', 1), ('container_path', '/model'), ('host_path', 'relative')]:
            value = {**literal, key: wrong}
            with self.subTest(key=key, wrong=wrong), self.assertRaises(AssertionError):
                checks.seeded_body(body(), value)
        for value in ('/', '', 'relative', '/a/../b', '/a//b', '/a/'):
            with self.subTest(value=value), self.assertRaises(AssertionError):
                checks.declared_mount(value)

    def test_real_refusal_rejects_mount_reset_path_purpose_and_original_row_loss(self):
        for acquisition in (False, True):
            original = body(acquisition)
            for baseline in ('two', 'omitted'):
                if baseline == 'omitted':
                    original['container_overrides'].pop('extra_mounts', None)
                kind = 'provider' if acquisition else 'name'
                bad, error = checks.refusal(original, kind, MOUNT)
                record = {'method': 'POST', 'path': '/api/v1/deployments', 'query': '',
                          'body': bad, 'status': 400, 'response': error}
                checks.assert_refusal(record, original, kind, MOUNT)
                for mutation in ('read_only', 'purpose', 'host', 'target', 'missing', 'extra'):
                    wrong = copy.deepcopy(record)
                    mounts = wrong['body']['container_overrides']['extra_mounts']
                    if mutation == 'read_only': mounts[-1]['read_only'] = False
                    elif mutation == 'purpose': mounts[-1].pop('purpose')
                    elif mutation == 'host': mounts[-1]['host_path'] = '/other'
                    elif mutation == 'target': mounts[-1]['container_path'] = '/model'
                    elif mutation == 'missing': mounts.pop(0)
                    else: mounts.append(copy.deepcopy(MOUNT))
                    with self.subTest(acquisition=acquisition, baseline=baseline, mutation=mutation), self.assertRaises(AssertionError):
                        checks.assert_refusal(wrong, original, kind, MOUNT)

    def test_original_mount_order_and_count_cannot_be_silently_generalized(self):
        original = body()
        for mounts in ([original['container_overrides']['extra_mounts'][0]],
                       list(reversed(original['container_overrides']['extra_mounts'])),
                       [*original['container_overrides']['extra_mounts'], copy.deepcopy(MOUNT)]):
            wrong = copy.deepcopy(original)
            wrong['container_overrides']['extra_mounts'] = mounts
            with self.assertRaises(AssertionError):
                checks.seeded_body(wrong, MOUNT)

    def test_complete_matrix_requires_original_draft_conditional_checkbox_entry(self):
        for engine in ('vllm', 'sglang'):
            records = [{'id': key, 'offered': True} for key in checks.pending_ids(engine)]
            wrong = [row for row in records if row['id'] != 'conditional/mount-read-only']
            with self.assertRaisesRegex(AssertionError, 'complete pending boolean matrix'):
                checks.complete_pending(wrong, engine)

    def test_expiry_guard_detects_real_request_events_and_always_removes_listener(self):
        from types import SimpleNamespace
        class Page:
            def __init__(self): self.listeners = {}
            def on(self, name, fn): self.listeners[name] = fn
            def remove_listener(self, name, fn):
                self.assert_listener = self.listeners.pop(name) is fn
        for request_path, fails in (('/api/v1/engines', False), ('/api/v1/evaluate', True)):
            page, state = Page(), report()
            def observe():
                with checks.expiry_guard(page, state):
                    page.listeners['request'](SimpleNamespace(method='POST', url='http://owned' + request_path,
                                                               post_data_json={'served_name': 'pending'}))
            if fails:
                with self.assertRaisesRegex(AssertionError, 'while provider expired'): observe()
            else:
                observe()
            self.assertEqual(page.listeners, {})
            self.assertTrue(page.assert_listener)
            self.assertEqual(len(state['boolean_rejection']['expired_window_evaluations']), int(fails))
        page, state = Page(), report()
        with self.assertRaisesRegex(RuntimeError, 'original provider restore failed'):
            with checks.expiry_guard(page, state):
                page.listeners['request'](SimpleNamespace(method='POST', url='http://owned/api/v1/evaluate', post_data_json={}))
                raise RuntimeError('original provider restore failed')
        self.assertEqual(page.listeners, {})
        self.assertEqual(len(state['boolean_rejection']['expired_window_evaluations']), 1)


if __name__ == '__main__':
    unittest.main()
