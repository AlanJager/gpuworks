#!/usr/bin/env python3
"""Pack the reviewable pre-start patch into the catalog asset, reproducibly.

The archive is byte-stable (fixed member metadata, no gzip timestamp), so the
SHA-256 the spec pins only changes when the patch itself changes.
"""
import argparse
import gzip
import hashlib
import io
from pathlib import Path
import tarfile

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[2]
MEMBER = "ascend_sleep_pool_patch.py"
DEFAULT_OUTPUT = (REPO / "internal/server/inferencecatalog/seed/catalog/models/qwen"
                  / "qwen3.8-flash-next/assets/ascend-sleep-pool-patch.tar.gz")


def build(script):
    data = io.BytesIO()
    with gzip.GzipFile(fileobj=data, mode="wb", mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode="w") as archive:
            entry = tarfile.TarInfo(MEMBER)
            entry.mode, entry.size = 0o644, len(script)
            archive.addfile(entry, io.BytesIO(script))
    return data.getvalue()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, default=DEFAULT_OUTPUT)
    args = parser.parse_args()
    script = (HERE / MEMBER).read_bytes()
    compile(script, MEMBER, "exec")
    data = build(script)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_bytes(data)
    print(f"{hashlib.sha256(data).hexdigest()}  {len(data)}  {args.output}")


if __name__ == "__main__":
    main()
