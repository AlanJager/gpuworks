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
`libvruntime.so` for each of amd64/arm64, and the amd64-only PPU shim.
Ascend CANN 8/9 validation combinations belong to the support matrix, not to
duplicate runtime assets. `make runtime-assets-probe` is the faster probe-only
target; `make runtime-assets-verify` checks asset completeness and checksums.
Committed `.sha256` files are delivery checksums, not a substitute for the
agent's runtime probe.

Vendor libraries are rebuilt explicitly with `ascend/build.sh` (set `GOARCH`)
and `ppu/build.sh`. Both scripts clone the pinned
upstream commit, apply only the checked-in patches, build in the vendor SDK
container, and print the resulting SHA-256 values for manifest refresh.
The Ascend script uses pinned multi-architecture CANN image digests and mounts
all three checked-in patches into the build container. This image is only the
compiler/toolchain environment; it does not label the resulting SO as CANN 9
or make a separate CANN 8 ABI. The PPU SDK image is
also pinned by digest. Set `ASCEND_BUILD_IMAGE` or `PPU_BUILD_IMAGE` only when
deliberately validating a replacement build image.
The historical ubs-virt pin is fetched from AtomGit's advertised
`refs/keep-around/<commit>` because the Gitee project migrated and no longer
serves that commit from its branch history.

The PPU HGML hook maps full UUIDs in `CUDA_VISIBLE_DEVICES` to the same
compact quota/ledger ordinals that HGGC uses for compute. Unselected devices
retain their vendor memory figures; missing or invalid identity information
cannot acquire another device's quota. Numeric visibility retains the legacy
agent ABI. The v3 behavioral probe independently locates the requested UUID
and checks its quota, so a quota on another card cannot produce a pass.
