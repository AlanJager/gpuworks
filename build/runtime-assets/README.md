# Runtime assets

This directory is the reproducible source of the logical-partition assets
embedded in the agent. Upstream source pins and local patches are recorded in
`manifest.json`; `probe/build-probe-image.sh` builds an offline behavioral
probe image for a requested Linux architecture. The probe rootfs is built from
a pinned Ubuntu 24.04 image instead of the host filesystem so an old driver-only
node cannot accidentally produce a probe image with glibc older than the
embedded Ascend runtime requires. Set `PROBE_GOARCH=amd64` or `arm64` when a
builder can execute the requested Docker platform through emulation.

`make runtime-assets` rebuilds both probe-image architectures, one Ascend
`libvruntime.so` for each of amd64/arm64, and the amd64-only PPU and AMD
shims. Ascend CANN 8/9 validation combinations belong to the support matrix,
not to duplicate runtime assets. `make runtime-assets-probe` is the faster
probe-only target; `make runtime-assets-verify` checks asset completeness and
checksums. Committed `.sha256` files are delivery checksums, not a substitute
for the agent's runtime probe.

Vendor libraries are rebuilt explicitly with `ascend/build.sh` (set `GOARCH`),
`ppu/build.sh` and `amd/build.sh`. All three scripts clone the pinned
upstream commit, apply only the checked-in patches (none for AMD), build in
the vendor SDK container, and print the resulting SHA-256 values for manifest
refresh. The Ascend script uses pinned multi-architecture CANN image digests
and mounts all three checked-in patches into the build container. This image
is only the compiler/toolchain environment; it does not label the resulting
SO as CANN 9 or make a separate CANN 8 ABI. The PPU and AMD SDK images are
also pinned by digest. Set `ASCEND_BUILD_IMAGE`, `PPU_BUILD_IMAGE` or
`AMD_BUILD_IMAGE` only when deliberately validating a replacement build image.
The historical ubs-virt pin is fetched from AtomGit's advertised
`refs/keep-around/<commit>` because the Gitee project migrated and no longer
serves that commit from its branch history.

`amd/build.sh` builds libvrocm.so (gpustack-operator's `csrc/amd/rocm-slicing-shim`,
the AMD counterpart of the PPU shim above) inside the same ROCm 7.2.3 base
image the gpuworks-runner ROCm/vLLM image uses, so its gcc and `/opt/rocm`
headers match a build this project already verifies elsewhere. The resulting
object links no ROCm object at all — only libc, up to GLIBC_2.4 — so one
build serves every ROCm version it is LD_PRELOAD'd into; see
`internal/accelerator/amd.go` and `internal/agent/engine/partition_amd.go`
for the container contract it is injected under. It enforces GPU memory only:
ROCm enforces compute (CU) quotas in hardware through `HSA_CU_MASK`, below
anything a preloaded library in the workload process can see, so this
backend never exposes a compute/SM limit.

The PPU HGML hook maps `GPUWORKS_PPU_DEVICE_UUIDS` to the selected compute
quota/ledger order. Inference engines receive numeric `CUDA_VISIBLE_DEVICES`;
probe identity and quota reporting use the separate verified UUID list.
An absent UUID variable preserves the older agent ABI via
`CUDA_VISIBLE_DEVICES`; an explicitly empty, invalid or duplicate UUID list
never falls back to another identity. The v4 behavioral probe independently
locates the requested UUID and checks its quota.
