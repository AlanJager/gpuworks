#!/usr/bin/env python3
"""Verify that closing a segment writer keeps the raft loop running."""
import argparse
import os
from pathlib import Path
import re
import shutil

from check_gateway_leader_close import (
    Commands, archive_manifest, now, source_manifest)
from check_shutdown_cancellation import digest, summary


HERE = Path(__file__).resolve().parent
FIX = 'uv-writer-close-off-loop.patch'
FIXTURE = 'uv-writer-close-off-loop-regression.patch'
UNIT_CASE = 'UvWriterClose/slowDestroy'
INSTANCE_CASES = ['append/closeWaitsForSlowWriterRelease',
                  'append/segmentChurnWithSlowWriterRelease']
PROGRAMS = ['raft-uv-unit-test', 'raft-uv-integration-test']
NETWORK_SUITES = {'recv', 'send', 'tcp_connect', 'tcp_listen'}
# io_destroy is reached through the tree's own syscall wrapper, so the slow
# kernel is injected at link time. The upstream test runner trips a
# suggest-attribute warning with this compiler; only that one is demoted.
MAKE_FLAGS = ['CFLAGS=-O2 -g0 -Wno-error=suggest-attribute=noreturn',
              'LDFLAGS=-static -Wl,-z,stack-size=1048576 -Wl,--wrap=io_destroy']


def check(args):
    source = args.source.resolve(strict=True)
    prefix = args.dependency_prefix.resolve(strict=True)
    headers = args.kernel_headers.resolve(strict=True)
    if not all(path.is_dir() for path in (source, prefix, headers)):
        raise ValueError('source, dependency prefix and kernel headers must be directories')
    work_paths = {args.work_dir.resolve(), Path(os.path.abspath(args.work_dir))}
    protected = {source, prefix, headers, Path(os.path.abspath(args.source)),
                 Path(os.path.abspath(args.dependency_prefix)),
                 Path(os.path.abspath(args.kernel_headers))}
    if any(work == path or work.is_relative_to(path) or path.is_relative_to(work)
           for work in work_paths for path in protected):
        raise ValueError('--work-dir must be separate from all protected input trees')
    before_source = source_manifest(source)
    before_archives = archive_manifest(prefix)
    args.work_dir.mkdir(mode=0o700)
    work = args.work_dir.resolve(strict=True)
    env = dict(os.environ)
    for key in ('CFLAGS', 'CPPFLAGS', 'CXXFLAGS', 'LDFLAGS', 'LIBS', 'CONFIG_SITE',
                'REALGCC', 'MAKEFLAGS', 'MFLAGS', 'RAFT_TMP_TMPFS', 'RAFT_TMP_EXT4',
                'RAFT_TMP_BTRFS', 'RAFT_TMP_XFS', 'RAFT_TMP_ZFS'):
        env.pop(key, None)
    env.update(LC_ALL='C', TZ='UTC', PKG_CONFIG_PATH='',
               PKG_CONFIG_LIBDIR=str(prefix / 'lib/pkgconfig'))
    receipt = {'source': str(source), 'dependency_prefix': str(prefix),
               'kernel_headers': str(headers), 'commands': [], 'success': False,
               'started_at': now(), 'source_before': before_source,
               'archives_before': before_archives,
               'production_patch_sha256': digest(HERE / FIX),
               'regression_patch_sha256': digest(HERE / FIXTURE),
               'limits': ['io_destroy is slowed by a link-time wrapper, not by real storage.',
                          'One host and its /tmp file system; no cluster, no leader election.',
                          'Not HA, consensus, durability, inference or SLO acceptance.']}
    runner = Commands(work, receipt, env)
    runner.save()
    try:
        copied, build = work / 'source', work / 'build'
        shutil.copytree(source, copied, symlinks=True)
        build.mkdir(mode=0o700)
        for label, options, asset in (('reverse-fix', ['--reverse'], FIX),
                                      ('test-fixture', ['--forward'], FIXTURE)):
            runner.success(label, ['patch', '--batch', '--fuzz=0', *options,
                '-p1', '-i', str(HERE / asset)], copied)
        configure = [str(copied / 'configure'), '--prefix=' + str(work / 'unpublished'),
            '--disable-shared', '--enable-static', '--with-static-deps',
            '--disable-backtrace', '--without-lz4', 'CC=' + args.cc,
            'CFLAGS=-O2 -g0',
            'LDFLAGS=-static -Wl,-z,stack-size=1048576',
            'CPPFLAGS=-isystem ' + str(headers), 'PKG_CONFIG_PATH=',
            'PKG_CONFIG_LIBDIR=' + str(prefix / 'lib/pkgconfig')]
        runner.success('configure', configure, build, 180)
        make = ['make', '-j' + str(args.jobs), *PROGRAMS, *MAKE_FLAGS]
        unit, instance = (str(build / name) for name in PROGRAMS)
        runner.success('red-build', make, build, 900)
        result, log = runner.run('red', [unit, '--seed', '1', UNIT_CASE], build, 120)
        text = result.stdout.decode(errors='replace')
        gap = re.search(r'assertion failed: heartbeat\.max_gap < 250 \* 1000000ULL '
                        r'\((\d+) < 250000000\)', text)
        if (result.returncode != 1 or not gap or int(gap[1]) < 400 * 1000000 or
                not re.search(r'0 of 1 .*tests successful, 0 .*test skipped', text) or
                not re.search(r'^' + re.escape(UNIT_CASE) + r'\s+\[ ERROR \]', text, re.M)):
            raise RuntimeError('expected stalled-loop RED absent; inspect ' + str(log))
        receipt['red'] = {'case': UNIT_CASE, 'actual_exit': result.returncode,
                          'loop_gap_ns': int(gap[1]),
                          'expected_failure': 'loop thread asleep in io_destroy'}
        runner.success('apply-fix', ['patch', '--batch', '--forward', '--fuzz=0',
            '-p1', '-i', str(HERE / FIX)], copied)
        runner.success('green-build', make, build, 900)
        receipt['green'] = []
        cases = [(unit, UNIT_CASE)] + [(instance, case) for case in INSTANCE_CASES]
        for index, (program, case) in enumerate(cases):
            for attempt in range(args.repeat):
                result = runner.success('green-%d-%d' % (index, attempt),
                    [program, '--seed', str(attempt + 1), case], build, 120)
                actual = summary(result.stdout)
                if actual['passed'] != 1 or actual['skipped'] != 0:
                    raise RuntimeError('GREEN regression must execute exactly once without skip')
            receipt['green'].append({'case': case, 'runs': args.repeat})
        # Every upstream storage case of both programs on this host's file
        # system. The transport suites bind a fixed local port and say nothing
        # about segment writers, so they are left out by name.
        receipt['suites'] = {}
        for program in (unit, instance):
            listed = runner.success('list-' + Path(program).name, [program, '--list'], build)
            names = [line for line in listed.stdout.decode().splitlines()
                     if '/' in line and line.split('/')[0] not in NETWORK_SUITES]
            if len(names) < 20 or len(set(names)) != len(names):
                raise RuntimeError('unexpected test inventory for ' + program)
            result = runner.success('suite-' + Path(program).name,
                [program, '--seed', '1', *names], build, 900)
            actual = summary(result.stdout)
            actual['cases'] = len(names)
            receipt['suites'][Path(program).name] = actual
        receipt['success'] = True
    except BaseException as error:
        receipt['error'] = str(error) or type(error).__name__
        raise
    finally:
        try:
            receipt['source_unchanged'] = source_manifest(source) == before_source
            receipt['archives_unchanged'] = archive_manifest(prefix) == before_archives
            if not receipt['source_unchanged'] or not receipt['archives_unchanged']:
                raise RuntimeError('protected source or dependency archives changed')
        except BaseException as error:
            receipt['success'] = False
            receipt['input_verification_error'] = str(error)
            raise
        finally:
            receipt['finished_at'] = now()
            runner.save()
    print('writer close RED/GREEN verified; receipt: ' + str(work / 'receipt.json'))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source', type=Path, required=True,
                        help='C source after all normal production patches, without test patches')
    parser.add_argument('--dependency-prefix', type=Path, required=True)
    parser.add_argument('--kernel-headers', type=Path, required=True)
    parser.add_argument('--work-dir', type=Path, required=True,
                        help='new absent directory outside all protected input trees')
    parser.add_argument('--cc', default='/usr/bin/musl-gcc')
    parser.add_argument('--jobs', type=int, default=2)
    parser.add_argument('--repeat', type=int, default=5,
                        help='GREEN executions of each regression case')
    args = parser.parse_args()
    if args.jobs < 1 or args.repeat < 1:
        parser.error('--jobs and --repeat must be positive')
    check(args)


if __name__ == '__main__':
    main()
