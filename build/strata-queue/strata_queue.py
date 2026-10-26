"""Apply the exact reviewed Strata queue patch before its original entrypoint.

Upstream: Niko1221/Strata v0.1.38, commit 09817da3979abfeb5fbdef7c8f1223d120c1ad39.
Patch and upstream excerpts are MIT licensed; see the bundled LICENSE.txt.
Only initial admission FIFO waits are exported; later MCP rounds are excluded.
"""
import hashlib
import os
from pathlib import Path
import re
import stat
import sys
import tempfile

SERVER = Path('/opt/strata/serve/server.py')
ENTRYPOINT = '/opt/strata/gpuworks-entrypoint.sh'
PATCH = Path(__file__).with_name('initial-queue.patch')
SOURCE_SHA = 'fc85cfd3a5b0379c8482de7a81673cae76299751aa5fe614413bc01f1fa54408'
PATCHED_SHA = '7f0ddfd4a07181e37a34e61a8b4587e3a97ba582aff8250747f918e93d7fa5cd'
PATCH_SHA = '1743e4965c127b1a38d4c4e1060969bf63515d099e832f24ee421e7a595fee2f'
MAX_SOURCE_BYTES = 1 << 20
MAX_PATCH_BYTES = 64 << 10


def read_bounded(path, limit):
    with path.open('rb') as source:
        data = source.read(limit + 1)
    if len(data) > limit:
        raise ValueError(f'{path.name} exceeds supported size')
    return data


def apply_patch(source, patch):
    """Apply one file's exact line-numbered hunks, without fuzzy search or shell."""
    lines = source.decode('utf-8').splitlines(keepends=True)
    changes = patch.decode('utf-8').splitlines(keepends=True)
    if changes[:2] != ['--- a/serve/server.py\n', '+++ b/serve/server.py\n']:
        raise ValueError('unsupported queue patch file header')
    out, cursor, index, hunks = [], 0, 2, 0
    while index < len(changes):
        match = re.fullmatch(r'@@ -(\d+),(\d+) \+(\d+),(\d+) @@\n', changes[index])
        if match is None:
            raise ValueError('invalid queue patch hunk header')
        old_start, old_count, new_start, new_count = map(int, match.groups())
        start = old_start - 1
        if start < cursor or start > len(lines):
            raise ValueError('overlapping or out-of-range queue patch hunk')
        out.extend(lines[cursor:start])
        if new_start != len(out) + 1:
            raise ValueError('inconsistent queue patch output position')
        cursor, removed, added = start, 0, 0
        index += 1
        while index < len(changes) and not changes[index].startswith('@@ '):
            line = changes[index]
            if line[:1] in (' ', '-'):
                if cursor >= len(lines) or lines[cursor] != line[1:]:
                    raise ValueError('queue patch context does not match exactly')
                cursor += 1
                removed += 1
            if line[:1] in (' ', '+'):
                out.append(line[1:])
                added += 1
            if line[:1] not in (' ', '+', '-'):
                raise ValueError('unsupported queue patch line')
            index += 1
        if (removed, added) != (old_count, new_count):
            raise ValueError('queue patch hunk counts do not match')
        hunks += 1
    if hunks == 0:
        raise ValueError('empty queue patch')
    out.extend(lines[cursor:])
    return ''.join(out).encode('utf-8')


def install(target=SERVER, patch_path=PATCH):
    patch = read_bounded(patch_path, MAX_PATCH_BYTES)
    if hashlib.sha256(patch).hexdigest() != PATCH_SHA:
        raise ValueError('queue patch checksum mismatch')
    if target.is_symlink():
        raise ValueError('Strata server.py must be a regular file, not a symlink')
    info = target.stat()
    if not stat.S_ISREG(info.st_mode):
        raise ValueError('Strata server.py must be a regular file')
    source = read_bounded(target, MAX_SOURCE_BYTES)
    digest = hashlib.sha256(source).hexdigest()
    if digest == PATCHED_SHA:
        return
    if digest != SOURCE_SHA:
        raise ValueError('unsupported Strata server.py checksum; expected reviewed v0.1.38')
    if not info.st_mode & 0o222:
        raise ValueError('Strata server.py is read-only; queue telemetry cannot be installed')
    updated = apply_patch(source, patch)
    if hashlib.sha256(updated).hexdigest() != PATCHED_SHA:
        raise ValueError('patched Strata server.py checksum mismatch')
    compile(updated, str(target), 'exec')
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(dir=target.parent, prefix='.strata-queue-', delete=False) as output:
            temporary = Path(output.name)
            output.write(updated)
            output.flush()
            os.fchmod(output.fileno(), stat.S_IMODE(info.st_mode))
            os.fsync(output.fileno())
        # Replacement breaks shared image hardlinks; never modify their inode.
        os.replace(temporary, target)
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)


def main():
    try:
        install()
        os.execv(ENTRYPOINT, [ENTRYPOINT, *sys.argv[1:]])
    except (OSError, ValueError, UnicodeError) as error:
        print(f'Strata queue telemetry startup failed: {error}', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
