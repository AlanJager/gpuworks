#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
commit="bac3d461c9312acfaa388bed89e9ed2a61971d1a"
goarch="${GOARCH:?set GOARCH to amd64 or arm64}"
case "${goarch}" in amd64|arm64) ;; *) echo "unsupported GOARCH: ${goarch}" >&2; exit 2 ;; esac
# The pinned vcann-rt source and patches define the shipped ABI. The CANN image
# supplies only a reproducible cross-architecture compiler/toolchain; it does
# not create separate CANN 8/9 runtime assets.
default_image="quay.io/ascend/cann:9.0.0-devel@sha256:925d4743fdc28db9e5e5d4081b65c6814aa9827a11df2df9c7096cf205cbb67d"
image="${ASCEND_BUILD_IMAGE:-${default_image}}"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

git init -q "${work}/ubs-virt"
git -C "${work}/ubs-virt" remote add origin https://atomgit.com/openeuler/ubs-virt.git
git -C "${work}/ubs-virt" fetch --depth=1 origin "refs/keep-around/${commit}"
git -C "${work}/ubs-virt" checkout -q --detach FETCH_HEAD
source_dir="${work}/ubs-virt/ubs-virt-enpu/vcann-rt"
out_dir="${work}/out"
mkdir -p "${out_dir}"
docker run --rm --platform "linux/${goarch}" \
  -v "${source_dir}:/src:ro" \
  -v "${repo_root}/build/runtime-assets/ascend:/pipeline:ro" \
  -v "${repo_root}/build/runtime-assets/ascend/patches:/vcann-rt-patches:ro" \
  -v "${out_dir}:/out" \
  "${image}" bash /pipeline/build-libvnpu.sh /src /out
target="${repo_root}/internal/assets/bin/runtime-assets/ascend/linux-${goarch}"
mkdir -p "${target}"
install -m 0755 "${out_dir}/lib/libvruntime.so" "${target}/libvruntime.so"
sha256sum "${target}/libvruntime.so"
