#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

for arch in amd64 arm64; do
  PROBE_GOARCH="${arch}" "${repo_root}/build/runtime-assets/probe/build-probe-image.sh"
done

for arch in amd64 arm64; do
  GOARCH="${arch}" "${repo_root}/build/runtime-assets/ascend/build.sh"
done

"${repo_root}/build/runtime-assets/ppu/build.sh"
"${repo_root}/build/runtime-assets/amd/build.sh"
"${repo_root}/build/runtime-assets/update-checksums.sh"
"${repo_root}/build/runtime-assets/verify.sh"
