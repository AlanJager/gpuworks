import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('rocm_build', HERE / 'rocm/build.py')
build = importlib.util.module_from_spec(spec)
spec.loader.exec_module(build)


class BuildTests(unittest.TestCase):
    def test_matrix_is_only_base_and_version_source(self):
        matrix_path = HERE.parent / 'runner-images/matrix.yaml'
        rows = build.load_images(matrix_path, HERE / 'rocm/build-config.json')
        matrix = build.yaml.safe_load(matrix_path.read_text())['items']
        self.assertEqual(len(rows), 2)
        for row in rows:
            original = next(i for i in matrix if i['id'] == row['id'])
            self.assertEqual(row['base'], original['base'])
            self.assertEqual(row['args'], original['args'])
        with tempfile.TemporaryDirectory() as temp:
            config = Path(temp) / 'config.json'
            config.write_text(json.dumps({'removed-matrix-id': 'gpuworks-local/amd:test'}))
            with self.assertRaisesRegex(ValueError, 'unknown'):
                build.load_images(matrix_path, config)

    def test_missing_endpoint_and_shared_root_stop_before_build(self):
        with tempfile.TemporaryDirectory() as temp:
            with patch.dict(os.environ, {}, clear=True), patch('sys.argv', ['build.py', '--output', temp]), patch.object(build, 'run') as run:
                with self.assertRaises(SystemExit):
                    build.main()
                run.assert_not_called()
            env = {'DOCKER_HOST': 'unix:///fixture/socket', 'DOCKER_CONTEXT': 'unrelated'}
            with patch.dict(os.environ, env, clear=True), patch('sys.argv', ['build.py', '--output', temp]), patch.object(build, 'run', return_value=subprocess.CompletedProcess([], 0, '/var/lib/docker\n')) as run:
                with self.assertRaises(SystemExit):
                    build.main()
                self.assertEqual(run.call_count, 1)
                self.assertEqual(run.call_args.args[0][1:3], ['--host', env['DOCKER_HOST']])
                self.assertNotIn('DOCKER_CONTEXT', os.environ)


if __name__ == '__main__':
    unittest.main()
