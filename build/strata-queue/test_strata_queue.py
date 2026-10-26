import contextlib
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest import mock

HERE = Path(__file__).resolve().parent


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


runtime = load('strata_queue', HERE / 'strata_queue.py')
build = load('strata_queue_build', HERE / 'build.py')
SOURCE = b'one = 1\ntwo = 2\n'
UPDATED = b'one = 1\ntwo = 3\n'
PATCH = b'--- a/serve/server.py\n+++ b/serve/server.py\n@@ -1,2 +1,2 @@\n one = 1\n-two = 2\n+two = 3\n'


def sha(data):
    return hashlib.sha256(data).hexdigest()


class Installer(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.target, self.patch = self.root / 'server.py', self.root / 'initial-queue.patch'
        self.target.write_bytes(SOURCE)
        self.target.chmod(0o640)
        self.patch.write_bytes(PATCH)
        constants = mock.patch.multiple(runtime, SOURCE_SHA=sha(SOURCE), PATCHED_SHA=sha(UPDATED), PATCH_SHA=sha(PATCH))
        constants.start()
        self.addCleanup(constants.stop)

    def install(self):
        runtime.install(self.target, self.patch)

    def test_exact_patch_idempotence_modes_and_shared_hardlink(self):
        shared = self.root / 'shared-image-file.py'
        os.link(self.target, shared)
        self.install()
        self.assertEqual(self.target.read_bytes(), UPDATED)
        self.assertEqual(shared.read_bytes(), SOURCE)
        self.assertNotEqual(self.target.stat().st_ino, shared.stat().st_ino)
        self.assertEqual(self.target.stat().st_mode & 0o777, 0o640)
        with mock.patch.object(runtime.os, 'replace', side_effect=AssertionError('idempotent install wrote again')):
            self.install()
        self.assertEqual(self.target.read_bytes(), UPDATED)

    def test_unknown_source_and_corrupt_patch_fail_without_mutation(self):
        for source, patch in ((b'unknown source', PATCH), (SOURCE, PATCH + b'corrupt')):
            with self.subTest(source=source, patch_corrupt=patch != PATCH):
                self.target.write_bytes(source)
                self.patch.write_bytes(patch)
                with self.assertRaisesRegex(ValueError, 'checksum'):
                    self.install()
                self.assertEqual(self.target.read_bytes(), source)

    def test_valid_patch_hash_but_wrong_result_refused(self):
        self.patch.write_bytes(PATCH.replace(b'+two = 3', b'+two = 4'))
        with mock.patch.object(runtime, 'PATCH_SHA', sha(self.patch.read_bytes())):
            with self.assertRaisesRegex(ValueError, 'patched.*checksum'):
                self.install()
        self.assertEqual(self.target.read_bytes(), SOURCE)

    def test_duplicate_hunk_or_context_never_fuzzy_applies(self):
        duplicate = PATCH + PATCH[PATCH.index(b'@@ '):]
        with self.assertRaises(ValueError):
            runtime.apply_patch(SOURCE, duplicate)
        with self.assertRaisesRegex(ValueError, 'context'):
            runtime.apply_patch(b'one = 1\ntwo = 9\none = 1\ntwo = 2\n', PATCH)
        for invalid in (PATCH.replace(b'-1,2', b'-1,3'), PATCH.replace(b'+++ b/serve/server.py', b'+++ b/other.py')):
            with self.assertRaises(ValueError):
                runtime.apply_patch(SOURCE, invalid)

    def test_readonly_symlink_and_replace_failure_refused(self):
        self.target.chmod(0o444)
        with self.assertRaisesRegex(ValueError, 'read-only'):
            self.install()
        self.target.chmod(0o640)
        with mock.patch.object(runtime.os, 'replace', side_effect=OSError('read-only filesystem')):
            with self.assertRaisesRegex(OSError, 'read-only'):
                self.install()
        self.assertEqual(self.target.read_bytes(), SOURCE)
        self.assertEqual(sorted(p.name for p in self.root.iterdir()), ['initial-queue.patch', 'server.py'])
        self.target.unlink()
        self.target.symlink_to(self.patch)
        with self.assertRaisesRegex(ValueError, 'symlink'):
            self.install()

    def test_bounded_input(self):
        with mock.patch.object(runtime, 'MAX_SOURCE_BYTES', 4):
            with self.assertRaisesRegex(ValueError, 'size'):
                self.install()
        with mock.patch.object(runtime, 'MAX_PATCH_BYTES', 4):
            with self.assertRaisesRegex(ValueError, 'size'):
                self.install()

    def test_startup_diagnostic_prevents_exec_on_failure(self):
        output = io.StringIO()
        with mock.patch.object(runtime, 'install', side_effect=OSError('read-only filesystem')), mock.patch.object(runtime.os, 'execv') as execute, contextlib.redirect_stderr(output):
            self.assertEqual(runtime.main(), 1)
            execute.assert_not_called()
        self.assertIn('Strata queue telemetry startup failed', output.getvalue())
        self.assertIn('read-only filesystem', output.getvalue())

    def test_exec_preserves_argv_without_shell(self):
        output = self.root / 'argv.json'
        entrypoint = self.root / 'original-entrypoint'
        entrypoint.write_text('#!' + sys.executable + '\nimport json,sys\nfrom pathlib import Path\nPath(sys.argv[1]).write_text(json.dumps(sys.argv[2:]))\n')
        entrypoint.chmod(0o755)
        args = ['--model', 'a path with spaces', '--context', '4096', '$(touch do-not-create)', '--', '尾部参数']
        code = 'import importlib.util,sys; s=importlib.util.spec_from_file_location("q",sys.argv[1]); m=importlib.util.module_from_spec(s); s.loader.exec_module(m); m.ENTRYPOINT=sys.argv[2]; m.install=lambda:None; sys.argv=["wrapper",*sys.argv[3:]]; raise SystemExit(m.main())'
        result = subprocess.run([sys.executable, '-c', code, str(HERE / 'strata_queue.py'), str(entrypoint), str(output), *args], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(output.read_text()), args)


class Bundle(unittest.TestCase):
    def test_reproducible_contents_and_metadata(self):
        data = build.bundle()
        self.assertEqual(data, build.bundle())
        with tarfile.open(fileobj=io.BytesIO(data), mode='r:gz') as archive:
            self.assertEqual(archive.getnames(), list(build.FILES))
            for member in archive.getmembers():
                self.assertEqual((member.uid, member.gid, member.mtime, member.mode), (0, 0, 0, 0o644))
                self.assertEqual(archive.extractfile(member).read(), (HERE / member.name).read_bytes())
        self.assertIn(b'MIT License', (HERE / 'LICENSE.txt').read_bytes())
        self.assertEqual(sha((HERE / 'initial-queue.patch').read_bytes()), runtime.PATCH_SHA)



if __name__ == '__main__':
    unittest.main(verbosity=2)
