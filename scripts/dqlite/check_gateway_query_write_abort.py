#!/usr/bin/env python3
"""Verify mid-ROWS client drop cleanup in a disposable native C build."""
import argparse
import os
from pathlib import Path
import re
import shutil

from check_gateway_leader_close import (Commands, archive_manifest, now,
                                        source_manifest)
from check_shutdown_cancellation import digest, summary


HERE = Path(__file__).resolve().parent
CASE = 'conn/query/client_drop_mid_rows_closes_leader'


def conn_names(output):
    names = [line for line in output.decode().splitlines()
             if line.startswith('conn/')]
    if not names or len(set(names)) != len(names) or CASE not in names:
        raise RuntimeError('conn inventory is empty, duplicated or lacks ' + CASE)
    return names


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
    for key in ('CFLAGS', 'CPPFLAGS', 'CXXFLAGS', 'LDFLAGS', 'LIBS',
                'CONFIG_SITE', 'REALGCC', 'MAKEFLAGS', 'MFLAGS'):
        env.pop(key, None)
    env.update(LC_ALL='C', TZ='UTC', PKG_CONFIG_PATH='',
               PKG_CONFIG_LIBDIR=str(prefix / 'lib/pkgconfig'))
    receipt = {'source': str(source), 'dependency_prefix': str(prefix),
               'kernel_headers': str(headers), 'commands': [], 'success': False,
               'started_at': now(), 'source_before': before_source,
               'archives_before': before_archives,
               'production_patch_sha256': digest(HERE / 'gateway-query-write-abort.patch'),
               'regression_patch_sha256': digest(HERE / 'gateway-query-write-abort-regression.patch'),
               'limits': ['Single in-process conn over a Unix socket; no remote operations.',
                          'Not HA, consensus, durability, inference or SLO acceptance.']}
    runner = Commands(work, receipt, env)
    runner.save()
    try:
        copied, build = work / 'source', work / 'build'
        shutil.copytree(source, copied, symlinks=True)
        build.mkdir(mode=0o700)
        for label, options, asset in (
            ('reverse-fix', ['--reverse'], 'gateway-query-write-abort.patch'),
            ('test-fixture', ['--forward'], 'gateway-query-write-abort-regression.patch')):
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
        make = ['make', '-j' + str(args.jobs), 'unit-test']
        runner.success('red-build', make, build, 600)
        executable = str(build / 'unit-test')
        inventory = runner.success('red-inventory', [executable, '--list'], build)
        names = conn_names(inventory.stdout)
        receipt['conn_inventory'] = names
        result, log = runner.run('red', [executable, '--seed', '1', CASE], build, 30)
        text = result.stdout.decode(errors='replace')
        if (result.returncode != 1 or
            not re.search(r'0 of 1 .*tests successful, 0 .*test skipped', text) or
            text.count('assertion failed: f->conn_test.closed is not true') != 1 or
            'child killed by signal 6' not in text or
            not re.search(r'^' + re.escape(CASE) + r'\s+\[ ERROR \]', text, re.M)):
            raise RuntimeError('expected unclosed-gateway RED absent; inspect ' + str(log))
        receipt['red'] = {'case': CASE, 'actual_exit': result.returncode,
                          'expected_failure': 'gateway close never completed'}
        runner.success('apply-fix', ['patch', '--batch', '--forward', '--fuzz=0',
            '-p1', '-i', str(HERE / 'gateway-query-write-abort.patch')], copied)
        runner.success('green-build', make, build, 600)
        inventory = runner.success('green-inventory', [executable, '--list'], build)
        if conn_names(inventory.stdout) != names:
            raise RuntimeError('conn inventory changed between RED and GREEN')
        result = runner.success('green', [executable, '--seed', '1', CASE], build, 30)
        actual = summary(result.stdout)
        if actual['passed'] != 1 or actual['skipped'] != 0:
            raise RuntimeError('GREEN regression must execute exactly once without skip')
        receipt['green'] = {'case': CASE, **actual}
        result = runner.success('conn-all', [executable, '--seed', '1', *names], build, 180)
        actual = summary(result.stdout)
        if actual['skipped'] != 0 or actual['passed'] < len(names):
            raise RuntimeError('every conn case must pass without skip')
        receipt['conn_all'] = actual
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
    print('query write-abort RED/GREEN verified; receipt: ' + str(work / 'receipt.json'))


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
    args = parser.parse_args()
    if args.jobs < 1:
        parser.error('--jobs must be positive')
    check(args)


if __name__ == '__main__':
    main()
