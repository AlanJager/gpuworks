# Embedded ib_write_bw

The agent measures RDMA bandwidth with perftest's `ib_write_bw`. It runs the
host's copy when `ib_write_bw` is on `PATH`, else this embedded build
(`internal/assets/bin/ib_write_bw-{x86_64,arm64}`, extracted to the agent's
state directory), else it reports that perftest must be installed. See
`internal/agent/perftest/locate.go`.

The embedded build covers hosts that have the RDMA user-space libraries
(rdma-core, or a vendor stack such as MLNX_OFED) but not the perftest package.
The agent is a static, cgo-free binary and cannot speak verbs itself; RDMA
traffic always goes through the host's `libibverbs` and its NIC provider, so
this build links them dynamically instead of carrying its own.

## Build

```sh
GOARCH=amd64 build/runtime-assets/perftest/build.sh
GOARCH=arm64 build/runtime-assets/perftest/build.sh
```

The script downloads perftest and rdma-core at the versions and SHA-256
pinned in `build.sh` (or takes them from `PERFTEST_SOURCE_DIR`), builds in a
Debian 10 container, and writes the binary into `internal/assets/bin/`. Then
update `internal/assets/bin/manifest.sha256` with the printed checksums and run
`make check-perftest`. `PERFTEST_APT_MIRROR` points apt at a Debian archive
mirror when `archive.debian.org` is slow.

| Choice | Why |
|---|---|
| Debian 10 (glibc 2.28) | The binary needs glibc ≤ 2.17 symbol versions (2.16 on x86_64), so it runs on EL7-era hosts as well as Kylin V10 and EL8. |
| rdma-core 35.0 headers | perftest 23.10 and later do not compile against Debian 10's rdma-core 22. At run time the host's own libraries load; the symbol versions linked in (up to `IBVERBS_1.12`) set the oldest rdma-core the host may have. |
| No vendor direct-verbs (`libmlx5`, `libhns`, `libefa`) | Their headers are deleted from the build prefix, so perftest builds generic verbs only and loads on any vendor's NIC. |
| `--disable-ibv_wr_api` | The hns (Huawei) provider of rdma-core 35 fails to create QPs for the `ibv_wr_*` send API. The classic post-send path measured the same bandwidth: 24.50 Gb/s against the host tool's 24.51 Gb/s on a 25G hns rail. |
| Patch `0001-include-immintrin-for-mm-pause` | perftest 26.07.8 calls `_mm_pause()` on x86 but includes `<immintrin.h>` only when it detects AVX; gcc 8 then fails to link. |

The binary still needs `libpci.so.3` (perftest's PCIe relaxed-ordering check),
which every host with pciutils has.

## Validation

`make check-perftest` (part of `make check-assets`) enforces the glibc and
`IBVERBS` ceilings, the allowed `NEEDED` libraries and the checksums.

Validated on 2026-09-28, see
`artifacts/archive/2026-09-28-rdma-zero-config/acceptance-2026-09-28-perftest.md`:
the build starts on Kylin V10 (arm64 and x86_64, rdma-core 35 and MLNX_OFED
23.10), Helix 8.4 without perftest, and Ubuntu 22.04; between two Ascend
nodes on 25G hns RoCE it interoperates with the host's perftest 5.96 in both
directions at the host tool's bandwidth.
