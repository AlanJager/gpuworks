#!/usr/bin/env bash
set -euo pipefail

probe_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
scratch="$(mktemp -d)"
trap 'rm -rf -- "${scratch}"' EXIT

cc -O2 -Wall -Wextra "${probe_dir}/probe.c" -ldl -o "${scratch}/probe"
cat > "${scratch}/fake-hip.c" <<'EOF'
#include <stdint.h>
#include <stdlib.h>

int hipGetDeviceCount(int *count) {
    *count = getenv("FAKE_NO_DEVICE") ? 0 : 1;
    return 0;
}
int hipSetDevice(int device) { return device == 0 ? 0 : 1; }
int hipMemGetInfo(size_t *free_bytes, size_t *total_bytes) {
    const char *visible = getenv("FAKE_VISIBLE_MIB");
    *total_bytes = (size_t)(visible ? strtoull(visible, NULL, 10) : 4096) << 20;
    *free_bytes = *total_bytes;
    return 0;
}
int hipMalloc(void **ptr, size_t bytes) {
    if (bytes > (4096ULL << 20) && !getenv("FAKE_OVERALLOC_SUCCEEDS")) {
        *ptr = NULL;
        return getenv("FAKE_WRONG_OOM") ? 100 : 2;
    }
    *ptr = malloc(1);
    return *ptr ? 0 : 2;
}
#ifndef OMIT_FREE
int hipFree(void *ptr) { free(ptr); return 0; }
#endif
EOF
cc -shared -fPIC -Wall -Wextra "${scratch}/fake-hip.c" -o "${scratch}/libamdhip64.so"
mkdir "${scratch}/missing-symbol"
cc -shared -fPIC -DOMIT_FREE -Wall -Wextra "${scratch}/fake-hip.c" -o "${scratch}/missing-symbol/libamdhip64.so"

export GPUWORKS_PARTITION_PROBE_BACKEND=hygon-dgpu
export GPUWORKS_PARTITION_PROBE_QUOTA_MIB=4096
LD_LIBRARY_PATH="${scratch}" "${scratch}/probe" > "${scratch}/output"
grep -q 'quota_enforced=true' "${scratch}/output"

expect_failure() {
    local pattern="$1"
    shift
    if "$@" > "${scratch}/output" 2>&1; then
        echo "probe unexpectedly passed: ${pattern}" >&2
        exit 1
    fi
    grep -q "${pattern}" "${scratch}/output"
}
expect_failure 'slice view mismatch' env FAKE_VISIBLE_MIB=8192 LD_LIBRARY_PATH="${scratch}" "${scratch}/probe"
expect_failure 'visible device count mismatch' env FAKE_NO_DEVICE=1 LD_LIBRARY_PATH="${scratch}" "${scratch}/probe"
expect_failure 'over-quota allocation unexpectedly succeeded' env FAKE_OVERALLOC_SUCCEEDS=1 LD_LIBRARY_PATH="${scratch}" "${scratch}/probe"
expect_failure 'want OOM(2)' env FAKE_WRONG_OOM=1 LD_LIBRARY_PATH="${scratch}" "${scratch}/probe"
expect_failure 'required HIP memory symbols unavailable' env LD_LIBRARY_PATH="${scratch}/missing-symbol" "${scratch}/probe"
if [[ -n "${GPUWORKS_PROBE_TEST_IMAGE:-}" ]]; then
    docker run --rm --network none --volume "${scratch}:/fake:ro" \
        --env LD_LIBRARY_PATH=/fake \
        --env GPUWORKS_PARTITION_PROBE_BACKEND \
        --env GPUWORKS_PARTITION_PROBE_QUOTA_MIB \
        "${GPUWORKS_PROBE_TEST_IMAGE}" > "${scratch}/output"
    grep -q 'quota_enforced=true' "${scratch}/output"
fi
echo 'Hygon HIP probe success and fail-closed cases passed'
