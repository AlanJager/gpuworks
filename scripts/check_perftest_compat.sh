#!/usr/bin/env bash
# Checks the ib_write_bw builds embedded in the agent: each loads only the
# RDMA user-space libraries a host with rdma-core has, needs no newer glibc
# or rdma-core symbol versions than the oldest supported hosts provide, and
# matches the checksum manifest. See build/runtime-assets/perftest/README.md.
set -euo pipefail

dir="${1:-internal/assets/bin}"
max_glibc="${GPUWORKS_MAX_GLIBC:-2.17}"
# rdma-core 33 provides IBVERBS_1.12; EL8, Kylin V10 and Ubuntu 22.04 ship newer.
max_ibverbs="${GPUWORKS_MAX_IBVERBS:-1.12}"
allowed_needed="libc.so.6 libm.so.6 libdl.so.2 libpthread.so.0 libibverbs.so.1 librdmacm.so.1 libpci.so.3"

command -v objdump >/dev/null || { echo "objdump is required" >&2; exit 1; }
command -v sha256sum >/dev/null || { echo "sha256sum is required" >&2; exit 1; }

highest() { # $1 prefix, $2 asset
  objdump -T "$2" | grep -oE "$1_[0-9]+(\.[0-9]+)*" | sed "s/^$1_//" | sort -V | tail -1
}
not_above() { # $1 version, $2 limit
  [[ "$(printf '%s\n' "$1" "$2" | sort -V | tail -1)" == "$2" ]]
}

for asset in "$dir/ib_write_bw-x86_64" "$dir/ib_write_bw-arm64"; do
  [[ -f "$asset" ]] || { echo "missing perftest asset: $asset" >&2; exit 1; }
  glibc="$(highest GLIBC "$asset")"
  ibverbs="$(highest IBVERBS "$asset")"
  if [[ -z "$glibc" || -z "$ibverbs" ]]; then
    echo "$asset: no GLIBC or IBVERBS symbol versions (not a dynamically linked verbs program?)" >&2
    exit 1
  fi
  not_above "$glibc" "$max_glibc" || { echo "$asset requires GLIBC_$glibc, maximum allowed is GLIBC_$max_glibc" >&2; exit 1; }
  not_above "$ibverbs" "$max_ibverbs" || { echo "$asset requires IBVERBS_$ibverbs, maximum allowed is IBVERBS_$max_ibverbs" >&2; exit 1; }
  needed="$(objdump -p "$asset" | awk '/NEEDED/ {print $2}')"
  for library in $needed; do
    # A vendor direct-verbs library (libmlx5, libhns, libefa) would stop the
    # tool loading on hosts with another vendor's NIC.
    grep -qw -- "$library" <<<"$allowed_needed" || { echo "$asset links $library, which not every RDMA host has" >&2; exit 1; }
  done
  name="$(basename "$asset")"
  expected="$(awk -v name="$name" '$2 == name {print $1}' "$dir/manifest.sha256")"
  actual="$(sha256sum "$asset" | awk '{print $1}')"
  if [[ -z "$expected" || "$actual" != "$expected" ]]; then
    echo "$name sha256 mismatch: manifest=$expected actual=$actual" >&2
    exit 1
  fi
  echo "$name highest_glibc=GLIBC_$glibc highest_ibverbs=IBVERBS_$ibverbs needed=$(tr '\n' ' ' <<<"$needed")"
done
