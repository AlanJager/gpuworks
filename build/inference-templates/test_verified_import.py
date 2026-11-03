import json
import shutil
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

import yaml
import verified_import as tool

HERE = Path(__file__).resolve().parent


class ImportTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.addCleanup(self.temp.cleanup)

    def mapping(self, source):
        images = tool.image_catalog(source)[0]
        refs = [v['image_ref'] for s in images['series'] for v in s['versions']]
        mapping = {ref: {'ref': f'verified.example/amd:{i}', 'image_id': 'sha256:' + str(i) * 64}
                   for i, ref in enumerate(refs, 1)}
        path = self.root / 'mapping.json'
        path.write_text(json.dumps(mapping))
        return path, mapping

    def test_both_packages_inherit_current_seed_and_rewrite_verified_images(self):
        for name in ('amd395', 'rocm'):
            with self.subTest(package=name):
                source = HERE / name
                path, mapping = self.mapping(source)
                output = self.root / name
                with patch.object(tool.subprocess, 'run') as run:
                    run.side_effect = [Mock(stdout=item['image_id']) for item in mapping.values()]
                    tool.materialize(source, path, output, docker_host='unix:///fixture/socket')
                self.assertEqual(run.call_count, len(mapping))
                for call in run.call_args_list:
                    self.assertEqual(call.args[0][1:3], ['--host', 'unix:///fixture/socket'])
                    self.assertNotIn('DOCKER_CONTEXT', call.kwargs['env'])
                for model_path in (output / 'catalog/models').rglob('model.yaml'):
                    model = tool.load(model_path)
                    original = tool.load(tool.SEED / model['model_id'] / 'model.yaml')
                    for key, value in original.items():
                        if key not in ('source', 'specs'):
                            self.assertEqual(model[key], value)
                    for spec in model['specs']:
                        selected = 'bf16-sglang-nvidia-general' if model['model_id'].endswith('0.6b') else 'bf16-vllm-nvidia-general'
                        fact = next(s for s in original['specs'] if s['name'] == selected)
                        self.assertNotIn('display_profile', spec)
                        self.assertNotIn('display_target', spec['gpus'])
                        self.assertTrue(spec['display_name_en'])
                        self.assertTrue(spec['display_name_zh_cn'])
                        self.assertEqual(spec['weights'], fact['weights'])
                        self.assertEqual(spec['weight_bytes'], fact['weight_bytes'])
                        self.assertTrue(spec['image'].startswith('verified.example/'))
                content = (output / 'image-import.json').read_text()
                self.assertNotIn('IMAGE_MAPPING_REQUIRED', content)
                self.assertIn('Verified local image ID', content)
                self.assertTrue(json.loads((output / 'source-sha256.json').read_text()))

    def test_missing_mapping_bad_digest_and_inspect_failure_leave_no_output(self):
        source = HERE / 'amd395'
        path, mapping = self.mapping(source)
        output = self.root / 'delivery'
        with self.assertRaisesRegex(ValueError, 'explicit'):
            with patch.dict(tool.os.environ, {}, clear=True):
                tool.materialize(source, path, output)
        path.write_text('{}')
        with self.assertRaisesRegex(ValueError, 'exactly'):
            tool.materialize(source, path, output, docker_host='unix:///fixture/socket')
        path.write_text(json.dumps(mapping))
        with patch.object(tool.subprocess, 'run', return_value=Mock(stdout='sha256:' + 'f' * 64)):
            with self.assertRaisesRegex(ValueError, 'image ID mismatch'):
                tool.materialize(source, path, output, docker_host='unix:///fixture/socket')
        with patch.object(tool.subprocess, 'run', side_effect=TimeoutError('inspect timeout')):
            with self.assertRaises(TimeoutError):
                tool.materialize(source, path, output, docker_host='unix:///fixture/socket')
        self.assertFalse(output.exists())
        self.assertFalse(list(self.root.glob('.delivery-*')))

    def test_overlay_cannot_replace_model_or_weight_facts_and_unknown_spec_fails(self):
        source = self.root / 'source'
        shutil.copytree(HERE / 'rocm', source)
        overlay_path = source / json.loads((source / 'import-config.json').read_text())['models'][0]
        original = tool.load(overlay_path)
        for alteration in ('model_config', 'weights', 'seed_spec'):
            overlay = tool.deepcopy(original)
            if alteration == 'model_config':
                overlay[alteration] = {}
            elif alteration == 'weights':
                overlay['specs'][0][alteration] = {}
            else:
                overlay['specs'][0][alteration] = 'removed-upstream-name'
            overlay_path.write_text(yaml.safe_dump(overlay))
            with self.subTest(alteration=alteration), self.assertRaises(ValueError):
                tool.compose(source)

    def test_invalid_mapping_and_engine_metadata_fail_before_inspect(self):
        source = HERE / 'rocm'
        path, original = self.mapping(source)
        for invalid in ('__PLACEHOLDER__:required', 'gpuworks-local/amd:test'):
            mapping = tool.deepcopy(original)
            next(iter(mapping.values()))['ref'] = invalid
            path.write_text(json.dumps(mapping))
            with self.subTest(ref=invalid), patch.object(tool.subprocess, 'run') as run:
                with self.assertRaises(ValueError):
                    tool.materialize(source, path, self.root / 'delivery', docker_host='unix:///fixture/socket')
                run.assert_not_called()
        modified = self.root / 'modified'
        shutil.copytree(source, modified)
        images = tool.image_catalog(modified)[0]
        images['series'][0]['engine'] = 'sglang'
        (modified / 'image-import.json').write_text(json.dumps(images))
        config = json.loads((modified / 'import-config.json').read_text())
        config.pop('images')
        config['image_catalog'] = 'image-import.json'
        (modified / 'import-config.json').write_text(json.dumps(config))
        path.write_text(json.dumps(original))
        with patch.object(tool.subprocess, 'run') as run:
            with self.assertRaisesRegex(ValueError, 'matching image metadata'):
                tool.materialize(modified, path, self.root / 'delivery', docker_host='unix:///fixture/socket')
            run.assert_not_called()

    def test_write_failure_cleans_staging_directory(self):
        source = HERE / 'rocm'
        path, mapping = self.mapping(source)
        output = self.root / 'delivery'
        with patch.object(tool.subprocess, 'run', side_effect=[Mock(stdout=i['image_id']) for i in mapping.values()]), \
                patch.object(tool.yaml, 'safe_dump', side_effect=OSError('disk full')):
            with self.assertRaises(OSError):
                tool.materialize(source, path, output, docker_host='unix:///fixture/socket')
        self.assertFalse(output.exists())
        self.assertFalse(list(self.root.glob('.delivery-*')))


if __name__ == '__main__':
    unittest.main()
