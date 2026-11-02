"""Reproduce the catalog's small Strata patch/entrypoint bundle using stdlib."""
import argparse
import gzip
import hashlib
import io
from pathlib import Path
import tarfile

HERE = Path(__file__).resolve().parent
OUTPUT = HERE.parents[1] / 'internal/server/inferencecatalog/seed/catalog/models/ista-daslab/qwen3.8-flash-next-strata/assets/strata-queue-runtime.tar.gz'
FILES = ('LICENSE.txt', 'initial-queue.patch', 'strata_queue.py')


def bundle():
    result = io.BytesIO()
    with gzip.GzipFile(fileobj=result, mode='wb', filename='', mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode='w', format=tarfile.USTAR_FORMAT) as archive:
            for name in FILES:
                data = (HERE / name).read_bytes()
                info = tarfile.TarInfo(name)
                info.size, info.mode, info.mtime = len(data), 0o644, 0
                archive.addfile(info, io.BytesIO(data))
    return result.getvalue()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--check', action='store_true', help='verify the committed asset without writing')
    args = parser.parse_args()
    data = bundle()
    if args.check:
        if not OUTPUT.is_file() or OUTPUT.read_bytes() != data:
            parser.exit(1, 'Strata queue runtime asset is stale; run build/strata-queue/build.py\n')
    else:
        OUTPUT.parent.mkdir(parents=True, exist_ok=True)
        OUTPUT.write_bytes(data)
    print(hashlib.sha256(data).hexdigest())


if __name__ == '__main__':
    main()
