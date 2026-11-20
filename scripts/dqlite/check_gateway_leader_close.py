#!/usr/bin/env python3
"""Verify gateway allocation-failure cleanup in a disposable native C build."""
import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import time

from check_shutdown_cancellation import digest, no_core, require_success, summary


HERE = Path(__file__).resolve().parent
CASES = ['gateway/' + suite + '/exec_allocation_failure_closes_leader'
         for suite in ('query', 'query_sql', 'exec_sql')]
EXPECTED_SKIP = 'gateway/exec/restore'


def now():
    return datetime.now(timezone.utc).isoformat()


def source_manifest(root):
    result = {}
    for path in sorted(root.rglob('*')):
        name = str(path.relative_to(root))
        if path.is_symlink():
            target = os.readlink(path)
            # Relative links within this tree remain within its disposable copy.
            if Path(target).is_absolute() or not path.resolve().is_relative_to(root):
                raise ValueError('source symlink escapes disposable copy: ' + name)
            result[name] = {'symlink': target}
        elif path.is_file():
            result[name] = {'sha256': digest(path)}
        elif path.is_dir():
            result[name] = {'directory': True}
        else:
            raise ValueError('unsupported source entry: ' + name)
    return result


def archive_manifest(prefix):
    archives = sorted((prefix / 'lib').rglob('*.a'))
    if not archives or not (prefix / 'lib/libdqlite.a').is_file():
        raise ValueError('dependency prefix must contain its qualified libdqlite.a')
    return {str(path.relative_to(prefix)): digest(path) for path in archives}


class Commands:
    """Record every actual wait; terminate and join a whole build group on timeout."""
    def __init__(self, work, receipt, env):
        self.work, self.receipt, self.env = work, receipt, env

    def run(self, label, argv, cwd, timeout=60):
        record = {'label': label, 'argv': argv, 'cwd': str(cwd),
                  'started_at': now(), 'actual_wait': False, 'timeout_s': timeout}
        self.receipt['commands'].append(record)
        log = self.work / (label + '.log')
        record['log'] = str(log)
        started = time.monotonic()
        process = None
        output = b''
        try:
            process = subprocess.Popen(argv, cwd=cwd, env=self.env,
                stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                start_new_session=True, preexec_fn=no_core)
            record['pid'] = process.pid
            try:
                output, _ = process.communicate(timeout=timeout)
            except subprocess.TimeoutExpired:
                record['timed_out'] = True
                self.kill_group(process, signal.SIGTERM, record)
                try:
                    output, _ = process.communicate(timeout=5)
                except subprocess.TimeoutExpired:
                    self.kill_group(process, signal.SIGKILL, record)
                    output, _ = process.communicate()
                # A make parent may exit before its children; close that group.
                self.kill_group(process, signal.SIGKILL, record)
                raise RuntimeError('command timed out; inspect ' + str(log))
            return subprocess.CompletedProcess(argv, process.returncode, output, b''), log
        finally:
            if process is not None:
                if process.poll() is None:
                    self.kill_group(process, signal.SIGKILL, record)
                    output, _ = process.communicate()
                record.update(actual_wait=True, actual_exit=process.wait())
            record.update(finished_at=now(), elapsed_s=time.monotonic() - started)
            log.write_bytes(output)
            log.chmod(0o600)
            self.save()

    @staticmethod
    def kill_group(process, sig, record):
        try:
            os.killpg(process.pid, sig)
            record.setdefault('cleanup_signals', []).append(sig.name)
        except ProcessLookupError:
            pass

    def save(self):
        path = self.work / 'receipt.json'
        path.write_text(json.dumps(self.receipt, indent=2) + '\n')
        path.chmod(0o600)

    def success(self, label, argv, cwd, timeout=60):
        result, log = self.run(label, argv, cwd, timeout)
        require_success(result, log)
        return result


def gateway_names(output):
    names = [line for line in output.decode().splitlines()
             if line.startswith('gateway/')]
    if len(names) != 95 or len(set(names)) != len(names):
        raise RuntimeError('expected 95 unique gateway inventory names')
    if any(case not in names for case in CASES):
        raise RuntimeError('allocation regression cases missing from inventory')
    # Munit receives complete listed names. It matches exact names or the name
    # followed by a parameter separator; "gateway/" alone executes zero tests.
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
               'production_patch_sha256': digest(HERE / 'gateway-leader-close.patch'),
               'regression_patch_sha256': digest(HERE / 'gateway-leader-close-regression.patch'),
               'limits': ['Synthetic exec allocation failure; no remote operations.',
                          'Not proof of the observed native stop assertion root cause.',
                          'Not HA, consensus, durability, inference or SLO acceptance.']}
    runner = Commands(work, receipt, env)
    runner.save()
    try:
        copied, build = work / 'source', work / 'build'
        shutil.copytree(source, copied, symlinks=True)
        build.mkdir(mode=0o700)
        for label, options, asset in (
            ('reverse-fix', ['--reverse'], 'gateway-leader-close.patch'),
            ('test-fixture', ['--forward'], 'gateway-leader-close-regression.patch')):
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
        names = gateway_names(inventory.stdout)
        receipt['gateway_inventory'] = names
        receipt['red'] = []
        for index, case in enumerate(CASES):
            result, log = runner.run('red-' + str(index),
                [executable, '--seed', '1', case], build, 30)
            text = result.stdout.decode(errors='replace')
            if (result.returncode != 1 or
                not re.search(r'0 of 1 .*tests successful, 0 .*test skipped', text) or
                text.count('assertion failed: f->context->invoked is not true') != 1 or
                'child killed by signal 6' not in text or
                not re.search(r'^' + re.escape(case) + r'\s+\[ ERROR \]', text, re.M)):
                raise RuntimeError('expected close-callback RED absent; inspect ' + str(log))
            receipt['red'].append({'case': case, 'actual_exit': result.returncode,
                                   'expected_failure': 'close callback missing'})
        runner.success('apply-fix', ['patch', '--batch', '--forward', '--fuzz=0',
            '-p1', '-i', str(HERE / 'gateway-leader-close.patch')], copied)
        runner.success('green-build', make, build, 600)
        inventory = runner.success('green-inventory', [executable, '--list'], build)
        if gateway_names(inventory.stdout) != names:
            raise RuntimeError('gateway inventory changed between RED and GREEN')
        receipt['green'] = []
        for index, case in enumerate(CASES):
            result = runner.success('green-' + str(index),
                [executable, '--seed', '1', case], build, 30)
            actual = summary(result.stdout)
            if actual['passed'] != 1 or actual['skipped'] != 0:
                raise RuntimeError('GREEN regression must execute exactly once without skip')
            receipt['green'].append({'case': case, **actual})
        result = runner.success('gateway-all', [executable, '--seed', '1', *names], build, 180)
        actual = summary(result.stdout)
        skipped = re.findall(r'^(gateway/\S+)\s+\[ SKIP\s*\]',
                             result.stdout.decode(errors='replace'), re.M)
        if actual['passed'] != 111 or actual['skipped'] != 1 or skipped != [EXPECTED_SKIP]:
            raise RuntimeError('expected 111 PASS and only upstream gateway/exec/restore SKIP')
        actual['skipped_test_names'] = skipped
        receipt['gateway_all'] = actual
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
    print('gateway cleanup RED/GREEN verified; receipt: ' + str(work / 'receipt.json'))


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
