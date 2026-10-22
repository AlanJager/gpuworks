"""Acquisition driver binding and independent byte/launch assertion negatives.

Run: python3 -B -m unittest discover -s test/e2e -p test_deployment_acquisition_runner.py
"""
import hashlib
import io
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import tarfile
import unittest
from unittest.mock import patch

import deployment_acquisition as driver
import deployment_acquisition_fixture as fixture
import deployment_acquisition_oracles as oracle
from model_download_fixture import ModelDownload, REVISION


def data(case='vllm-huggingface'):
    engine, provider = case.split('-', 1)
    return {'engine': engine, 'provider': provider, 'run_id': 'case', 'model_id': 7,
        'repo': 'e2e-case/weights', 'template': {'id': 3}, 'node': {'uuid': 'eligible'},
        'unfit': {'uuid': 'unfit'}, 'storages': [{'id': 11}, {'id': 12}],
        'served_name': 'e2e-case-route', 'name': 'e2e-case-deployment', 'hub': ModelDownload('case')}


def record(name, content, *, start=None, end=None, provider='huggingface'):
    span = None if start is None else f'bytes={start}-{end}'
    body = content if start is None else content[start:end + 1]
    return {'method': 'GET', 'target': ('/hf/e2e-case/weights/resolve/' if provider == 'huggingface'
        else '/ms/models/e2e-case/weights/resolve/') + REVISION + '/' + name,
        'range': span, 'status': 200 if start is None else 206, 'complete': True,
        'interrupted': False, 'bytes_sent': len(body), 'sha256_sent': hashlib.sha256(body).hexdigest()}


class AcquisitionRunner(unittest.TestCase):
    def test_each_cli_case_binds_its_own_contract_manifest_and_prerequisites(self):
        for case in fixture.CASES:
            with self.subTest(case=case), patch.object(driver, 'run_case', return_value=0) as run:
                self.assertEqual(driver.main(['--case', case, '--locale', 'zh']), 0)
                args, kwargs = run.call_args
                self.assertEqual(args[0].case, case)
                self.assertEqual(kwargs['contract_id'], 'gpuworks/deployment/acquisition-' + case)
                self.assertEqual(kwargs['manifest'], 'test/e2e/coverage/deployment-acquisition-' + case + '.json')
                self.assertEqual(kwargs['prepare'].keywords, {'case': case})
                self.assertIs(kwargs['cleanup'], fixture.cleanup)
                self.assertIn('test/e2e/model_download_fixture.py', kwargs['helpers'])

    def test_concurrent_provider_edit_uses_overwrite_and_independent_readback(self):
        import deployment_acquisition_browser as browser
        item = data()
        item['template_model_id'] = 'e2e-case/scheme'
        changed = {'specs': [{'weights': {'sources': [{'modelscope': {'repo_id': item['repo']}}]}}]}
        for actual in ('modelscope', 'huggingface'):
            fresh = {'id': 3, 'specs': [{'name': fixture.SPEC, 'source': 'user',
                     'weight_sources': [{'provider': actual}]}]}
            with self.subTest(actual=actual), patch.object(browser, 'http', side_effect=[(200, {}), (200, fresh)]) as request:
                if actual == 'modelscope':
                    browser.replace_owned_template(SimpleNamespace(base='http://local'), 'admin', item, changed)
                else:
                    with self.assertRaisesRegex(AssertionError, 'actual catalog readback provider set'):
                        browser.replace_owned_template(SimpleNamespace(base='http://local'), 'admin', item, changed)
                self.assertEqual(request.call_args_list[0].args[2], fixture.TEMPLATES + '/import?overwrite=true')
                self.assertEqual(request.call_args_list[1].args[1], 'GET')

    def test_console_listener_attributes_only_the_anonymous_session_probe(self):
        import deployment_acquisition_browser as browser
        class Page:
            def __init__(self):
                self.handlers = {}
            def on(self, event, handler):
                self.handlers.setdefault(event, []).append(handler)
        page, report = Page(), {}
        requests, evaluations, session = browser.watch(page, report)
        message = SimpleNamespace(type='error', text='Failed to load resource: status of 401',
            location={'url': 'http://local/api/v1/auth/me'})
        probe = SimpleNamespace(url='http://local/api/v1/auth/me', status=401,
            request=SimpleNamespace(method='GET'))
        page.handlers['console'][0](message)
        page.handlers['response'][0](probe)
        self.assertEqual(report['console_errors'], [])
        self.assertEqual(report['browser_http_errors'], [])
        session['authenticated'] = True
        page.handlers['console'][0](message)
        page.handlers['response'][0](probe)
        self.assertEqual(report['console_errors'], [{'path': '/api/v1/auth/me', 'text': message.text}])
        self.assertEqual(report['browser_http_errors'],
                         [{'method': 'GET', 'path': '/api/v1/auth/me', 'status': 401}])
        with self.assertRaisesRegex(AssertionError, 'unexpected browser HTTP failure'):
            browser.conclude(report)

    def test_conclude_accepts_only_the_attributed_expiry_rejection(self):
        import deployment_acquisition_browser as browser
        expected = {'method': 'POST', 'path': browser.containers.DEPLOYMENTS, 'status': 400}
        clean = {'browser_http_errors': [], 'page_errors': [], 'console_errors': []}
        browser.conclude(clean)
        browser.conclude({**clean, 'browser_http_errors': [expected]})
        for defect in ({'browser_http_errors': [{'method': 'GET', 'path': '/api/v1/models', 'status': 500}]},
                       {'browser_http_errors': [{'method': 'POST', 'path': browser.containers.DEPLOYMENTS,
                                                 'status': 403}]},
                       {'page_errors': ['ReferenceError: drift']},
                       {'console_errors': [{'path': '/deploy', 'text': 'TypeError: boom'}]}):
            with self.subTest(defect=defect), self.assertRaises(AssertionError):
                browser.conclude({**clean, **defect})

    def test_console_echo_of_the_expected_rejection_is_attributed_in_any_order(self):
        import deployment_acquisition_browser as browser
        class Page:
            def __init__(self):
                self.handlers = {}
            def on(self, event, handler):
                self.handlers.setdefault(event, []).append(handler)
        rejection = SimpleNamespace(url='http://local' + browser.containers.DEPLOYMENTS, status=400,
            request=SimpleNamespace(method='POST'))
        echo = SimpleNamespace(type='error', location={'url': 'http://local' + browser.containers.DEPLOYMENTS},
            text='Failed to load resource: the server responded with a status of 400 (Bad Request)')
        for order in ('response-first', 'console-first'):
            with self.subTest(order=order):
                page, report = Page(), {}
                browser.watch(page, report)[2]['authenticated'] = True
                events = {'response-first': ('response', 'console'), 'console-first': ('console', 'response')}[order]
                payloads = {'response': rejection, 'console': echo}
                for event in events:
                    page.handlers[event][0](payloads[event])
                self.assertEqual(len(report['browser_http_errors']), 1)
                self.assertEqual(len(report['console_errors']), 1)
                browser.conclude(report)  # The echo of the attributed rejection must not fail.

    def test_console_resource_echo_without_matching_attributed_response_fails(self):
        import deployment_acquisition_browser as browser
        expected = {'method': 'POST', 'path': browser.containers.DEPLOYMENTS, 'status': 400}
        base = {'browser_http_errors': [expected], 'page_errors': [], 'console_errors': []}
        text = 'Failed to load resource: the server responded with a status of 400 (Bad Request)'
        for echo in ({'path': '/api/v1/models', 'text': text},  # different path
                     {'path': browser.containers.DEPLOYMENTS,
                      'text': 'Failed to load resource: the server responded with a status of 404 (Not Found)'}):
            with self.subTest(echo=echo), self.assertRaisesRegex(AssertionError, 'unexpected browser console errors'):
                browser.conclude({**base, 'console_errors': [echo]})
        # A resource echo with no attributed response at all also fails.
        with self.assertRaisesRegex(AssertionError, 'unexpected browser console errors'):
            browser.conclude({'browser_http_errors': [], 'page_errors': [],
                              'console_errors': [{'path': browser.containers.DEPLOYMENTS, 'text': text}]})

    def test_unknown_cli_case_fails_before_runtime(self):
        with patch.object(driver, 'run_case') as run:
            with self.assertRaises(SystemExit):
                driver.main(['--case', 'callable-url'])
            run.assert_not_called()

    def test_automatic_wire_names_independent_node_gpu_and_destination(self):
        for case in fixture.CASES:
            item = data(case)
            body = oracle.expected_body(item)
            self.assertEqual(body['placement_mode'], 'auto')
            self.assertEqual(body['node_uuid'], 'eligible')
            self.assertEqual(body['placement'], {'gpus': [0], 'tp': 1})
            self.assertEqual(body['model_source'], {'provider': item['provider'], 'storage_id': 12})
            self.assertNotIn('evaluation', item)
            with self.assertRaisesRegex(AssertionError, 'wrong node'):
                oracle.same({**body, 'node_uuid': 'unfit'}, body, 'wrong node')
            with self.assertRaisesRegex(AssertionError, 'omitted storage'):
                oracle.same({**body, 'model_source': {'provider': item['provider']}}, body, 'omitted storage')

    def test_unknown_fake_vllm_release_does_not_state_version_gated_metrics(self):
        body = oracle.expected_body(data())
        self.assertNotIn('enable_request_metrics', body['quick_params'])
        self.assertEqual(body['quick_params']['enable_cache_report'], True)
        # A frontend that spuriously sends a version-sensitive flag is a wire drift.
        changed = dict(body, quick_params={**body['quick_params'], 'enable_request_metrics': True})
        with self.assertRaisesRegex(AssertionError, 'version-gated metrics'):
            oracle.same(changed, body, 'version-gated metrics')

    def test_strict_wire_separates_boolean_number_and_omitted(self):
        for actual in ({'tp': True}, {'tp': '1'}, {}):
            with self.subTest(actual=actual), self.assertRaises(AssertionError):
                oracle.same(actual, {'tp': 1}, 'typed placement')

    def test_actual_disk_bytes_reject_missing_corrupt_and_symlink(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            files = {'config.json': b'{"owned":true}', 'model.safetensors': b'weight'}
            for name, value in files.items():
                (root / name).write_bytes(value)
            self.assertEqual(oracle.file_facts(root, files)['model.safetensors']['bytes'], 6)
            (root / 'model.safetensors').write_bytes(b'WEIGHT')
            with self.assertRaisesRegex(AssertionError, 'acquired file'):
                oracle.file_facts(root, files)
            (root / 'model.safetensors').unlink()
            (root / 'model.safetensors').symlink_to(root / 'config.json')
            with self.assertRaisesRegex(AssertionError, 'regular acquired file'):
                oracle.file_facts(root, files)

    def test_source_actual_bytes_and_ranges_cover_each_file(self):
        for provider in ('huggingface', 'modelscope'):
            item = data('vllm-' + provider)
            audit = []
            for name, value in item['hub'].files.items():
                cut = len(value) // 2
                audit += [record(name, value, start=0, end=cut - 1, provider=provider),
                          record(name, value, start=cut, end=len(value) - 1, provider=provider)]
            self.assertEqual(len(oracle.source_transfer(audit, item)), 4)

    def test_source_does_not_accept_fake_content_length_or_another_provider(self):
        for defect in ('hash', 'bytes', 'incomplete', 'provider', 'revision', 'missing'):
            with self.subTest(defect=defect):
                item = data()
                audit = [record(n, b) for n, b in item['hub'].files.items()]
                if defect == 'hash': audit[-1]['sha256_sent'] = '0' * 64
                if defect == 'bytes': audit[-1]['bytes_sent'] -= 1
                if defect == 'incomplete': audit[-1]['complete'] = False
                if defect == 'provider': audit[-1]['target'] = audit[-1]['target'].replace('/hf/', '/ms/')
                if defect == 'revision': audit[-1]['target'] = audit[-1]['target'].replace(REVISION, 'main')
                if defect == 'missing': audit.pop()
                with self.assertRaises(AssertionError):
                    oracle.source_transfer(audit, item)

    def test_source_ranges_with_gaps_cannot_claim_whole_file(self):
        item = data()
        config, weight = item['hub'].files.values()
        audit = [record('config.json', config), record('model.safetensors', weight, start=1, end=len(weight) - 1)]
        with self.assertRaisesRegex(AssertionError, 'gap'):
            oracle.source_transfer(audit, item)

    def test_docker_cp_requires_successful_single_regular_member(self):
        def archive(kind):
            stream = io.BytesIO()
            with tarfile.open(fileobj=stream, mode='w') as output:
                entry = tarfile.TarInfo('config.json')
                entry.size = 4
                if kind == 'symlink':
                    entry.type, entry.linkname, entry.size = tarfile.SYMTYPE, '/outside', 0
                output.addfile(entry, io.BytesIO(b'data') if entry.isfile() else None)
                if kind == 'multiple': output.addfile(tarfile.TarInfo('extra'))
            return stream.getvalue()
        for kind in ('regular', 'symlink', 'multiple', 'daemon'):
            result = SimpleNamespace(returncode=1 if kind == 'daemon' else 0,
                stdout=archive(kind), stderr=b'no such host' if kind == 'daemon' else b'')
            with self.subTest(kind=kind), patch.object(oracle.subprocess, 'run', return_value=result):
                if kind == 'regular':
                    self.assertEqual(oracle.container_file('owned', '/model/config.json'), b'data')
                else:
                    with self.assertRaises(AssertionError):
                        oracle.container_file('owned', '/model/config.json')

    def test_manifest_source_and_browser_bindings_are_real(self):
        from local_business_runner import source_identity
        for case in fixture.CASES:
            manifest, identity = source_identity('test/e2e/coverage/deployment-acquisition-' + case + '.json',
                'test/e2e/deployment_acquisition.py', driver.exercise,
                ('test/e2e/deployment_acquisition_fixture.py', 'test/e2e/deployment_acquisition_oracles.py'))
            self.assertEqual(manifest['contract_id'], 'gpuworks/deployment/acquisition-' + case)
            self.assertIn('test/e2e/deployment_acquisition_fixture.py', identity['source_hashes'])
            self.assertEqual(set(manifest['required_evidence']), {'ui', 'request', 'persistence', 'execution', 'cleanup'})


if __name__ == '__main__':
    unittest.main()
