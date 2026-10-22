"""Derive a small single-device TTS config from the pinned upstream asset."""
import argparse
import importlib.util
from pathlib import Path
import yaml

parser = argparse.ArgumentParser()
parser.add_argument('--vendor', required=True, choices=['nvidia', 'ascend'])
args = parser.parse_args()
spec = importlib.util.find_spec('vllm_omni')
source = Path(spec.origin).parent / 'deploy/qwen3_tts.yaml'
config = yaml.safe_load(source.read_text())
if [stage['stage_id'] for stage in config['stages']] != [0, 1]:
    raise RuntimeError('upstream TTS stage contract changed')
for stage in config['stages']:
    stage.update(max_num_seqs=1, gpu_memory_utilization=0.2 if args.vendor == 'nvidia' else 0.12,
                 enforce_eager=True, devices='0')
    stage['compilation_config'] = {'mode': 0, 'cudagraph_mode': 'NONE'}
# The upstream NPU overlay enables graphs; this small, portable smoke profile
# deliberately uses eager execution on both stages.
config.pop('platforms', None)
Path('/opt/gpuworks/qwen3-tts.yaml').write_text(yaml.safe_dump(config, sort_keys=False))
