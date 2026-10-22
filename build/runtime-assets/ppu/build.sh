#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
commit="fa399c037a96d2cb3e1e755e0a7dbce65773caeb"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
git clone --filter=blob:none https://github.com/gpustack/gpustack-operator.git "${work}/operator"
git -C "${work}/operator" checkout --detach "${commit}"
cp -a "${work}/operator/csrc/thead/ppu-slicing-shim" "${work}/shim"
patch -d "${work}/shim" -p1 --batch --forward < "${repo_root}/build/runtime-assets/ppu/patches/0001-resolve-hggc-via-explicit-handle.patch"
out="${work}/out"
mkdir -p "${out}"
default_image="gpustack/thead-ppu-devel:2.1.1@sha256:529dcfa20cfbb5552784b7916f18ca510903bcc1f7d293bf49532a1b9d499b1a"
docker run --rm --platform linux/amd64 \
  -v "${work}/shim:/src" -v "${out}:/out" \
  "${PPU_BUILD_IMAGE:-${default_image}}" \
  bash -lc 'cd /src && OUT=/out bash build.sh lib hggc_quota hgml_dlsym_hook'
target="${repo_root}/internal/assets/bin/runtime-assets/ppu/linux-amd64/hggc-13.0"
mkdir -p "${target}"
install -m 0755 "${out}/hggc_quota.so" "${target}/hggc_quota.so"
install -m 0755 "${out}/hgml_dlsym_hook.so" "${target}/hgml_dlsym_hook.so"
sha256sum "${target}"/*.so
