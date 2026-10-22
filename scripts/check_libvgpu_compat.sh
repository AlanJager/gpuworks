#!/usr/bin/env bash
set -euo pipefail

asset="${1:-internal/assets/bin/libvgpu.so}"
max_glibc="${GPUWORKS_MAX_GLIBC:-2.17}"

if [[ ! -f "$asset" ]]; then
  echo "missing libvgpu asset: $asset" >&2
  exit 1
fi
command -v objdump >/dev/null || { echo "objdump is required" >&2; exit 1; }
command -v sha256sum >/dev/null || { echo "sha256sum is required" >&2; exit 1; }

versions="$(objdump -T "$asset" | grep -oE 'GLIBC_[0-9]+(\.[0-9]+)*' | sort -Vu || true)"
if [[ -z "$versions" ]]; then
  echo "no GLIBC symbol versions found in $asset" >&2
  exit 1
fi

highest="$(printf '%s\n' "$versions" | sed 's/^GLIBC_//' | sort -V | tail -1)"
if [[ "$(printf '%s\n' "$highest" "$max_glibc" | sort -V | tail -1)" != "$max_glibc" ]]; then
  echo "libvgpu requires GLIBC_$highest, maximum allowed is GLIBC_$max_glibc" >&2
  exit 1
fi

echo "libvgpu=$asset highest_glibc=GLIBC_$highest max_allowed=GLIBC_$max_glibc"
needed="$(objdump -p "$asset" | awk '/NEEDED/ {print $2}')"
for library in libcuda.so.1 libnvidia-ml.so.1 libc.so.6; do
  if ! grep -qx "$library" <<<"$needed"; then
    echo "libvgpu is missing required dynamic dependency: $library" >&2
    exit 1
  fi
done
printf '  NEEDED %s\n' $needed

asset_dir="$(cd "$(dirname "$asset")" && pwd)"
asset_name="$(basename "$asset")"
expected="$(awk -v name="$asset_name" '$2 == name {print $1}' "$asset_dir/manifest.sha256")"
actual="$(sha256sum "$asset" | awk '{print $1}')"
if [[ -z "$expected" || "$actual" != "$expected" ]]; then
  echo "libvgpu sha256 mismatch: manifest=$expected actual=$actual" >&2
  exit 1
fi
