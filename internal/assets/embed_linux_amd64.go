//go:build linux && amd64

package assets

import "embed"

// This agent embeds only the linux/amd64 assets it can execute. libvgpu.so is
// an x86-64 ELF, and the PPU hooks are built for linux/amd64.
//
//go:embed bin/proot-x86_64-static-nofd
//go:embed bin/udocker-offline-x86_64-v1.1.2.tar.gz
//go:embed bin/manifest.sha256
//go:embed bin/libvgpu.so
//go:embed bin/runtime-assets/manifest.json
//go:embed bin/runtime-assets/probe/partition-probe-linux-amd64.tar.gz
//go:embed bin/runtime-assets/ascend/linux-amd64/libvruntime.so
//go:embed bin/runtime-assets/ppu/linux-amd64/hggc-13.0/hggc_quota.so
//go:embed bin/runtime-assets/ppu/linux-amd64/hggc-13.0/hgml_dlsym_hook.so
//go:embed bin/runtime-assets/amd/linux-amd64/libvrocm.so
var files embed.FS

//go:embed bin/ib_write_bw-x86_64
var perftestFiles embed.FS
