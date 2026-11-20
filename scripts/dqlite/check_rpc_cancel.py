"""Offline pinned SDK wire cancellation and replay-safety regression (no HA claim)."""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess

import prepare_go_sdk as sdk

SOURCE_SHA256 = 'db3a7e6508e7ad099f746d83e54686771a63a29f04e1f4a69c1a80f21991c3cf'
REGRESSION_SHA256 = '04b700815fc2589b48309396974129da3ebdc0acf80739a75f92959aeb1469c5'
TEST_FILES = {'internal/protocol/rpc_cancel_test.go', 'driver/rpc_cancel_test.go'}


def run(command, cwd, env, log, expected=0):
    result = subprocess.run(command, cwd=cwd, env=env, capture_output=True, text=True, timeout=180)
    log.write_text(result.stdout + result.stderr)
    if result.returncode != expected:
        raise ValueError('unexpected exit ' + str(result.returncode) + '; see ' + str(log))
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source', type=Path, required=True, help='pinned Unix plus node-store SDK module')
    parser.add_argument('--work-dir', type=Path, required=True, help='new private output directory')
    parser.add_argument('--go', default=os.environ.get('GO', 'go'))
    args = parser.parse_args()
    source, requested = args.source.resolve(), args.work_dir.absolute()
    if any(path.is_symlink() for path in (requested, *requested.parents)):
        raise ValueError('work directory must not traverse a symlink')
    work = requested.resolve()
    if work.exists() or work == source or source in work.parents:
        raise ValueError('work directory must be new and outside source')
    original = sdk.file_hashes(source)
    if sdk.tree_digest(original) != SOURCE_SHA256:
        raise ValueError('source must be the complete pinned Unix plus node-store module')
    here = Path(__file__).resolve().parent
    patch = here / 'go-dqlite-rpc-cancel.patch'
    regression = here / 'go-dqlite-rpc-cancel-regression.patch'
    if sdk.digest(patch) != sdk.RPC_CANCEL_PATCH_SHA256 or sdk.digest(regression) != REGRESSION_SHA256:
        raise ValueError('RPC cancellation patch identity differs')
    work.mkdir(mode=0o700, parents=True)
    module = work / 'module'
    shutil.copytree(source, module)
    for path in module.rglob('*'):
        path.chmod(0o700 if path.is_dir() else 0o600)
    # The native caller supplies CC/C flags and libraries. nosqlite3 matches the
    # native SDK build and avoids compiling an unrelated bundled SQLite engine.
    env = dict(os.environ, GOENV='off', GOPROXY='off', GOSUMDB='off', GOTOOLCHAIN='local',
               GOFLAGS='', GONOPROXY='none', GOVCS='*:off')
    receipt = {'passed': False, 'scope': 'SDK wire cancellation only; no native SQL/HA/SLO claim',
               'source_sha256': SOURCE_SHA256, 'production_patch_sha256': sdk.digest(patch),
               'regression_patch_sha256': sdk.digest(regression), 'exits': {}}
    try:
        apply = ['patch', '--batch', '--forward', '--fuzz=0', '-p1', '-d', str(module), '-i']
        run(apply + [str(regression)], module, env, work / 'apply-regression.log')
        tests = ['-tags=nosqlite3', '-p=1', '-ldflags=-linkmode external',
                 './internal/protocol', './driver', '-run=^TestRPCCancel', '-count=1', '-v', '-timeout=45s']
        baseline = run([args.go, 'test', *tests], module, env, work / 'baseline.log', expected=1)
        if ('panic:' in baseline.stdout or '[build failed]' in baseline.stdout + baseline.stderr
                or 'actual sentRPC ignored cancel' not in baseline.stdout
                or 'sentRPC replay signal/lostcloseerror' not in baseline.stdout
                or 'FAIL: TestRPCCancelActiveWire' not in baseline.stdout):
            raise ValueError('baseline did not expose the expected cancellation/replay regressions')
        receipt['exits']['baseline'] = 1
        run(apply + [str(patch)], module, env, work / 'apply-production.log')
        production_files = {name: value for name, value in sdk.file_hashes(module).items() if name not in TEST_FILES}
        if sdk.tree_digest(production_files) != sdk.RPC_CANCEL_MODULE_SHA256:
            raise ValueError('patched complete production module differs')
        for name, expected in sdk.RPC_CANCEL_PATCHED_FILES.items():
            if sdk.digest(module / name) != expected:
                raise ValueError('patched file differs: ' + name)
        run([args.go, 'test', *tests], module, env, work / 'green.log')
        receipt['exits']['green'] = 0
        run([args.go, 'test', '-tags=nosqlite3', '-p=1', '-ldflags=-linkmode external',
             './internal/protocol', './driver', '-run=^(TestProtocol_|TestDriver_|TestConn_|TestStmt_)',
             '-count=1', '-v', '-timeout=90s'], module, env, work / 'compatibility.log')
        receipt['exits']['compatibility'] = 0
        run([args.go, 'test', '-race', *tests], module, env, work / 'race.log')
        receipt['exits']['race'] = 0
        run([args.go, 'vet', '-tags=nosqlite3', './internal/protocol', './driver'], module, env, work / 'vet.log')
        receipt['exits']['vet'] = 0
        receipt.update(passed=True, result_module_sha256=sdk.tree_digest(production_files))
    finally:
        receipt['source_unchanged'] = sdk.file_hashes(source) == original
        if not receipt['source_unchanged']:
            receipt['passed'] = False
        (work / 'receipt.json').write_text(json.dumps(receipt, indent=2) + '\n')
    if not receipt['passed']:
        raise ValueError('RPC cancellation regression incomplete')
    print(json.dumps(receipt))


if __name__ == '__main__':
    main()
