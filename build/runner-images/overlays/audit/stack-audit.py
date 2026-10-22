"""Offline component gate. Never equates imports or metadata with GPU acceptance.

Origin: extracted from a colleague's gpuworks-runner image (Gen3, layer 37); see
../../NOTICE and artifacts/work/2026-10-03-runner-images/research/upstream-image-diff.md.

gpuworks modification (r2): GPUWORKS_PIP_CHECK_BASELINE, an optional path to the `pip check`
output of the pristine base image (recorded by the Dockerfile before any pip operation).
Lines that also appear in the baseline are the base's own, known problems (e.g. upstream
vLLM overrides torch's pinned NCCL; the arm64 base ships "nvidia-cusparselt-cu13 0.8.1 is
not supported on this platform"): they are recorded under `pip_check_allowed` instead of
failing the gate. A line that is NOT in the baseline is a new problem and still fails.
Without the variable, any non-zero `pip check` fails (the original behaviour).
Run `python3 stack-audit.py --self-test` for the built-in check of that classification.
"""
import glob
import importlib.metadata as metadata
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import sys
import sysconfig
import tempfile


def command(argv, timeout=90):
    try:
        p = subprocess.run(argv, capture_output=True, text=True, timeout=timeout)
        return {'exit': p.returncode, 'output': (p.stdout + p.stderr)[-20000:]}
    except (OSError, subprocess.TimeoutExpired) as exc:
        return {'exit': -1, 'output': str(exc)}


def classify_pip_check(output, baseline):
    """Split `pip check` output into (allowed, new). `allowed` = lines also present in the
    baseline text; `new` = everything else. No baseline (None) allows nothing."""
    known = {ln.strip() for ln in (baseline or '').splitlines() if ln.strip()}
    lines = [ln.strip() for ln in output.splitlines() if ln.strip()]
    allowed = [ln for ln in lines if ln in known]
    new = [ln for ln in lines if ln not in known]
    # a failing pip check that printed nothing is itself a problem
    return allowed, new or ([] if lines else ['<pip check failed without output>'])


def self_test():
    nccl = 'torch 2.14.0 has requirement nvidia-nccl-cu13==2.29.7, but you have nvidia-nccl-cu13 2.30.7.'
    cusp = 'nvidia-cusparselt-cu13 0.8.1 is not supported on this platform'
    extra = 'foo 1.0 has requirement bar>=2, but you have bar 1.0.'
    base = nccl + '\n' + cusp + '\n'
    # 1) lines inside the baseline are allowed
    assert classify_pip_check(nccl + '\n' + cusp + '\n', base) == ([nccl, cusp], []), 'baseline lines'
    # 2) one line outside the baseline fails, the rest stay allowed
    assert classify_pip_check(nccl + '\n' + extra + '\n', base) == ([nccl], [extra]), 'new line'
    # 3) no baseline: nothing is allowed (original behaviour)
    assert classify_pip_check(nccl + '\n', None) == ([], [nccl]), 'no baseline'
    # 4) empty failing output never passes silently
    assert classify_pip_check('', base)[1], 'empty output'
    print('stack-audit self-test OK')


def sdk_facts(framework):
    patterns = {
        'cuda': ['/usr/local/cuda/version.json', '/usr/local/cuda/version.txt'],
        'cann': ['/usr/local/Ascend/ascend-toolkit/latest/*/ascend_toolkit_install.info',
                 '/usr/local/Ascend/cann-*/ascend_toolkit_install.info'],
        'hggc': ['/usr/local/PPU_SDK/include/hggc_v*/hggc.h',
                 '/usr/local/PPU_SDK/include/hggc_v*/hggc_version.h'],
        'dtk': ['/opt/dtk/VERSION', '/opt/dtk/version', '/opt/dtk/version.txt'],
        'xre': ['/usr/local/xpu/version.txt', '/usr/local/xre/version.txt'],
    }
    result = {}
    for pattern in patterns.get(framework, []):
        for name in glob.glob(pattern):
            if Path(name).is_file():
                result[name] = Path(name).read_text(errors='replace')[:32000]
    return result


def check_versions(installed, engine, expected):
    actual = installed.get(engine)
    if not actual or actual.split('+')[0] != expected:
        return [f'{engine}: expected {expected}, found {actual}']
    return []


def inspect(environ=None):
    env = os.environ if environ is None else environ
    engine = env.get('GPUWORKS_EXPECT_ENGINE', '')
    expected = env.get('GPUWORKS_EXPECT_ENGINE_VERSION', '')
    framework = env.get('GPUWORKS_EXPECT_FRAMEWORK', '')
    runtime = env.get('GPUWORKS_EXPECT_FRAMEWORK_VERSION', '')
    errors = []
    if engine not in ('vllm', 'sglang') or framework not in ('cuda', 'cann', 'dtk', 'hggc', 'xre') or not expected or not runtime:
        errors.append('missing or unsupported expected stack identity')
    installed = {d.metadata['Name'].lower().replace('_', '-'): d.version
                 for d in metadata.distributions() if d.metadata.get('Name')}
    errors.extend(check_versions(installed, engine, expected))
    for package in ('torch', 'transformers'):
        if package not in installed:
            errors.append(f'missing {package}')
    pip_check = command([sys.executable, '-m', 'pip', 'check'])
    pip_check_allowed = []
    if pip_check['exit'] != 0:
        baseline = None
        baseline_path = env.get('GPUWORKS_PIP_CHECK_BASELINE', '')
        if baseline_path:
            try:
                baseline = Path(baseline_path).read_text(errors='replace')
            except OSError:
                baseline = None  # unreadable baseline == no baseline: fail closed
        pip_check_allowed, new_problems = classify_pip_check(pip_check['output'], baseline)
        if new_problems:
            errors.append('pip dependency closure failed')
    facts = sdk_facts(framework)
    runtime_check = {'exit': 0, 'output': ''}
    if framework == 'cuda':
        runtime_check = command([sys.executable, '-c',
            'import torch; print(torch.version.cuda); '
            'assert torch.version.cuda == ' + repr(runtime)])
        if runtime_check['exit'] != 0:
            errors.append('torch CUDA version/import mismatch')
    elif framework == 'cann':
        observed = [v for text in facts.values() for v in re.findall(r'^version=(\S+)', text, re.M)]
        if not any(v == runtime or v.startswith(runtime + '.') for v in observed):
            errors.append('CANN SDK version not verified')
    else:
        values = list(facts.values())
        if framework == 'hggc':
            versions = [int(v) for text in values for v in re.findall(r'^\s*#define\s+HGGC_VERSION\s+([0-9]+)', text, re.M)]
            observed = [f'{v // 1000}.{v % 1000 // 10}.{v % 10}' for v in versions]
            verified = any(v == runtime or v.startswith(runtime + '.') for v in observed)
        else:
            verified = any(re.search(r'(?<![0-9.])' + re.escape(runtime) + r'(?:\.[0-9]+)*(?![0-9.])', text) for text in values)
        if not verified:
            errors.append(f'{framework} SDK version not verified')
    compiler = shutil.which('gcc')
    compiler_check = {'exit': -1, 'output': 'compiler or Python headers missing'}
    if compiler and shutil.which('g++') and Path(sysconfig.get_path('include'), 'Python.h').is_file():
        with tempfile.TemporaryDirectory(prefix='gpuworks-stack-jit-') as work:
            source, binary = Path(work, 'check.c'), Path(work, 'check')
            source.write_text('#include <stdio.h>\nint main(void){puts("gpuworks-jit-ok");return 0;}\n')
            compiler_check = command([compiler, str(source), '-o', str(binary)], 30)
            if compiler_check['exit'] == 0:
                compiler_check = command([str(binary)], 10)
                if compiler_check['output'].strip() != 'gpuworks-jit-ok':
                    compiler_check['exit'] = -1
    if compiler_check['exit'] != 0:
        errors.append('runtime compiler smoke failed')
    return {'status': 'FAIL' if errors else 'PASS', 'errors': errors,
            'expected': {'engine': engine, 'version': expected, 'framework': framework, 'runtime': runtime},
            'architecture': platform.machine(), 'packages': installed, 'sdk': facts,
            'pip_check': pip_check, 'pip_check_allowed': pip_check_allowed,
            'runtime_check': runtime_check, 'compiler_check': compiler_check,
            'gpu_acceptance': 'NOT_RUN'}


if __name__ == '__main__':
    if '--self-test' in sys.argv:
        self_test()
        raise SystemExit(0)
    result = inspect()
    print(json.dumps(result, indent=2, sort_keys=True))
    raise SystemExit(0 if result['status'] == 'PASS' else 1)
