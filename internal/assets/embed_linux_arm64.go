//go:build linux && arm64

package assets

import "embed"

// This agent embeds only the linux/arm64 assets it can execute. libvgpu.so and
// the PPU hooks are amd64 objects, so they are not included here.
//
//go:embed bin/proot-arm64-static-nofd
//go:embed bin/udocker-offline-arm64-v1.1.2.tar.gz
//go:embed bin/manifest.sha256
//go:embed bin/runtime-assets/manifest.json
//go:embed bin/runtime-assets/probe/partition-probe-linux-arm64.tar.gz
//go:embed bin/runtime-assets/ascend/linux-arm64/libvruntime.so
var files embed.FS

//go:embed bin/ib_write_bw-arm64
var perftestFiles embed.FS
