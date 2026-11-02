"""Explicit image integration: exact original source, RO assets, idempotent install.

Run in the pinned base image with the generated asset directory mounted read-only.
This test intentionally fails if the external image/source/mount is missing.
"""
import errno
import hashlib
import importlib.util
import os
from pathlib import Path
import sys
import unittest
from unittest import mock

ASSETS = Path(os.environ.get('STRATA_QUEUE_ASSET_DIR', '/opt/gpuworks/strata-queue'))


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


class SourcePatch(unittest.TestCase):
    def test_actual_image_source_and_readonly_assets(self):
        spec = importlib.util.spec_from_file_location('strata_queue', ASSETS / 'strata_queue.py')
        runtime = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(runtime)
        before = {p.name: digest(p) for p in ASSETS.iterdir() if p.is_file()}
        probe = ASSETS / '.readonly-probe'
        created = False
        try:
            with self.assertRaises(OSError) as failure:
                with probe.open('xb') as output:
                    created = True
                    output.write(b'must not be writable')
        finally:
            if created:
                probe.unlink()
        self.assertEqual(failure.exception.errno, errno.EROFS, 'use an actual read-only asset mount')
        self.assertEqual(digest(runtime.SERVER), runtime.SOURCE_SHA, 'start from a fresh pinned base container')
        runtime.install()
        self.assertEqual(digest(runtime.SERVER), runtime.PATCHED_SHA)
        runtime.install()  # Restart is idempotent, with the same read-only asset.
        arguments = ['--model', '/model/path with spaces', '--port', '8080', '--context', '4096']
        with mock.patch.object(sys, 'argv', ['strata_queue.py', *arguments]), mock.patch.object(runtime.os, 'execv') as execute:
            self.assertEqual(runtime.main(), 0)
            execute.assert_called_once_with(runtime.ENTRYPOINT, [runtime.ENTRYPOINT, *arguments])
        self.assertEqual(before, {p.name: digest(p) for p in ASSETS.iterdir() if p.is_file()})
        print('ACTUAL_BASE_SOURCE_SHA256', runtime.SOURCE_SHA)
        print('ASSET_MOUNT_READONLY_AND_UNCHANGED')
        print('ACTUAL_PATCHED_SOURCE_SHA256', digest(runtime.SERVER))
        print('ORIGINAL_ENTRYPOINT_ARGV_CHECKED_WITH_EXEC_MOCK_ONLY')


if __name__ == '__main__':
    unittest.main(verbosity=2)
