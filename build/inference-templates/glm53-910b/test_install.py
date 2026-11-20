import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("glm_install", Path(__file__).with_name("install.py"))
installer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(installer)


class InstallTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name) / "container"
        self.bundle = Path(self.temporary.name) / "bundle"
        self.root.mkdir()
        (self.bundle / "payload").mkdir(parents=True)
        self.files = []
        for name, before, after in [("existing.py", b"original\n", b"patched\n"),
                                    ("new.py", None, b"new module\n")]:
            if before is not None:
                (self.root / name).write_bytes(before)
            (self.bundle / "payload" / name).write_bytes(after)
            self.files.append({"path": name,
                               "base_sha256": hashlib.sha256(before).hexdigest() if before else None,
                               "sha256": hashlib.sha256(after).hexdigest()})
        (self.bundle / "manifest.json").write_text(json.dumps({"files": self.files}))

    def test_installs_existing_and_new_modules_and_preserves_restart(self):
        installer.install(self.bundle, self.root)
        self.assertEqual((self.root / "existing.py").read_bytes(), b"patched\n")
        self.assertEqual((self.root / "new.py").read_bytes(), b"new module\n")
        mtime = (self.root / "existing.py").stat().st_mtime_ns
        installer.install(self.bundle, self.root)
        self.assertEqual((self.root / "existing.py").stat().st_mtime_ns, mtime)

    def test_bad_later_payload_does_not_modify_earlier_module(self):
        (self.bundle / "payload/new.py").write_bytes(b"corrupt")
        with self.assertRaisesRegex(ValueError, "payload checksum mismatch"):
            installer.install(self.bundle, self.root)
        self.assertEqual((self.root / "existing.py").read_bytes(), b"original\n")
        self.assertFalse((self.root / "new.py").exists())

    def test_unknown_base_is_not_overwritten(self):
        (self.root / "existing.py").write_bytes(b"another engine version")
        with self.assertRaisesRegex(ValueError, "unexpected base module"):
            installer.install(self.bundle, self.root)
        self.assertEqual((self.root / "existing.py").read_bytes(), b"another engine version")
        self.assertFalse((self.root / "new.py").exists())
