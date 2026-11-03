"""Build AMD import packages from current seed facts and explicit local images."""
import argparse
from copy import deepcopy
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile

import yaml
import runner_matrix

DIGEST = re.compile(r"sha256:[0-9a-f]{64}\Z")
IMAGE_REF = re.compile(r"[a-z0-9][a-z0-9._:/-]+(?:@sha256:[0-9a-f]{64})?\Z")
LAUNCH_FIELDS = {'name', 'display_name_zh_cn', 'display_name_en', 'scenario', 'engine', 'image', 'gpus',
                 'entrypoint', 'args', 'env', 'placement', 'api'}
REPO = Path(__file__).resolve().parents[2]
SEED = REPO / 'internal/server/inferencecatalog/seed/catalog/models'


def load(path):
    return yaml.safe_load(Path(path).read_text())


def compose(source, seed=SEED):
    """Only model identity selects seed; overlays cannot replace model facts."""
    config_bytes = (source / 'import-config.json').read_bytes()
    config = json.loads(config_bytes)
    if (set(config) not in ({'models', 'images'}, {'models', 'image_catalog'})
            or not config['models']):
        raise ValueError('import config needs model overlay paths')
    models, provenance = [], {'import-config.json': hashlib.sha256(config_bytes).hexdigest()}
    for relative in config['models']:
        path = (source / relative).resolve()
        if not path.is_relative_to(source.resolve()):
            raise ValueError('overlay path escapes package')
        overlay_bytes = path.read_bytes()
        overlay = yaml.safe_load(overlay_bytes)
        if set(overlay) != {'model_id', 'specs'}:
            raise ValueError('overlay may only contain model_id and launch specs')
        mid = overlay['model_id']
        if not re.fullmatch(r'[a-z0-9._-]+/[a-z0-9._-]+', mid):
            raise ValueError('invalid model ID')
        seed_path = seed / mid / 'model.yaml'
        seed_bytes = seed_path.read_bytes()
        original = yaml.safe_load(seed_bytes)
        if original['model_id'] != mid:
            raise ValueError('seed model identity mismatch')
        model = {k: deepcopy(v) for k, v in original.items() if k != 'specs'}
        model['source'] = 'user'
        model['specs'] = []
        names = set()
        for recipe in overlay['specs']:
            if set(recipe) - (LAUNCH_FIELDS | {'seed_spec'}):
                raise ValueError('overlay cannot replace model or weight facts')
            candidates = [s for s in original['specs'] if s['name'] == recipe['seed_spec']]
            if len(candidates) != 1:
                raise ValueError(f"unknown or ambiguous seed_spec: {recipe['seed_spec']}")
            facts = candidates[0]
            if facts['weights']['quantization'] != 'bf16':
                raise ValueError('AMD launch overlay requires BF16 source weights')
            spec = {k: deepcopy(v) for k, v in recipe.items() if k != 'seed_spec'}
            if spec['name'] in names or spec['engine'] not in ('vllm', 'sglang'):
                raise ValueError('duplicate spec name or unsupported engine')
            if spec['gpus'].get('vendor') != 'amd' or not spec['gpus'].get('arch_id'):
                raise ValueError('AMD overlay requires explicit vendor and architecture')
            if 'display_target' in spec['gpus']:
                raise ValueError('use explicit display names instead of retired display_target')
            names.add(spec['name'])
            spec['weights'] = deepcopy(facts['weights'])
            spec['weight_bytes'] = facts['weight_bytes']
            model['specs'].append(spec)
        if not model['specs']:
            raise ValueError('model overlay has no specs')
        models.append(model)
        for label, content in [('seed/' + mid, seed_bytes), ('overlay/' + mid, overlay_bytes)]:
            provenance[label] = hashlib.sha256(content).hexdigest()
    return models, provenance


def image_catalog(source):
    """Historical imports own metadata; rebuildable candidates only name matrix IDs."""
    config = json.loads((source / 'import-config.json').read_text())
    if 'image_catalog' in config:
        path = (source / config['image_catalog']).resolve()
        if not path.is_relative_to(source.resolve()):
            raise ValueError('image metadata path escapes package')
        content = path.read_bytes()
        return json.loads(content), {config['image_catalog']: hashlib.sha256(content).hexdigest()}
    matrix_path = REPO / 'build/runner-images/matrix.yaml'
    content = matrix_path.read_bytes()
    matrix = yaml.safe_load(content)
    series = []
    for item in config['images']:
        if set(item) != {'matrix_id', 'placeholder'}:
            raise ValueError('image configuration may only reference matrix ID and placeholder')
        row = runner_matrix.select(matrix, item['matrix_id'])
        series.append({'name': row['engine'] + ' · ROCm local validation',
                       'engine': row['engine'], 'purpose': 'inference', 'framework': 'rocm',
                       'source': 'custom', 'versions': [{
                           'name': row['id'] + '-' + row['revision'],
                           'image_ref': item['placeholder'],
                           'engine_version': runner_matrix.engine_version(row),
                           'framework_version': row['args']['FRAMEWORK_VERSION'],
                           'arch': 'amd64',
                           'notes': 'Unpublished MI300X candidate; hardware validation pending.'}]})
    return {'series': series}, {'runner-matrix.yaml': hashlib.sha256(content).hexdigest()}


def materialize(source, mapping_path, output, docker='docker', docker_host=None, seed=SEED):
    source, output = Path(source), Path(output)
    endpoint = docker_host or os.environ.get('DOCKER_HOST')
    if not endpoint:
        raise ValueError('explicit --docker-host or DOCKER_HOST is required')
    if output.exists():
        raise ValueError(f'output already exists: {output}')
    models, provenance = compose(source, seed)
    images, image_provenance = image_catalog(source)
    provenance.update(image_provenance)
    image_engines = {(v['image_ref'], s['engine']) for s in images['series'] for v in s['versions']}
    placeholders = {ref for ref, _ in image_engines}
    if not all(re.fullmatch(r'__[A-Z0-9_]+_IMAGE_MAPPING_REQUIRED__:required', p) for p in placeholders):
        raise ValueError('source image catalog requires unpullable placeholders')
    mapping = json.loads(Path(mapping_path).read_text())
    if set(mapping) != placeholders:
        raise ValueError('mapping must contain exactly the source image placeholders')
    for model in models:
        for spec in model['specs']:
            if (spec['image'], spec['engine']) not in image_engines:
                raise ValueError('launch spec has no matching image metadata')
    env = dict(os.environ)
    env.pop('DOCKER_CONTEXT', None)
    for placeholder, item in mapping.items():
        if not isinstance(item, dict) or set(item) != {'ref', 'image_id'}:
            raise ValueError(f'mapping for {placeholder} needs ref and image_id')
        ref, expected = item['ref'], item['image_id']
        if (not isinstance(ref, str) or not IMAGE_REF.fullmatch(ref)
                or ref.startswith(('gpuworks-local/', 'docker.io/zhangckid/'))
                or not isinstance(expected, str) or not DIGEST.fullmatch(expected)):
            raise ValueError(f'invalid image mapping for {placeholder}')
        actual = subprocess.run([docker, '--host', endpoint, 'image', 'inspect', ref,
                                 '--format', '{{.Id}}'], capture_output=True, text=True,
                                check=True, env=env, timeout=30).stdout.strip()
        if actual != expected:
            raise ValueError(f'image ID mismatch for {placeholder}: expected {expected}, found {actual}')
    for model in models:
        for spec in model['specs']:
            spec['image'] = mapping[spec['image']]['ref']
    for series in images['series']:
        series['source'] = 'custom'
        for version in series['versions']:
            item = mapping[version['image_ref']]
            version['image_ref'] = item['ref']
            version['notes'] = (version.get('notes', '') + ' Verified local image ID: ' + item['image_id']).strip()
    output.parent.mkdir(parents=True, exist_ok=True)
    temporary = Path(tempfile.mkdtemp(prefix='.' + output.name + '-', dir=output.parent))
    try:
        catalog = temporary / 'catalog'
        catalog.mkdir()
        for model in models:
            dest = catalog / 'models' / model['model_id'] / 'model.yaml'
            dest.parent.mkdir(parents=True)
            dest.write_text(yaml.safe_dump(model, sort_keys=False, allow_unicode=True))
        (catalog / 'manifest.yaml').write_text(yaml.safe_dump({
            'schema_version': 1, 'published_at': '2026-11-03T00:00:00Z',
            'models': len(models), 'specs': sum(len(m['specs']) for m in models)}))
        for name, value in [('image-import.json', images), ('image-mapping.json', mapping),
                            ('source-sha256.json', provenance)]:
            (temporary / name).write_text(json.dumps(value, indent=2) + '\n')
        temporary.rename(output)
    finally:
        if temporary.exists():
            shutil.rmtree(temporary)


def main(source):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--mapping', required=True, type=Path)
    parser.add_argument('--output', required=True, type=Path)
    parser.add_argument('--docker', default='docker')
    parser.add_argument('--docker-host')
    args = parser.parse_args()
    materialize(source, args.mapping, args.output, args.docker, args.docker_host)
