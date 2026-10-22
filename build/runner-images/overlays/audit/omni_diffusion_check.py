"""Offline content check; actual model quality is verified on the target device."""
import argparse
import ast
import importlib.metadata as metadata
import importlib.util
import io
import json
import os
from pathlib import Path

os.environ['TORCH_DEVICE_BACKEND_AUTOLOAD'] = '0'
p = argparse.ArgumentParser()
p.add_argument('--omni-version', required=True)
p.add_argument('--vendor', choices=['nvidia', 'ascend'], required=True)
a = p.parse_args()
import torch
from diffusers import EulerDiscreteScheduler, EulerAncestralDiscreteScheduler
from PIL import Image
# Transformers' Ascend CLIP import probes torch_npu and needs host driver
# libraries. Validate its source offline; the real model test imports it with
# the NPU runtime mounted. Never report this as a successful driver import.
if a.vendor == 'nvidia':
    from transformers import CLIPTextModel, CLIPTextModelWithProjection, CLIPTokenizer
clip_root = Path(importlib.util.find_spec('transformers').origin).parent / 'models/clip'
for filename, names in [('modeling_clip.py', {'CLIPTextModel', 'CLIPTextModelWithProjection'}),
                        ('tokenization_clip.py', {'CLIPTokenizer'})]:
    definitions = {node.name for node in ast.parse((clip_root / filename).read_text()).body
                   if isinstance(node, ast.ClassDef)}
    assert names <= definitions, (filename, definitions)
versions = {name: metadata.version(name) for name in ['torch', 'vllm', 'vllm-omni', 'diffusers', 'transformers', 'Pillow', 'safetensors']}
for name in ['vllm', 'vllm-omni']:
    assert versions[name].split('+')[0] == a.omni_version, versions
if a.vendor == 'ascend':
    versions['torch-npu'] = metadata.version('torch-npu')
    assert Path('/usr/lib/aarch64-linux-gnu/libjemalloc.so.2').is_file()
root = Path(importlib.util.find_spec('vllm_omni').origin).parent
source = (root/'diffusion/models/sdxl/pipeline_sdxl.py').read_text()
ast.parse(source)
assert 'self.vae.to(dtype=torch.float32)' in source
assert 'self.vae.config.force_upcast' in source
assert 'scheduler_type.from_pretrained(' in source
assert '"EulerAncestralDiscreteScheduler": EulerAncestralDiscreteScheduler' in source
for scheduler in [EulerDiscreteScheduler(), EulerAncestralDiscreteScheduler()]:
    scheduler.set_timesteps(4)
    assert len(scheduler.timesteps) == 4
buf = io.BytesIO()
Image.new('RGB', (16, 16), 'red').save(buf, format='PNG')
with Image.open(io.BytesIO(buf.getvalue())) as im:
    im.load()
    assert im.getpixel((0, 0)) == (255, 0, 0)
print(json.dumps({'profile':'sdxl-turbo', 'versions':versions, 'png_decode':'PASS',
                  'sdxl_vae_upcast':'PASS', 'declared_scheduler':'PASS',
                  'clip_driver_import':'NOT_RUN' if a.vendor == 'ascend' else 'PASS',
                  'gpu_model_acceptance':'NOT_RUN'}, indent=2))
