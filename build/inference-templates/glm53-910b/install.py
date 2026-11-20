#!/usr/bin/env python3
"""Install the template's verified GLM modules in this container's writable layer."""
import hashlib
import json
from pathlib import Path
import os
import tempfile


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest() if path.exists() else None


def install(bundle, root=Path("/")):
    manifest = json.loads((bundle / "manifest.json").read_text())
    pending = []
    # Check every input before replacing any module. An unexpected image fails
    # closed, and a restarted container may already contain the expected output.
    for item in manifest["files"]:
        relative = Path(item["path"])
        if relative.is_absolute() or ".." in relative.parts:
            raise ValueError(f"invalid module path: {relative}")
        source, target = bundle / "payload" / relative, root / relative
        if digest(source) != item["sha256"]:
            raise ValueError(f"template payload checksum mismatch: {relative}")
        actual = digest(target)
        if actual == item["sha256"]:
            continue
        if actual != item["base_sha256"]:
            raise ValueError(f"unexpected base module: {relative}")
        pending.append((source, target))
    for source, target in pending:
        target.parent.mkdir(parents=True, exist_ok=True)
        descriptor, temporary = tempfile.mkstemp(dir=target.parent)
        try:
            with os.fdopen(descriptor, "wb") as output:
                output.write(source.read_bytes())
            os.chmod(temporary, 0o644)
            os.replace(temporary, target)
        finally:
            if os.path.exists(temporary):
                os.unlink(temporary)
    print(f"GLM template modules verified: {len(manifest['files'])}", flush=True)


if __name__ == "__main__":
    install(Path(__file__).resolve().parent)
