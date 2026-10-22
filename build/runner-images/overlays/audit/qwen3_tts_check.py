"""Offline checks for the small TTS profile. Hardware/semantic tests are separate."""
import argparse
import importlib
import importlib.metadata as metadata
import json
import os
from pathlib import Path
import platform
import shutil
import subprocess
import sysconfig
import tempfile

parser = argparse.ArgumentParser()
parser.add_argument('--omni-version', required=True)
parser.add_argument('--vendor', required=True, choices=['nvidia', 'ascend'])
args = parser.parse_args()
packages = ['vllm', 'vllm-omni', 'torch', 'transformers', 'diffusers', 'av', 'soundfile',
            'accelerate', 'imageio-ffmpeg', 'onnxruntime']
if args.vendor == 'ascend':
    packages += ['vllm-ascend', 'torch-npu', 'triton-ascend']
versions = {name: metadata.version(name) for name in packages}
for name in ['vllm', 'vllm-omni']:
    if versions[name].split('+')[0] != args.omni_version:
        raise RuntimeError(f'{name} version mismatch: {versions[name]}')
# NPU driver mounts intentionally do not exist during an image build. Disable
# torch's device backend autoload only for these CPU-side ABI/import checks.
os.environ['TORCH_DEVICE_BACKEND_AUTOLOAD'] = '0'
imports = {}
for name in ['torch', 'transformers', 'diffusers', 'av', 'soundfile']:
    importlib.import_module(name)
    imports[name] = 'PASS'
import soundfile as sf
import numpy as np
with tempfile.TemporaryDirectory(prefix='gpuworks-omni-check-') as work:
    work = Path(work)
    audio = work / 'check.wav'
    sf.write(str(audio), np.array([0.1, -0.1] * 12000), 24000, subtype='PCM_16')
    decoded, rate = sf.read(str(audio))
    if rate != 24000 or len(decoded) != 24000 or not np.max(np.abs(decoded)) > 0.09:
        raise RuntimeError('WAV codec round trip failed')
    gcc = shutil.which('gcc')
    if not gcc or not shutil.which('g++') or not Path(sysconfig.get_path('include'), 'Python.h').is_file():
        raise RuntimeError('runtime JIT compiler or Python headers missing')
    source, binary = work / 'check.c', work / 'check'
    source.write_text('int main(void) { return 0; }\n')
    subprocess.run([gcc, str(source), '-o', str(binary)], check=True, timeout=30)
    subprocess.run([str(binary)], check=True, timeout=10)
if args.vendor == 'ascend' and not Path('/usr/lib/aarch64-linux-gnu/libjemalloc.so.2').is_file():
    raise RuntimeError('Ascend import subprocess requires the tested jemalloc library')
print(json.dumps({'profile': 'qwen3-tts', 'versions': versions, 'imports': imports,
                  'platform': platform.machine(), 'compiler_smoke': 'PASS',
                  'wav_codec': 'PASS', 'gpu_model_acceptance': 'NOT_RUN'}, indent=2))
