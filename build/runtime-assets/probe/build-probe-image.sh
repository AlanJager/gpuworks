#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
arch="${PROBE_GOARCH:-$(uname -m)}"
case "${arch}" in
  x86_64|amd64) goarch=amd64 ;;
  aarch64|arm64) goarch=arm64 ;;
  *) echo "unsupported probe build architecture: ${arch}" >&2; exit 2 ;;
esac
out_dir="${repo_root}/internal/assets/bin/runtime-assets/probe"
mkdir -p "${out_dir}"
oci_tmp="$(mktemp "${out_dir}/.partition-probe-linux-${goarch}.oci.XXXXXX")"
docker_tmp="$(mktemp "${out_dir}/.partition-probe-linux-${goarch}.docker.XXXXXX")"
trap 'rm -f -- "${oci_tmp}" "${docker_tmp}"' EXIT

docker buildx build \
  --platform "linux/${goarch}" \
  --provenance=false \
  --sbom=false \
  --file "${repo_root}/build/runtime-assets/probe/Dockerfile" \
  --tag gpuworks/partition-probe:v2 \
  --output "type=oci,dest=${oci_tmp}" \
  "${repo_root}/build/runtime-assets/probe"
image_id="$(python3 "${repo_root}/build/runtime-assets/probe/oci-to-docker-archive.py" \
  "${oci_tmp}" "${docker_tmp}" \
  --image gpuworks/partition-probe:v2 \
  --arch "${goarch}")"
gzip -n -9 < "${docker_tmp}" > "${out_dir}/partition-probe-linux-${goarch}.tar.gz"
(cd "${out_dir}" && sha256sum "partition-probe-linux-${goarch}.tar.gz" > "partition-probe-linux-${goarch}.tar.gz.sha256")
echo "built ${image_id} -> ${out_dir}/partition-probe-linux-${goarch}.tar.gz"
