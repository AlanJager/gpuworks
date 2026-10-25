#!/usr/bin/env python3
"""Rebuild the catalog asset from checked vendor modules and reviewable patches."""
import argparse
import gzip
import hashlib
import io
import json
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[2]
DEFAULT_OUTPUT = REPO / "internal/server/inferencecatalog/seed/catalog/models/gpuworks/glm-5.3-flash-910c/assets/runtime.tar.gz"


def build(base, staging, manifest):
    payload = staging / "payload"
    payload.mkdir()
    for item in manifest["files"]:
        source, target = base / item["path"], payload / item["path"]
        actual = hashlib.sha256(source.read_bytes()).hexdigest() if source.exists() else None
        if actual != item["base_sha256"]:
            raise ValueError(f"base checksum mismatch: {item['path']}")
        target.parent.mkdir(parents=True, exist_ok=True)
        if source.exists():
            shutil.copyfile(source, target)
        subprocess.run(["patch", "--batch", "--fuzz=0", "-p1", "-i", str(HERE / item["patch"])],
                       cwd=payload, check=True, stdout=subprocess.DEVNULL)
        if hashlib.sha256(target.read_bytes()).hexdigest() != item["sha256"]:
            raise ValueError(f"patched checksum mismatch: {item['path']}")
    for name in ("manifest.json", "install.py", "serve.sh", "NOTICE.txt", "LICENSE.txt"):
        shutil.copyfile(HERE / name, staging / name)
    data = io.BytesIO()
    with gzip.GzipFile(fileobj=data, mode="wb", mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode="w") as archive:
            for path in sorted(staging.rglob("*")):
                if not path.is_file():
                    continue
                content = path.read_bytes()
                entry = tarfile.TarInfo(path.relative_to(staging).as_posix())
                entry.mode, entry.size = 0o644, len(content)
                archive.addfile(entry, io.BytesIO(content))
    return data.getvalue()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-root", type=Path, help="already extracted base modules; hashes are still checked")
    parser.add_argument("--image", help="local alias of the pinned image; its image ID must match")
    parser.add_argument("--output", type=Path, default=DEFAULT_OUTPUT)
    args = parser.parse_args()
    manifest = json.loads((HERE / "manifest.json").read_text())
    with tempfile.TemporaryDirectory() as temporary:
        root = Path(temporary)
        base = args.base_root or root / "base"
        if args.base_root is None:
            image = args.image or manifest["image"]
            inspected = json.loads(subprocess.check_output(["docker", "image", "inspect", image]))[0]
            if inspected["Id"] != manifest["image_id"]:
                raise ValueError("image ID differs from the tested base")
            container = subprocess.check_output(["docker", "create", "--runtime", "runc", image, "true"], text=True).strip()
            try:
                for item in manifest["files"]:
                    if item["base_sha256"] is None:
                        continue
                    target = base / item["path"]
                    target.parent.mkdir(parents=True, exist_ok=True)
                    subprocess.run(["docker", "cp", f"{container}:/{item['path']}", str(target)], check=True)
            finally:
                subprocess.run(["docker", "rm", container], check=True, stdout=subprocess.DEVNULL)
        staging = root / "bundle"
        staging.mkdir()
        data = build(base, staging, manifest)
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_bytes(data)
        print(f"{hashlib.sha256(data).hexdigest()}  {args.output}")


if __name__ == "__main__":
    main()
