#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
commit="ff55d90bfcafb9c813f318857b6942092d788d0a"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
git clone --filter=blob:none https://github.com/gpustack/gpustack-operator.git "${work}/operator"
git -C "${work}/operator" checkout --detach "${commit}"
cp -a "${work}/operator/csrc/amd/rocm-slicing-shim" "${work}/shim"
out="${work}/out"
mkdir -p "${out}"
# Pinned by digest to the same ROCm 7.2.3 base the gpuworks-runner vLLM/ROCm
# image is built from (build/runner-images/rocm/README.md): gcc 11 and
# /opt/rocm headers, which hip/ needs to compile against (the resulting
# libvrocm.so itself links only libc — see build/runtime-assets/README.md).
default_image="vllm/vllm-openai-rocm@sha256:2e7da1ad1c66836802072588adea75f9f4991da5f9545b4318e91d422c22ce6a"
docker run --rm --platform linux/amd64 \
  -v "${work}/shim:/src" -v "${out}:/out" \
  "${AMD_BUILD_IMAGE:-${default_image}}" \
  bash -lc 'cd /src && OUT=/out bash build.sh lib'
target="${repo_root}/internal/assets/bin/runtime-assets/amd/linux-amd64"
mkdir -p "${target}"
install -m 0755 "${out}/libvrocm.so" "${target}/libvrocm.so"
sha256sum "${target}/libvrocm.so"
