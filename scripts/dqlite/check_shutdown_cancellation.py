#!/usr/bin/env python3
"""Reproduce and verify shutdown cancellation against an isolated SDK source copy."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import resource
import shutil
import subprocess
import time


HERE = Path(__file__).resolve().parent
CASES = [
    'replication/closePendingQuorumAppliedAppend',
    'replication/closeAppliedPrefixUncommittedSuffix',
]


def digest(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def no_core():
    resource.setrlimit(resource.RLIMIT_CORE, (0, 0))


def run(work, label, command, cwd, env, timeout=60):
    started = time.monotonic()
    result = subprocess.run(command, cwd=cwd, env=env, capture_output=True,
                            timeout=timeout, preexec_fn=no_core)
    log = work / (label + '.log')
    log.write_bytes(result.stdout + result.stderr)
    log.chmod(0o600)
    return result, time.monotonic() - started, log


def require_success(result, log):
    if result.returncode:
        raise RuntimeError('command failed; inspect ' + str(log))


def summary(output):
    text = output.decode('utf-8', errors='replace')
    match = re.search(r'(\d+) of (\d+) .*?tests successful, (\d+) .*?skipped', text)
    if not match or int(match[1]) != int(match[2]) or int(match[2]) == 0:
        raise RuntimeError('missing successful executed-test summary')
    last_test = ''
    skipped = []
    for line in text.splitlines():
        test = re.match(r'^([a-zA-Z0-9_]+/[a-zA-Z0-9_]+)', line)
        if test:
            last_test = test[1]
        if '[ SKIP' in line:
            skipped.append(last_test + ': ' + line.strip())
    return {'passed': int(match[1]), 'skipped': int(match[3]),
            'skip_records': skipped}


def check(args):
    source = args.source.resolve(strict=True)
    prefix = args.dependency_prefix.resolve(strict=True)
    headers = args.kernel_headers.resolve(strict=True)
    # Check lexical and resolved paths: supplied inputs may contain symlinks.
    work_paths = {args.work_dir.resolve(), Path(os.path.abspath(args.work_dir))}
    protected = {source, prefix, headers,
                 Path(os.path.abspath(args.source)),
                 Path(os.path.abspath(args.dependency_prefix)),
                 Path(os.path.abspath(args.kernel_headers))}
    if any(work == path or work.is_relative_to(path)
           for work in work_paths for path in protected):
        raise ValueError('--work-dir must be outside source, dependency prefix and headers')
    archive = prefix / 'lib/libdqlite.a'
    original_archive_digest = digest(archive)
    # Exclusive creation prevents replay or modifying an existing diagnostic run.
    args.work_dir.mkdir(mode=0o700)
    work = args.work_dir.resolve(strict=True)
    copied = work / 'source'
    shutil.copytree(source, copied, symlinks=True)
    build = work / 'build'
    build.mkdir(mode=0o700)
    env = dict(os.environ)
    for key in ['CFLAGS', 'CPPFLAGS', 'CXXFLAGS', 'LDFLAGS', 'LIBS',
                'CONFIG_SITE', 'REALGCC']:
        env.pop(key, None)
    env.update(LC_ALL='C', TZ='UTC', PKG_CONFIG_PATH='',
               PKG_CONFIG_LIBDIR=str(prefix / 'lib/pkgconfig'))
    receipt = {
        'source': str(source), 'dependency_prefix': str(prefix),
        'archive_sha256_before': original_archive_digest,
        'production_patch_sha256': digest(HERE / 'shutdown-cancellation.patch'),
        'regression_patch_sha256': digest(HERE / 'shutdown-cancellation-regression.patch'),
        'limits': ['Synthetic queued cancellation callback, not real uvAppendClose.',
                   'No physical I/O, restart durability or remote recovery proof.',
                   'Live IOERR, follower cancellation and close-time IOERR unresolved.'],
    }
    try:
        # The input is the normal recipe's patched source. Reverse only this fix
        # in the private copy to establish the exact original RED baseline.
        for label, options, asset in [
            ('reverse-fix', ['--reverse'], 'shutdown-cancellation.patch'),
            ('test-fixture', [], 'shutdown-cancellation-regression.patch'),
        ]:
            result, _, log = run(work, label,
                ['patch', '--batch', '--fuzz=0', *options, '-p1', '-i', str(HERE / asset)],
                copied, env)
            require_success(result, log)
        configure = [str(copied / 'configure'), '--prefix=' + str(work / 'unpublished'),
            '--disable-shared', '--enable-static', '--with-static-deps',
            '--disable-backtrace', '--without-lz4', 'CC=' + args.cc,
            'CFLAGS=-O2 -g0 -Wno-error=suggest-attribute=noreturn',
            'LDFLAGS=-static -Wl,-z,stack-size=1048576',
            'CPPFLAGS=-isystem ' + str(headers), 'PKG_CONFIG_PATH=',
            'PKG_CONFIG_LIBDIR=' + str(prefix / 'lib/pkgconfig')]
        result, _, log = run(work, 'configure', configure, build, env)
        require_success(result, log)
        make = ['make', '-j' + str(args.jobs), 'raft-core-integration-test']
        result, _, log = run(work, 'red-build', make, build, env, 180)
        require_success(result, log)
        executable = str(build / 'raft-core-integration-test')
        result, _, log = run(work, 'inventory', [executable, '--list'], build, env)
        require_success(result, log)
        inventory = result.stdout.decode().splitlines()
        if any(case not in inventory for case in CASES):
            raise RuntimeError('regression cases absent from actual inventory')
        original_cases = [case for case in inventory if case not in CASES]
        # munit positional filters are prefixes, and overlapping filters execute
        # matching cases again. Keep one covering prefix for each existing case.
        original_filters = [case for case in original_cases
                            if not any(case != other and case.startswith(other)
                                       for other in original_cases)]
        if any(case.startswith(test_filter)
               for case in CASES for test_filter in original_filters):
            raise RuntimeError('existing-case filter overlaps a RED regression case')
        receipt['original_test_names'] = len(original_cases)
        receipt['original_nonoverlapping_filters'] = len(original_filters)
        result, elapsed, log = run(work, 'original-existing',
            [executable, '--seed', '1', *original_filters], build, env)
        require_success(result, log)
        receipt['original_existing'] = summary(result.stdout + result.stderr)
        receipt['original_existing']['elapsed_s'] = elapsed
        receipt['red'] = []
        for number, case in enumerate(CASES):
            result, elapsed, log = run(work, 'red-' + str(number),
                [executable, '--seed', '1', case], build, env, 30)
            output = result.stdout + result.stderr
            if (result.returncode == 0 or b'0 of 1' not in output or
                b'sm_move: 83' not in output or b'.allowed &' not in output or
                b'child killed by signal 6' not in output):
                raise RuntimeError('expected transition assertion not reproduced; inspect ' + str(log))
            receipt['red'].append({'case': case, 'exit': result.returncode, 'elapsed_s': elapsed})
        result, _, log = run(work, 'apply-fix',
            ['patch', '--batch', '--forward', '--fuzz=0', '-p1', '-i',
             str(HERE / 'shutdown-cancellation.patch')], copied, env)
        require_success(result, log)
        result, _, log = run(work, 'green-build', make, build, env, 180)
        require_success(result, log)
        receipt['green'] = []
        for number, case in enumerate(CASES):
            result, elapsed, log = run(work, 'green-' + str(number),
                [executable, '--seed', '1', case], build, env, 30)
            require_success(result, log)
            actual = summary(result.stdout + result.stderr)
            if actual['passed'] != 1 or actual['skipped'] != 0:
                raise RuntimeError('regression did not execute exactly once')
            receipt['green'].append({'case': case, **actual, 'elapsed_s': elapsed})
        result, elapsed, log = run(work, 'candidate-all',
            [executable, '--seed', '1'], build, env)
        require_success(result, log)
        receipt['candidate_all'] = summary(result.stdout + result.stderr)
        receipt['candidate_all']['elapsed_s'] = elapsed
        old, new = receipt['original_existing'], receipt['candidate_all']
        if (new['passed'] != old['passed'] + len(CASES) or
            new['skip_records'] != old['skip_records'] or new['skipped'] != old['skipped']):
            raise RuntimeError('baseline/candidate case or skip counts differ unexpectedly')
        if digest(archive) != original_archive_digest:
            raise RuntimeError('input SDK archive changed during isolated regression')
        receipt['archive_unchanged'] = True
        receipt['success'] = True
    except Exception as error:
        receipt['success'] = False
        receipt['error'] = str(error)
        raise
    finally:
        path = work / 'receipt.json'
        path.write_text(json.dumps(receipt, indent=2) + '\n')
        path.chmod(0o600)
    print('shutdown cancellation RED/GREEN verified; receipt: ' + str(work / 'receipt.json'))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source', type=Path, required=True,
                        help='normal SDK cache source after all tracked patches')
    parser.add_argument('--dependency-prefix', type=Path, required=True)
    parser.add_argument('--kernel-headers', type=Path, required=True)
    parser.add_argument('--work-dir', type=Path, required=True,
                        help='new absent directory for isolated source/build/evidence')
    parser.add_argument('--cc', default='/usr/bin/musl-gcc')
    parser.add_argument('--jobs', type=int, default=2)
    args = parser.parse_args()
    if args.jobs < 1:
        parser.error('--jobs must be positive')
    check(args)


if __name__ == '__main__':
    main()
