"""Offline node-store behavior/race and actual file-persistence syscall regression."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys

import prepare_go_sdk as sdk

REGRESSION_SHA256 = 'dfe446cea65e196d3f79124348949df2509a6da3d085e8aad5ece738c98a162c'


def run(args, cwd, env, log, expected=0):
    result = subprocess.run(args, cwd=cwd, env=env, capture_output=True, text=True, timeout=180)
    log.write_text(result.stdout + result.stderr)
    if result.returncode != expected:
        raise ValueError('unexpected command exit: ' + str(args) + '; see ' + str(log))
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source', type=Path, required=True, help='unmodified cached Go SDK module')
    parser.add_argument('--work-dir', type=Path, required=True, help='new private output directory')
    parser.add_argument('--go', default=os.environ.get('GO', 'go'))
    args = parser.parse_args()
    source, requested = args.source.resolve(), args.work_dir.absolute()
    if any(path.is_symlink() for path in (requested, *requested.parents)):
        raise ValueError('work directory must not traverse a symlink')
    work = requested.resolve()
    if work.exists() or work.is_symlink() or work == source or source in work.parents:
        raise ValueError('work directory must be new and outside the source')
    original = sdk.file_hashes(source)
    if sdk.tree_digest(original) != sdk.SOURCE_TREE_SHA256:
        raise ValueError('regression requires the complete pinned SDK source')
    here = Path(__file__).resolve().parent
    patch, regression = here / 'go-dqlite-node-store.patch', here / 'go-dqlite-node-store-regression.patch'
    if sdk.digest(patch) != sdk.NODE_STORE_PATCH_SHA256 or sdk.digest(regression) != REGRESSION_SHA256:
        raise ValueError('node-store patch identity differs')
    if not shutil.which('strace') or not shutil.which('gcc'):
        raise ValueError('real syscall/race regression requires local strace and gcc')
    work.mkdir(mode=0o700, parents=True)
    module = work / 'module'
    shutil.copytree(source, module)
    for path in module.rglob('*'):
        path.chmod(0o700 if path.is_dir() else 0o600)
    env = dict(os.environ, GOPROXY='off', GOSUMDB='off', GOTOOLCHAIN='local', GOFLAGS='',
               GONOPROXY='none', GOVCS='*:off', CGO_ENABLED='1', CC='gcc')
    receipt = {'passed': False, 'scope': 'local file persistence only; no native/GPU/HA/SLO claim',
               'source_tree_sha256': sdk.tree_digest(original), 'patch_sha256': sdk.digest(patch),
               'regression_sha256': sdk.digest(regression)}
    try:
        apply = ['patch', '--batch', '--forward', '--fuzz=0', '-p1', '-d', str(module), '-i']
        run(apply + [str(regression)], module, env, work / 'apply-regression.log')
        files = ['store.go', 'store_dedup_test.go']
        baseline = run([args.go, 'test', '-count=1', *files], module / 'client', env,
                       work / 'baseline.log', expected=1)
        if ('unchanged Set replaced or rewrote file' not in baseline.stdout
                or 'TestYamlNodeStoreSnapshotOwnershipAndConcurrentAccess' not in baseline.stdout):
            raise ValueError('baseline failed without the expected behavior regressions')
        run(apply + [str(patch)], module, env, work / 'apply-production.log')
        if sdk.digest(module / 'client/store.go') != sdk.NODE_STORE_PATCHED_SHA256:
            raise ValueError('patched store source differs')
        run([args.go, 'test', '-race', '-count=1', '-v', *files], module / 'client', env, work / 'race.log')
        run([args.go, 'vet', *files], module / 'client', env, work / 'vet.log')
        binary = work / 'node-store.test'
        run([args.go, 'test', '-c', '-o', str(binary), *files], module / 'client', env, work / 'compile-probe.log')
        results = {}
        for mode, expected_syncs in [('stable', 1), ('startup', 1), ('changed', 2), ('repair', 2), ('sync-error', 2)]:
            trace = work / (mode + '.trace')
            command = ['strace', '-f', '-qq', '-e', 'trace=fsync,rename,renameat,renameat2', '-o', str(trace)]
            if mode == 'sync-error':
                command += ['-e', 'inject=fsync:error=EIO:when=1']
            command += [str(binary), '-test.run=^TestYamlNodeStorePersistenceSyscallProbe$', '-test.v']
            run(command, module / 'client', dict(env, GPUWORKS_NODESTORE_PROBE=mode), work / (mode + '.log'))
            raw = trace.read_text()
            count = len(re.findall(r'\bfsync\(', raw))
            injected = raw.count('(INJECTED)')
            if count != expected_syncs or (mode == 'sync-error' and injected != 1):
                raise ValueError('unexpected real fsync count/injection for ' + mode + ': ' + str(count))
            results[mode] = {'fsync_calls': count, 'expected': expected_syncs, 'injected_errors': injected}
        receipt.update(passed=True, actual_syscalls=results)
    finally:
        receipt['source_unchanged'] = sdk.file_hashes(source) == original
        if not receipt['source_unchanged']:
            receipt['passed'] = False
        (work / 'receipt.json').write_text(json.dumps(receipt, indent=2) + '\n')
    if not receipt['passed']:
        raise ValueError('node-store regression incomplete')
    print(json.dumps(receipt))


if __name__ == '__main__':
    try:
        main()
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
