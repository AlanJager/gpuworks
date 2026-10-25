#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
asset="$root/internal/assets/bin/ascend-runveil-compat-arm64.so"
(cd "$root/internal/assets/bin"; awk '$2 == "ascend-runveil-compat-arm64.so"' manifest.sha256 | sha256sum -c -)
readelf -h "$asset" | grep -q 'Machine:.*AArch64'
exports=$(nm -D --defined-only "$asset" | awk '{print $3}')
test "$exports" = halGetDeviceInfo || { echo "unexpected compatibility exports: $exports" >&2; exit 1; }
python3 - "$asset" <<'PY'
import re,subprocess,sys
symbols=subprocess.check_output(['objdump','-T',sys.argv[1]],text=True)
versions=[tuple(map(int,v.split('.'))) for v in re.findall(r'GLIBC_([0-9.]+)',symbols)]
assert versions and max(versions)<=(2,28), 'Ascend compatibility requires glibc newer than 2.28'
PY
