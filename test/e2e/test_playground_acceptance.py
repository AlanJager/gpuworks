"""Offline runner-contract tests; no browser or GPU coverage.

E2E standard: test/e2e/browser-e2e-testing-standard.md
"""
import copy
from contextlib import redirect_stderr
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch

import yaml

import playground_acceptance as runner


class ManifestGateTest(unittest.TestCase):
    def manifest(self):
        return {'defaults': {'base_url': 'http://example.invalid'}, 'cases': [
            {'id': 'navigation', 'browser': 'chat', 'expect': {'url_regex': '/playground/chat$'}}]}

    def rejected_before_external_actions(self, manifest):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'cases.yaml'
            path.write_text(yaml.safe_dump(manifest))
            artifacts = Path(directory) / 'artifacts'
            with patch('sys.argv', ['runner', '--cases', str(path), '--artifact-dir', str(artifacts)]), \
                    patch.object(runner, 'sync_playwright', side_effect=AssertionError('external action before validation')) as browser, \
                    patch.object(runner, 'login') as login, patch.object(runner, 'deploy') as deploy, \
                    redirect_stderr(io.StringIO()):
                self.assertNotEqual(runner.main(), 0)
                browser.assert_not_called()
                login.assert_not_called()
                deploy.assert_not_called()
            report = json.loads((artifacts / 'report.json').read_text())
            self.assertEqual(report['status'], 'MISSING')
            self.assertFalse(report['external_actions_started'])
            return report

    # Mainline: a later unsupported action/expect/input → reject the entire selection before login.
    def test_unsupported_case_semantics_reject_before_external_actions(self):
        defects = [{'browser': 'typo'}, {'expect': {'sse_ok': True}},
                   {'inputs': {'text': 'must actually be sent'}}, {'raw_patch': {'speed': 1.2}},
                   {'curl': 'replay_copied_curl_with_temporary_api_key'},
                   {'deploy': {'template': 'example/model', 'spec': 'spec', 'gpu': 0}},
                   {'deploy': {'unknown': 'ignored'}}, {'inputs': {}},
                   {'browser': ['chat']},
                   {'reuse': 'silently-ignored-model'}]
        for defect in defects:
            with self.subTest(defect=defect):
                manifest = self.manifest()
                second = copy.deepcopy(manifest['cases'][0])
                second.update(id='unsupported', **defect)
                manifest['cases'].append(second)
                report = self.rejected_before_external_actions(manifest)
                self.assertEqual([c['id'] for c in report['cases']], ['navigation', 'unsupported'])
                self.assertEqual(report['cases'][0]['status'], 'NOT_RUN')
                self.assertEqual(report['cases'][1]['status'], 'MISSING')

    # Mainline: defaults with unconsumed node/GPU selection or malformed inputs → no remote setup.
    def test_unconsumed_defaults_and_malformed_shapes_reject(self):
        for field in ('node_hostname', 'node_uuid', 'model_source', 'chain_dir', 'ready_timeout_seconds'):
            with self.subTest(field=field):
                manifest = self.manifest()
                manifest['defaults'][field] = 'not-consumed'
                report = self.rejected_before_external_actions(manifest)
                self.assertIn(field, json.dumps(report['missing']))
        for value in (None, [], {}, {'defaults': [], 'cases': []},
                      {'cases': [None]}, {'cases': [{'id': '../escape', 'browser': 'chat'}]},
                      {'cases': [{'id': ['invalid'], 'browser': 'chat'}]}):
            with self.subTest(value=value):
                self.rejected_before_external_actions(value)

    # Mainline: duplicate IDs, invalid regex and misleading assertion types → fail before browser startup.
    def test_invalid_expectations_and_duplicate_ids_reject(self):
        for expected in ({}, {'url_regex': '['}, {'url_regex': 1},
                         {'visible_text': 'not-a-list'}, {'visible_text': []},
                         {'visible_text': ['']}):
            with self.subTest(expected=expected):
                manifest = self.manifest()
                manifest['cases'][0]['expect'] = expected
                self.rejected_before_external_actions(manifest)
        manifest = self.manifest()
        manifest['cases'] *= 2
        self.rejected_before_external_actions(manifest)

    # Mainline: regex compiler overflow/deep recursion → MISSING diagnostics before any external action.
    def test_regex_limits_produce_missing_reports(self):
        for pattern in ('a{4294967296}', '(' * 2000 + 'a' + ')' * 2000):
            with self.subTest(pattern_length=len(pattern)):
                manifest = self.manifest()
                manifest['cases'][0]['expect'] = {'url_regex': pattern}
                self.assertIn('expect.url_regex is invalid', runner.case_gaps(manifest['cases'][0]))
                report = self.rejected_before_external_actions(manifest)
                self.assertEqual(report['cases'][0]['status'], 'MISSING')
                self.assertIn('expect.url_regex is invalid', report['cases'][0]['missing'])
                with tempfile.TemporaryDirectory() as directory:
                    path = Path(directory) / 'cases.yaml'
                    path.write_text(yaml.safe_dump(manifest))
                    result = subprocess.run([sys.executable, str(Path(runner.__file__)), '--cases', str(path),
                        '--artifact-dir', directory, '--chrome-executable', '/nonexistent-e2e-chrome'],
                        capture_output=True, text=True, timeout=10)
                    self.assertEqual(result.returncode, 2, result.stderr)
                    saved = json.loads((Path(directory) / 'report.json').read_text())
                    self.assertEqual(saved['status'], 'MISSING')
                    self.assertFalse(saved['external_actions_started'])
                    self.assertNotIn('Traceback', result.stderr)

    # Mainline: unchanged repository matrix → nonzero MISSING report retains every declared case.
    def test_real_manifest_cli_fails_closed_without_browser(self):
        manifest_path = Path(__file__).parent / 'testdata/playground-acceptance/cases.yaml'
        manifest = yaml.safe_load(manifest_path.read_text())
        report = self.rejected_before_external_actions(manifest)
        self.assertEqual([c['id'] for c in report['cases']], [c['id'] for c in manifest['cases']])
        self.assertTrue(all(c['status'] == 'MISSING' for c in report['cases']))
        with tempfile.TemporaryDirectory() as directory:
            result = subprocess.run([sys.executable, str(Path(runner.__file__)), '--cases', str(manifest_path),
                '--artifact-dir', directory, '--chrome-executable', '/nonexistent-e2e-chrome'],
                capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 2, result.stderr)
            self.assertIn('MISSING', result.stderr)
            self.assertNotIn('BrowserType.launch', result.stderr)

    # Mainline: supported navigation asserts the actual destination; no invented request evidence.
    def test_supported_navigation_has_no_copied_input_evidence(self):
        case = self.manifest()['cases'][0]
        page = Mock(url='http://example.invalid/playground/chat')
        with patch.object(runner, 'expect'):
            result = runner.check_case(page, 'http://example.invalid', case, None)
        page.goto.assert_called_once_with('http://example.invalid/playground/chat')
        self.assertNotIn('request', result)
        self.assertEqual(result['scope'], 'navigation-only')
        self.assertEqual(runner.validate_manifest(self.manifest())['status'], 'NOT_RUN')
        with self.assertRaises(ValueError):
            runner.check_case(page, 'http://example.invalid', dict(case, browser='typo'), None)

    # Mainline: existing-deployment smoke bypasses GPU manifest cases and declares only its smoke scope.
    def test_existing_deployment_is_separate_smoke_mode(self):
        path = Path(__file__).parent / 'testdata/playground-acceptance/cases.yaml'
        with tempfile.TemporaryDirectory() as directory, \
                patch('sys.argv', ['runner', '--cases', str(path), '--artifact-dir', directory,
                    '--existing-deployment', 'explicit-fixture', '--existing-model', 'fake-model']), \
                patch.object(runner, 'sync_playwright'), patch.object(runner, 'login'), \
                patch.object(runner, 'validate_manifest') as gate, patch.object(runner, 'deploy') as deploy, \
                patch.object(runner, 'check_existing_deployment', return_value=[{'chat': 'ok'}]) as smoke:
            self.assertEqual(runner.main(), 0)
            gate.assert_not_called()
            deploy.assert_not_called()
            smoke.assert_called_once()
            report = json.loads((Path(directory) / 'report.json').read_text())
            self.assertEqual(report[0]['scope'], 'existing-deployment-renderer-smoke')

    # Mainline: invalid YAML → sanitized MISSING report before startup, without printing secret excerpts.
    def test_invalid_yaml_is_missing_before_browser(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'invalid.yaml'
            path.write_text('defaults: [password: synthetic-secret\n')
            with patch('sys.argv', ['runner', '--cases', str(path), '--artifact-dir', directory]), \
                    patch.object(runner, 'sync_playwright') as browser, redirect_stderr(io.StringIO()) as stderr:
                self.assertEqual(runner.main(), 2)
                browser.assert_not_called()
            self.assertNotIn('synthetic-secret', stderr.getvalue())
            self.assertEqual(json.loads((Path(directory) / 'report.json').read_text())['status'], 'MISSING')

    # Mainline: duplicate YAML keys cannot overwrite an unimplemented contract with supported navigation.
    def test_duplicate_yaml_keys_cannot_hide_missing_semantics(self):
        document = ('defaults: {base_url: "http://example.invalid"}\ncases:\n'
            '- id: duplicate\n  browser: transcribe_upload\n  browser: chat\n'
            '  expect: {url_regex: "/playground/chat$"}\n')
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'duplicate.yaml'
            path.write_text(document)
            with patch('sys.argv', ['runner', '--cases', str(path), '--artifact-dir', directory]), \
                    patch.object(runner, 'sync_playwright') as browser, redirect_stderr(io.StringIO()):
                self.assertEqual(runner.main(), 2)
                browser.assert_not_called()


if __name__ == '__main__':
    unittest.main()
