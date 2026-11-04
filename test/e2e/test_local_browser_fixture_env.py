"""LocalServer child-only environment overrides; no server is launched."""
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import local_browser_fixture
from local_browser_fixture import LocalServer


class LocalServerEnvironmentTest(unittest.TestCase):
    def launch_environment(self, overrides=None, *, auto_check_updates=None):
        captured = []
        with tempfile.TemporaryDirectory() as directory:
            server = LocalServer(Path('/bin/true'), Path(directory), env_overrides=overrides,
                                 auto_check_updates=auto_check_updates)

            def refuse(argv, **kwargs):
                captured.append(kwargs['env'])
                self.launched_config = Path(argv[argv.index('--config') + 1]).read_text()
                raise RuntimeError('test stops before process launch')

            try:
                with mock.patch.object(local_browser_fixture.subprocess, 'Popen', side_effect=refuse):
                    with self.assertRaisesRegex(RuntimeError, 'before process launch'):
                        server.start()
            finally:
                self.assertTrue(server.stop())
            if auto_check_updates is None:
                self.assertEqual(self.launched_config,
                    'listen:\n  console:\n    - "' + server.base.removeprefix('http://') + '"\n'
                    'storage_probe_roots:\n  - "' + str(server.storage_root) + '"\n'
                    'outbound_allow_private_hosts:\n  - "127.0.0.1"\n')
        self.assertEqual(len(captured), 1)
        return captured[0]

    def test_default_inherits_environment_and_keeps_local_test_password(self):
        before = dict(os.environ)
        child = self.launch_environment()
        self.assertEqual(child['PATH'], before['PATH'])
        self.assertEqual(child['GPUWORKS_ADMIN_PASSWORD'], 'gpuworks-test-password-2026')
        self.assertTrue(dict(os.environ) == before, 'default launch changed parent environment')
        self.assertNotIn('auto_check_updates', self.launched_config)

    def test_explicit_update_switch_is_in_private_yaml_before_popen(self):
        before = dict(os.environ)
        for enabled in (False, True):
            with self.subTest(enabled=enabled):
                child = self.launch_environment({'GPUWORKS_USAGE_REPORT': 'off'}, auto_check_updates=enabled)
                self.assertEqual(child['GPUWORKS_USAGE_REPORT'], 'off')
                self.assertTrue(self.launched_config.endswith('auto_check_updates: ' + str(enabled).lower() + '\n'))
                self.assertEqual(self.launched_config.count('auto_check_updates:'), 1)
                self.assertIn('outbound_allow_private_hosts:\n  - "127.0.0.1"\n', self.launched_config)
                self.assertTrue(dict(os.environ) == before, 'per-case settings changed parent environment')
        self.launch_environment()
        self.assertNotIn('auto_check_updates', self.launched_config)

    def test_update_switch_rejects_non_boolean_without_writing_config(self):
        for value in ('false', 0, {}, []):
            with self.subTest(value=value), tempfile.TemporaryDirectory() as directory:
                work = Path(directory)
                with self.assertRaisesRegex(ValueError, 'bool or None'):
                    local_browser_fixture.write_server_config(work, work / 'storage', '127.0.0.1:8800',
                                                              auto_check_updates=value)
                self.assertFalse((work / 'server.yaml').exists())

    def test_override_and_removal_only_affect_popen_environment(self):
        before = dict(os.environ)
        marker = 'GPUWORKS_E2E_PRIVATE_CHILD_MARKER'
        child = self.launch_environment({marker: 'child-only', 'HTTPS_PROXY': 'http://127.0.0.1:9', 'PATH': None})
        self.assertEqual(child[marker], 'child-only')
        self.assertEqual(child['HTTPS_PROXY'], 'http://127.0.0.1:9')
        self.assertNotIn('PATH', child)
        self.assertTrue(dict(os.environ) == before, 'overrides changed parent environment')

    def test_constructor_copies_overrides_without_exposing_other_instances(self):
        with tempfile.TemporaryDirectory() as directory:
            overrides = {'HTTPS_PROXY': 'http://127.0.0.1:9'}
            first = LocalServer(Path('/bin/true'), Path(directory), env_overrides=overrides)
            second = LocalServer(Path('/bin/true'), Path(directory))
            overrides['HTTPS_PROXY'] = 'http://127.0.0.1:10'
            self.assertEqual(first.env_overrides, {'HTTPS_PROXY': 'http://127.0.0.1:9'})
            self.assertEqual(second.env_overrides, {})


if __name__ == '__main__':
    unittest.main()
